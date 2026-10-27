package mgmt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// cheapHash keeps the suite fast: the cost factor is the only slow part of
// bcrypt and it does not change the answer.
func cheapHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func loadedAuth(t *testing.T, users *Users) *Authenticator {
	t.Helper()
	a := &Authenticator{}
	a.users = users
	a.at = time.Now()
	return a
}

func testUsers(t *testing.T) *Users {
	t.Helper()
	return &Users{
		Admins:  []Account{{Username: "admin", PasswordHash: cheapHash(t, "admin-pw")}},
		Viewers: []Account{{Username: "viewer", PasswordHash: cheapHash(t, "viewer-pw")}},
	}
}

func TestParseUsersRejectsAPayloadNobodyCouldUse(t *testing.T) {
	good := cheapHash(t, "x")
	cases := map[string]string{
		"no admins":     "viewers:\n  - username: v\n    passwordHash: " + good + "\n",
		"not bcrypt":    "admins:\n  - username: a\n    passwordHash: plain\n",
		"no username":   "admins:\n  - passwordHash: " + good + "\n",
		"no hash":       "admins:\n  - username: a\n",
		"unknown field": "admins:\n  - username: a\n    password: x\n",
		"not a mapping": "- just\n- a list\n",
	}
	for name, payload := range cases {
		if _, err := ParseUsers([]byte(payload)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseUsers([]byte("admins:\n  - username: a\n    passwordHash: " + good + "\n")); err != nil {
		t.Fatalf("a valid payload was rejected: %v", err)
	}
}

func TestLookupReturnsTheGroup(t *testing.T) {
	a := loadedAuth(t, testUsers(t))
	cases := []struct {
		user, pass string
		role       Role
		ok         bool
	}{
		{"admin", "admin-pw", RoleAdmin, true},
		{"viewer", "viewer-pw", RoleViewer, true},
		{"viewer", "admin-pw", "", false},
		{"admin", "", "", false},
		{"nobody", "admin-pw", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		role, ok := a.Lookup(c.user, c.pass)
		if ok != c.ok || role != c.role {
			t.Errorf("%q/%q: %q %v, want %q %v", c.user, c.pass, role, ok, c.role, c.ok)
		}
	}
}

// A name in both groups is an admin: the operator put it there on purpose.
func TestLookupPrefersAdminWhenListedTwice(t *testing.T) {
	u := testUsers(t)
	u.Viewers = append(u.Viewers, u.Admins[0])
	a := loadedAuth(t, u)
	if role, ok := a.Lookup("admin", "admin-pw"); !ok || role != RoleAdmin {
		t.Fatalf("%q %v, want admin", role, ok)
	}
}

// Nothing is served until an account list is loaded, and deleting the Secret
// closes the page instead of leaving the old passwords valid.
func TestLoadReadsTheSecretAndFailsClosed(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nodescale-ui-auth", Namespace: "default"},
		Data: map[string][]byte{"users.yaml": []byte(
			"admins:\n  - username: a\n    passwordHash: " + cheapHash(t, "pw") + "\n")},
	}
	fc := testClient(t, secret)
	a := &Authenticator{Client: fc, SecretName: "nodescale-ui-auth", Namespace: "default"}

	if a.Ready() {
		t.Fatal("ready before the first load")
	}
	if err := a.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.Ready() {
		t.Fatal("not ready after a good load")
	}

	if err := fc.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := a.Load(context.Background()); err == nil {
		t.Fatal("a missing Secret was accepted")
	}
	if a.Ready() {
		t.Fatal("still ready after the Secret was deleted")
	}
}

func TestLoadRejectsAMissingKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nodescale-ui-auth", Namespace: "default"},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	a := &Authenticator{Client: testClient(t, secret), SecretName: "nodescale-ui-auth"}
	if err := a.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "users.yaml") {
		t.Fatalf("err %v, want a missing-key error", err)
	}
}

