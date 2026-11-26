package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"/profile":             "/profile",
		"/designer?tab=produk": "/designer?tab=produk",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"javascript:alert(1)":  "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidEmail(t *testing.T) {
	for _, ok := range []string{"a@b.co", "raka.wijaya@karyakita.id"} {
		if !validEmail(ok) {
			t.Errorf("validEmail(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "raka", "raka@", "Raka <raka@x.id>", "a b@c.id"} {
		if validEmail(bad) {
			t.Errorf("validEmail(%q) = true, want false", bad)
		}
	}
}

func TestUsernameFromEmail(t *testing.T) {
	cases := map[string]string{
		"Raka.Wijaya@gmail.com": "raka.wijaya",
		"a+b@gmail.com":         "userab",
		".x.@gmail.com":         "userx",
		"averyveryveryveryverylongname123@gmail.com": "averyveryveryveryverylongname1",
	}
	for in, want := range cases {
		got := usernameFromEmail(in)
		if got != want {
			t.Errorf("usernameFromEmail(%q) = %q, want %q", in, got, want)
		}
		if !usernameRe.MatchString(got) {
			t.Errorf("usernameFromEmail(%q) = %q does not pass usernameRe", in, got)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter()
	for i := 0; i < 3; i++ {
		if !l.allow("k", 3, time.Minute) {
			t.Fatalf("hit %d blocked, want allowed", i+1)
		}
	}
	if l.allow("k", 3, time.Minute) {
		t.Fatal("4th hit allowed, want blocked")
	}
	if !l.allow("other", 3, time.Minute) {
		t.Fatal("separate key blocked")
	}
	if !l.allow("short", 1, time.Millisecond) {
		t.Fatal("first hit blocked")
	}
	time.Sleep(5 * time.Millisecond)
	if !l.allow("short", 1, time.Millisecond) {
		t.Fatal("hit after window blocked, want allowed")
	}
}

func TestHasRole(t *testing.T) {
	u := &User{Role: "designer"}
	if !hasRole(u, "designer", "admin") || hasRole(u, "admin") {
		t.Fatal("hasRole mismatch")
	}
}

func TestHashTokenStable(t *testing.T) {
	tok := randomToken()
	if hashToken(tok) != hashToken(tok) || hashToken(tok) == tok || len(hashToken(tok)) != 64 {
		t.Fatal("hashToken must be a stable 64-char sha256 hex digest")
	}
	if randomToken() == tok {
		t.Fatal("randomToken repeated")
	}
}

func TestPickClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		hops   int
		want   string
	}{
		{"hops 0 ignores xff", "192.0.2.1:1234", "6.6.6.6", 0, "192.0.2.1"},
		{"hops 0 no xff", "192.0.2.1:1234", "", 0, "192.0.2.1"},
		{"hops 1 takes rightmost", "10.0.0.1:1", "6.6.6.6, 203.0.113.9", 1, "203.0.113.9"},
		{"hops 2 skips own proxy entry", "10.0.0.1:1", "6.6.6.6, 203.0.113.9, 10.0.0.2", 2, "203.0.113.9"},
		{"hops 2 ignores spoofed prefix", "10.0.0.1:1", "1.1.1.1, 2.2.2.2, 203.0.113.9, 10.0.0.2", 2, "203.0.113.9"},
		{"fewer entries than hops uses leftmost", "10.0.0.1:1", "203.0.113.9", 2, "203.0.113.9"},
		{"hops without xff falls back to peer", "10.0.0.1:1", "", 2, "10.0.0.1"},
		{"remote without port", "192.0.2.1", "", 0, "192.0.2.1"},
	}
	for _, c := range cases {
		if got := pickClientIP(c.remote, c.xff, c.hops); got != c.want {
			t.Errorf("%s: pickClientIP(%q, %q, %d) = %q, want %q", c.name, c.remote, c.xff, c.hops, got, c.want)
		}
	}
}

// With hops = 0 a client cannot dodge the per-IP login limit by rotating
// X-Forwarded-For. Needs TEST_DATABASE_URL (the limit check runs after body
// parsing but before any query; the pool is still required by NewServer users).
func TestLoginLimitIgnoresSpoofedXFF(t *testing.T) {
	pool := testPool(t)
	s := NewServer(pool, os.TempDir(), AuthConfig{TrustedProxyHops: 0})
	var lastCode int
	for i := 1; i <= 31; i++ {
		body, _ := json.Marshal(map[string]string{"identifier": fmt.Sprintf("nobody-xff-%d", i), "password": "salahsalah"})
		req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		req.RemoteAddr = "192.0.2.77:4321"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		lastCode = rec.Code
		if i <= 30 && rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d already limited", i)
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("31st attempt: %d, want 429", lastCode)
	}
}
