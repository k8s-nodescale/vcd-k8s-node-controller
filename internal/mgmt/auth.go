package mgmt

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Role is what an account may do. A viewer reads the page; an admin also edits
// the pool.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Account is one entry of users.yaml.
type Account struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// Users is the Secret payload: the two groups the operator asked for. A name
// listed in both groups is an admin, because a stricter reading would silently
// demote someone who was meant to be one.
type Users struct {
	Admins  []Account `json:"admins"`
	Viewers []Account `json:"viewers"`
}

// ParseUsers validates the payload. A missing admin group is an error rather
// than a read-only page: nobody could ever act, which is a misconfiguration and
// must be visible in the log instead of looking like a policy.
func ParseUsers(raw []byte) (*Users, error) {
	u := &Users{}
	if err := yaml.UnmarshalStrict(raw, u); err != nil {
		return nil, fmt.Errorf("users.yaml: %w", err)
	}
	for _, a := range u.Admins {
		if err := a.validate(); err != nil {
			return nil, err
		}
	}
	for _, a := range u.Viewers {
		if err := a.validate(); err != nil {
			return nil, err
		}
	}
	if len(u.Admins) == 0 {
		return nil, errors.New("users.yaml: no admins")
	}
	return u, nil
}

func (a Account) validate() error {
	if strings.TrimSpace(a.Username) == "" {
		return errors.New("users.yaml: an account has no username")
	}
	if a.PasswordHash == "" {
		return fmt.Errorf("users.yaml: %s has no passwordHash", a.Username)
	}
	if !strings.HasPrefix(a.PasswordHash, "$2") {
		return fmt.Errorf("users.yaml: %s: passwordHash is not a bcrypt hash", a.Username)
	}
	return nil
}

// dummyHash is compared against when the name is unknown, so a wrong name and a
// wrong password cost the same time.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("nodescale-ui-unknown-account"), bcrypt.DefaultCost)
	if err != nil {
		return []byte("$2a$12$0000000000000000000000000000000000000000000000000000")
	}
	return h
}()

// Authenticator holds the accounts read from the Secret and answers one
// question: given a name and a password, which role, if any.
type Authenticator struct {
	Client client.Client

	// Namespace, SecretName and SecretKey locate users.yaml.
	Namespace  string
	SecretName string
	SecretKey  string

	// Refresh is how often the Secret is re-read, so a rotated password works
	// without a restart.
	Refresh time.Duration

	// Disabled turns authentication off. It exists for a laptop run; the chart
	// sets it only when ui.auth.enabled is false.
	Disabled bool

	mu    sync.RWMutex
	users *Users
	at    time.Time
	err   error
}

// Ready reports whether a usable account list is loaded. Until it is, every
// request is refused: a page that can delete workers never opens by accident.
func (a *Authenticator) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.users != nil
}

// Err is why the last load failed.
func (a *Authenticator) Err() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.err
}

// LoadedAt is when the current account list was read.
func (a *Authenticator) LoadedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.at
}

func (a *Authenticator) namespace() string {
	if a.Namespace != "" {
		return a.Namespace
	}
	return "default"
}

func (a *Authenticator) secretKey() string {
	if a.SecretKey != "" {
		return a.SecretKey
	}
	return "users.yaml"
}

// Load reads and parses the Secret. On any failure the previous account list is
// dropped, so a Secret that was deleted mid-flight closes the page instead of
// leaving the old passwords valid.
func (a *Authenticator) Load(ctx context.Context) error {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: a.namespace(), Name: a.SecretName}
	err := a.Client.Get(ctx, key, secret)
	var users *Users
	if err == nil {
		raw, ok := secret.Data[a.secretKey()]
		if !ok {
			err = fmt.Errorf("secret %s/%s has no key %q", a.namespace(), a.SecretName, a.secretKey())
		} else {
			users, err = ParseUsers(raw)
		}
	}

	a.mu.Lock()
	a.users, a.at, a.err = users, time.Now(), err
	a.mu.Unlock()
	if err != nil {
		logf("auth: %v", err)
	}
	return err
}

// Run reloads the Secret until ctx is done.
func (a *Authenticator) Run(ctx context.Context) {
	period := a.Refresh
	if period <= 0 {
		period = time.Minute
	}
	_ = a.Load(ctx)
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = a.Load(ctx)
		}
	}
}

// Lookup checks one pair. Every name in both groups is compared, and an unknown
// name is compared against a dummy hash, so neither a wrong name nor a wrong
// password answers faster than the other.
func (a *Authenticator) Lookup(username, password string) (Role, bool) {
	a.mu.RLock()
	users := a.users
	a.mu.RUnlock()
	if users == nil || username == "" {
		return "", false
	}

	var role Role
	var hash string
	for _, acc := range users.Admins {
		if subtle.ConstantTimeCompare([]byte(acc.Username), []byte(username)) == 1 {
			role, hash = RoleAdmin, acc.PasswordHash
		}
	}
	for _, acc := range users.Viewers {
		if role == "" && subtle.ConstantTimeCompare([]byte(acc.Username), []byte(username)) == 1 {
			role, hash = RoleViewer, acc.PasswordHash
		}
	}
	if hash == "" {
		hash = string(dummyHash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", false
	}
	if role == "" {
		return "", false
	}
	return role, true
}

// actorKey carries the authenticated account on the request context.
type actorKey struct{}

// Actor is who made a request.
type Actor struct {
	Username string
	Role     Role
}

func actorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// csrfHeader is required on every write. A cross-site form cannot set a custom
// header, while a browser happily replays cached Basic credentials for one.
const (
	csrfHeader = "X-Requested-With"
	csrfValue  = "nodescale-ui"
)

// withAuth wraps the mux. The health probe stays open; everything else needs
// credentials, a write needs the CSRF header and the admin role.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Auth == nil || s.Auth.Disabled {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{},
				Actor{Username: "", Role: RoleAdmin})))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.Auth.Ready() {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Errorf("authentication is not configured: %v", s.Auth.Err()))
			return
		}
		name, password, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		role, ok := s.Auth.Lookup(name, password)
		if !ok {
			logf("auth: rejected a sign-in as %q", name)
			unauthorized(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, fmt.Errorf("missing %s: %s header", csrfHeader, csrfValue))
				return
			}
			if role != RoleAdmin {
				writeError(w, http.StatusForbidden, fmt.Errorf("%q is a %s and may not change the pool", name, role))
				return
			}
		}
		ctx := context.WithValue(r.Context(), actorKey{}, Actor{Username: name, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="nodescale-ui", charset="UTF-8"`)
	writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
}
