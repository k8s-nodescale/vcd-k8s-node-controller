package mgmt

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sessionServer is the auth server with the account payload the cookie key is
// derived from. The suite sets users directly, so the payload is set here too.
func sessionServer(t *testing.T) *Server {
	t.Helper()
	s := testAuthServer(t)
	s.Auth.raw = []byte("accounts used by the session tests")
	return s
}

func form(path string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func signIn(t *testing.T, h http.Handler, user, password string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, form("/nodescale-ui/api/login", url.Values{
		"username": {user}, "password": {password},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login %s: %d %s", user, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("login %s set no %s cookie", user, sessionCookie)
	return nil
}

func callWithCookie(h http.Handler, method, path string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{\"replicas\":2}"))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf {
		req.Header.Set(csrfHeader, csrfValue)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A browser with no session is sent to the form, not to a 401 body.
func TestPageWithoutSessionRedirectsToTheForm(t *testing.T) {
	h := sessionServer(t).Handler()
	rec := callWithCookie(h, http.MethodGet, "/nodescale-ui/", nil, false)
	if rec.Code != http.StatusFound {
		t.Fatalf("%d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/nodescale-ui/login?next=") {
		t.Fatalf("redirect %q", got)
	}
	rec = callWithCookie(h, http.MethodGet, "/nodescale-ui/login", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("form: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "action=\"/nodescale-ui/api/login\"") || !strings.Contains(body, "type=\"password\"") {
		t.Fatalf("not a login form: %s", body[:200])
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("the form must not ask for Basic")
	}
}

// The cookie a sign-in sets is the whole session: the API accepts it, a write
// still needs the CSRF header and the admin role, and signing out clears it.
func TestSignInSetsASessionAndSignOutClearsIt(t *testing.T) {
	h := sessionServer(t).Handler()
	cookie := signIn(t, h, "admin", "admin-pw")
	if !cookie.HttpOnly {
		t.Errorf("session cookie is readable by page scripts")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v", cookie.SameSite)
	}

	if rec := callWithCookie(h, http.MethodGet, "/nodescale-ui/api/pools", cookie, false); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callWithCookie(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", cookie, false); rec.Code != http.StatusForbidden {
		t.Fatalf("write without the CSRF header: %d, want 403", rec.Code)
	}
	if rec := callWithCookie(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", cookie, true); rec.Code != http.StatusOK {
		t.Fatalf("admin write with the header: %d %s", rec.Code, rec.Body.String())
	}

	rec := callWithCookie(h, http.MethodPost, "/nodescale-ui/api/logout", cookie, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear %s", sessionCookie)
	}
}

func TestWrongPasswordKeepsTheForm(t *testing.T) {
	h := sessionServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, form("/nodescale-ui/api/login", url.Values{
		"username": {"admin"}, "password": {"nope"},
	}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("a failed sign-in set a cookie")
	}
	if !strings.Contains(rec.Body.String(), "Wrong user name or password.") {
		t.Errorf("no error on the form: %s", rec.Body.String()[:200])
	}
}

// A viewer who signs in through the form is still read-only.
func TestViewerSessionIsReadOnly(t *testing.T) {
	h := sessionServer(t).Handler()
	cookie := signIn(t, h, "viewer", "viewer-pw")
	if rec := callWithCookie(h, http.MethodGet, "/nodescale-ui/api/me", cookie, false); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "\"role\":\"viewer\"") {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callWithCookie(h, http.MethodPost, "/nodescale-ui/api/pools/default/kion-web-dev/replicas", cookie, true); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer write: %d, want 403", rec.Code)
	}
}

// Rotating a password or removing the account ends the sessions that used it,
// without waiting for the cookie to expire.
func TestSessionDiesWhenTheAccountChanges(t *testing.T) {
	s := sessionServer(t)
	h := s.Handler()
	cookie := signIn(t, h, "admin", "admin-pw")

	s.Auth.users = &Users{Admins: []Account{{Username: "admin", PasswordHash: cheapHash(t, "rotated-pw")}}}
	rec := callWithCookie(h, http.MethodGet, "/nodescale-ui/api/pools", cookie, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after a rotation: %d, want 401", rec.Code)
	}

	s.Auth.users = &Users{Admins: []Account{{Username: "someone-else", PasswordHash: cheapHash(t, "x")}}}
	if rec := callWithCookie(h, http.MethodGet, "/nodescale-ui/api/pools", cookie, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after the account was removed: %d, want 401", rec.Code)
	}
}

func TestTokenRejectsTamperingAndExpiry(t *testing.T) {
	key := []byte("k")
	hash := cheapHash(t, "pw")
	token := issueSession(key, "admin", hash, time.Hour)
	if data, err := readSession(key, token); err != nil || data.Username != "admin" {
		t.Fatalf("fresh token: %v %v", data, err)
	}
	if _, err := readSession([]byte("other"), token); err == nil {
		t.Errorf("a token signed with another key was accepted")
	}
	if _, err := readSession(key, token[:len(token)-2]+"AA"); err == nil {
		t.Errorf("a tampered token was accepted")
	}
	if _, err := readSession(key, issueSession(key, "admin", hash, -time.Minute)); err == nil {
		t.Errorf("an expired token was accepted")
	}
}

// The form's next field cannot send the browser off the page.
func TestSafeNextStaysInsideThePrefix(t *testing.T) {
	cases := map[string]string{
		"":                           "/nodescale-ui/",
		"https://elsewhere.example/": "/nodescale-ui/",
		"//elsewhere.example/x":      "/nodescale-ui/",
		"/other":                     "/nodescale-ui/",
		"/nodescale-ui/login":        "/nodescale-ui/",
		// An API path is not a page to land on after signing in.
		"/nodescale-ui/api/pools": "/nodescale-ui/",
	}
	for in, want := range cases {
		if got := safeNext("/nodescale-ui", in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
