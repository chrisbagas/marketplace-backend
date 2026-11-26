package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An uploaded SVG with a script must be served with headers that stop the
// script from running on the app origin. No database needed: the uploads
// route never touches the pool.
func TestUploadsServedWithLockdownHeaders(t *testing.T) {
	dir := t.TempDir()
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`
	if err := os.WriteFile(filepath.Join(dir, "x.svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, dir, AuthConfig{})

	for _, path := range []string{"/uploads/x.svg", "/uploads/missing.png"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got, want := rec.Header().Get("Content-Security-Policy"), "default-src 'none'; style-src 'unsafe-inline'; sandbox"; got != want {
			t.Errorf("%s: Content-Security-Policy = %q, want %q", path, got, want)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/uploads/x.svg", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<script>") {
		t.Fatalf("svg not served: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
}
