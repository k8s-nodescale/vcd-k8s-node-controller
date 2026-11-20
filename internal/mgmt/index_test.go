package mgmt

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The page prints the release the chart passed in, and never the placeholder.
// It is the only version information an operator has from the page itself, so it
// must not be able to silently render as "dev" in a release install.
func TestIndexCarriesTheRelease(t *testing.T) {
	s := &Server{Release: "9.9.9", PathPrefix: "/nodescale-ui"}
	rec := httptest.NewRecorder()
	s.index(rec, httptest.NewRequest("GET", "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, ">9.9.9<") {
		t.Fatalf("the release is not in the header: %s", head(body, 400))
	}
	if strings.Contains(body, "__VERSION__") {
		t.Fatal("the version placeholder was served verbatim")
	}
	if !strings.Contains(body, `"/nodescale-ui"`) {
		t.Fatalf("the path prefix was not substituted: %s", head(body, 400))
	}
}

// An unset release is "dev", not an empty span.
func TestIndexDefaultsToDev(t *testing.T) {
	s := &Server{PathPrefix: "/nodescale-ui"}
	rec := httptest.NewRecorder()
	s.index(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), ">dev<") {
		t.Fatal("an unset release must render as dev")
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