func testAuthServer(t *testing.T) *Server {
	t.Helper()
	pool := testPool(1)
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name:     "KION-WEB-DEV-wrk-01",
		NodeName: "kion-web-dev-wrk-01",
		Phase:    infrav1.NodePhaseReady,
	}}
	fc := testClient(t, pool, testNode("kion-web-dev-wrk-01", 2000, 4096))
	col := &Collector{Client: fc}
	if _, err := col.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Server{Client: fc, Collector: col, Auth: loadedAuth(t, testUsers(t)), PathPrefix: "/nodescale-ui"}
}

func do(h http.Handler, method, path, user, pass string, withCSRF bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"replicas":2}`))
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	if withCSRF {
		req.Header.Set(csrfHeader, csrfValue)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The point of the groups: a viewer sees everything and changes nothing, and
// the server is what enforces it.
func TestViewerReadsButCannotAct(t *testing.T) {
	s := testAuthServer(t)
	h := s.Handler()

	if rec := do(h, http.MethodGet, "/nodescale-ui/api/pools", "viewer", "viewer-pw", false); rec.Code != http.StatusOK {
		t.Fatalf("viewer GET: %d %s", rec.Code, rec.Body.String())
	}
	rec := do(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", "viewer", "viewer-pw", true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "viewer") {
		t.Errorf("refusal does not name the account: %s", rec.Body.String())
	}

	// An admin without the CSRF header is refused too: the header is about the
	// request's shape, not the account.
	if rec := do(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", "admin", "admin-pw", false); rec.Code != http.StatusForbidden {
		t.Fatalf("admin POST without the header: %d, want 403", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", "admin", "admin-pw", true); rec.Code != http.StatusOK {
		t.Fatalf("admin POST: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNoCredentialsIsUnauthorized(t *testing.T) {
	h := testAuthServer(t).Handler()
	rec := do(h, http.MethodGet, "/nodescale-ui/api/pools", "", "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("challenge %q", rec.Header().Get("WWW-Authenticate"))
	}
	if rec := do(h, http.MethodGet, "/nodescale-ui/api/pools", "admin", "wrong", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", rec.Code)
	}
}

// The probe cannot authenticate, so it must not need to.
func TestHealthzIsOpen(t *testing.T) {
	h := testAuthServer(t).Handler()
	if rec := do(h, http.MethodGet, "/nodescale-ui/healthz", "", "", false); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

// A Secret that is missing or unreadable closes the page; it never opens it.
func TestUnloadedAuthenticatorRefusesEverything(t *testing.T) {
	s := testAuthServer(t)
	s.Auth = &Authenticator{}
	h := s.Handler()
	rec := do(h, http.MethodGet, "/nodescale-ui/api/pools", "admin", "admin-pw", false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d, want 503", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/nodescale-ui/healthz", "", "", false); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d, want 200", rec.Code)
	}
}

func TestMeReportsTheRole(t *testing.T) {
	h := testAuthServer(t).Handler()
	rec := do(h, http.MethodGet, "/nodescale-ui/api/me", "viewer", "viewer-pw", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":"viewer"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/nodescale-ui/api/me", "admin", "admin-pw", false)
	if !strings.Contains(rec.Body.String(), `"role":"admin"`) {
		t.Fatalf("%s", rec.Body.String())
	}
}

// Who changed the pool survives in kubectl describe, not only in the log.
func TestActionIsAnnotatedWithTheActor(t *testing.T) {
	s := testAuthServer(t)
	h := s.Handler()
	if rec := do(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", "admin", "admin-pw", true); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := &infrav1.K8sWorkerPool{}
	if err := s.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got); err != nil {
		t.Fatal(err)
	}
	note := got.Annotations[lastActionAnnotation]
	if !strings.Contains(note, "admin") || !strings.Contains(note, "replicas=2") {
		t.Fatalf("annotation %q", note)
	}
}
