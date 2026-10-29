package mgmt

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

	// SessionTTL is how long the login cookie lasts.
	SessionTTL time.Duration

	// Disabled turns authentication off. It exists for a laptop run; the chart
	// sets it only when ui.auth.enabled is false.
	Disabled bool

	mu    sync.RWMutex
	users *Users
	raw   []byte
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
	var raw []byte
	if err == nil {
		var ok bool
		raw, ok = secret.Data[a.secretKey()]
		if !ok {
			err = fmt.Errorf("secret %s/%s has no key %q", a.namespace(), a.SecretName, a.secretKey())
		} else {
			users, err = ParseUsers(raw)
		}
	}

	var payload []byte
	if users != nil {
		payload = raw
	}

	a.mu.Lock()
	a.users, a.raw, a.at, a.err = users, payload, time.Now(), err
	a.mu.Unlock()
	if err != nil {
		logf("auth: %v", err)
	}
	return err
}

// TTL is the login cookie lifetime, with the default filled in.
func (a *Authenticator) TTL() time.Duration {
	if a.SessionTTL > 0 {
		return a.SessionTTL
	}
	return DefaultSessionTTL
}

// SessionKey signs the login cookie. It is derived from the account file, so a
// session survives a restart but ends the moment any account changes - which is
// exactly when a signed-in browser should be asked again.
func (a *Authenticator) SessionKey() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.raw) == 0 {
		return nil
	}
	h := sha256.New()
	h.Write([]byte("nodescale-ui-session-v1\n"))
	h.Write(a.raw)
	return h.Sum(nil)
}

// account returns the role and the password hash currently on file for a name.
// The session path uses it to notice a removed account or a rotated password.
func (a *Authenticator) account(username string) (Role, string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.users == nil || username == "" {
		return "", "", false
	}
	for _, acc := range a.users.Admins {
		if subtle.ConstantTimeCompare([]byte(acc.Username), []byte(username)) == 1 {
			return RoleAdmin, acc.PasswordHash, true
		}
	}
	for _, acc := range a.users.Viewers {
		if subtle.ConstantTimeCompare([]byte(acc.Username), []byte(username)) == 1 {
			return RoleViewer, acc.PasswordHash, true
		}
	}
	return "", "", false
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

// withAuth wraps the mux. The health probe and the login form stay open;
// everything else needs a signed-in browser, and a write needs the CSRF header
// and the admin role. HTTP Basic is still accepted, so curl and a script do not
// have to hold a cookie.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.prefix()
		if s.isOpen(r, p) {
			next.ServeHTTP(w, r)
			return
		}
		if s.Auth == nil || s.Auth.Disabled {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{},
				Actor{Username: "", Role: RoleAdmin})))
			return
		}
		if !s.Auth.Ready() {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Errorf("authentication is not configured: %v", s.Auth.Err()))
			return
		}

		actor, ok := Actor{}, false
		if name, password, hasBasic := r.BasicAuth(); hasBasic {
			var role Role
			if role, ok = s.Auth.Lookup(name, password); ok {
				actor = Actor{Username: name, Role: role}
			}
		} else {
			actor, ok = s.actorFromSession(r)
		}
		if !ok {
			s.refuse(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, fmt.Errorf("missing %s: %s header", csrfHeader, csrfValue))
				return
			}
			if actor.Role != RoleAdmin {
				writeError(w, http.StatusForbidden,
					fmt.Errorf("%q is a %s and may not change the pool", actor.Username, actor.Role))
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
	})
}

// isOpen reports whether a path is served without credentials: the probe cannot
// authenticate, and the form is how credentials arrive in the first place.
func (s *Server) isOpen(r *http.Request, prefix string) bool {
	switch r.URL.Path {
	case prefix + "/healthz", prefix + "/login", prefix + "/api/login":
		return true
	}
	return false
}

// actorFromSession validates the cookie and then re-reads the account list, so
// a removed user or a rotated password ends the session immediately.
func (s *Server) actorFromSession(r *http.Request) (Actor, bool) {
	key := s.Auth.SessionKey()
	if len(key) == 0 {
		return Actor{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Actor{}, false
	}
	data, err := readSession(key, c.Value)
	if err != nil {
		return Actor{}, false
	}
	role, hash, ok := s.Auth.account(data.Username)
	if !ok || fingerprint(hash) != data.Fingerprint {
		return Actor{}, false
	}
	return Actor{Username: data.Username, Role: role}, true
}

// refuse answers a request with no usable credentials: the page is sent to the
// form, the API gets the 401 it reports.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		unauthorized(w)
		return
	}
	target := s.prefix() + "/login?next=" + url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, target, http.StatusFound)
}

// wantsJSON separates the page from the API behind it.
func wantsJSON(r *http.Request) bool {
	if strings.Contains(r.URL.Path, "/api/") {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="nodescale-ui", charset="UTF-8"`)
	writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
}
