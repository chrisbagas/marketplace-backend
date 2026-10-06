package api

import (
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
