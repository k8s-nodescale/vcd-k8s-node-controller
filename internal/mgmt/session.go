package mgmt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionCookie holds the signed sign-in token. It is HttpOnly and SameSite, so
// a page script cannot read it and a cross-site form cannot make the browser
// send it on a write it forged.
const sessionCookie = "nodescale-ui-session"

// DefaultSessionTTL is how long a sign-in lasts.
const DefaultSessionTTL = 12 * time.Hour

var errNoSession = errors.New("no usable session")

// sessionData is what a token carries. The fingerprint ties it to the password
// the account had when it signed in, so rotating a password ends the session.
type sessionData struct {
	Username    string
	Fingerprint string
}

// fingerprint is a short, non-reversible tag of a password hash.
func fingerprint(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func sessionMAC(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// issueSession builds payload.MAC, both base64url.
func issueSession(key []byte, username, passwordHash string, ttl time.Duration) string {
	payload := username + "\x00" + fingerprint(passwordHash) + "\x00" +
		strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return body + "." + base64.RawURLEncoding.EncodeToString(sessionMAC(key, body))
}

// readSession checks the MAC and the expiry. It says nothing about whether the
// account still exists: the caller re-reads the Secret for that.
func readSession(key []byte, token string) (sessionData, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return sessionData{}, errNoSession
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(sig, sessionMAC(key, parts[0])) {
		return sessionData{}, errNoSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionData{}, errNoSession
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) != 3 {
		return sessionData{}, errNoSession
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return sessionData{}, errNoSession
	}
	return sessionData{Username: fields[0], Fingerprint: fields[1]}, nil
}

// isTLS reports whether the browser reached us over HTTPS, directly or through
// the ingress. The cookie is only marked Secure when it did, otherwise a plain
// HTTP installation could never sign in.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie writes the token. Path is the page prefix so the cookie is
// not sent anywhere else on the cluster.
func setSessionCookie(w http.ResponseWriter, r *http.Request, prefix, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isTLS(r),
		MaxAge:   int(ttl / time.Second),
	})
}

// clearSessionCookie signs the browser out.
func clearSessionCookie(w http.ResponseWriter, prefix string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// safeNext keeps the post-login redirect inside the page: an absolute URL or a
// path outside the prefix falls back to the index.
func safeNext(prefix, next string) string {
	home := prefix + "/"
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return home
	}
	if prefix != "" && !strings.HasPrefix(next, prefix) {
		return home
	}
	if strings.HasPrefix(next, prefix+"/login") || strings.HasPrefix(next, prefix+"/api/") {
		return home
	}
	return next
}
