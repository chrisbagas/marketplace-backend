package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tes integrasi alur login Google dengan server Google palsu.
// Butuh Postgres yang sudah termigrasi: set TEST_DATABASE_URL, mis.
//
//	TEST_DATABASE_URL=postgres://karyakita:karyakita_dev@localhost:5432/karyakita go test ./...
func testPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL tidak di-set — tes integrasi dilewati")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("database tidak terjangkau: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeGoogle melayani endpoint token + userinfo dan mengembalikan profil yang diberikan.
func fakeGoogle(t *testing.T, info googleInfo) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "kode-ok" || r.FormValue("client_secret") != "rahasia" {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "akses-ok"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer akses-ok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(info)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldToken, oldInfo := googleTokenURL, googleUserinfoURL
	googleTokenURL, googleUserinfoURL = srv.URL+"/token", srv.URL+"/userinfo"
	t.Cleanup(func() { googleTokenURL, googleUserinfoURL = oldToken, oldInfo })
}

func googleServer(pool *pgxpool.Pool) *Server {
	return NewServer(pool, os.TempDir(), AuthConfig{Google: GoogleConfig{
		ClientID: "klien", ClientSecret: "rahasia", RedirectURL: "http://localhost:3000/api/auth/google/callback",
	}})
}

// googleLogin menjalankan start → callback dan mengembalikan respons callback.
func googleLogin(t *testing.T, s *Server, next string) *httptest.ResponseRecorder {
	t.Helper()
	start := httptest.NewRecorder()
	s.ServeHTTP(start, httptest.NewRequest("GET", "/api/auth/google/start?next="+url.QueryEscape(next), nil))
	if start.Code != http.StatusFound {
		t.Fatalf("start: status %d, want 302", start.Code)
	}
	loc, _ := url.Parse(start.Header().Get("Location"))
	if loc.Query().Get("client_id") != "klien" || loc.Query().Get("scope") != "openid email profile" {
		t.Fatalf("start: redirect ke Google tidak lengkap: %s", loc)
	}
	state := loc.Query().Get("state")

	req := httptest.NewRequest("GET", "/api/auth/google/callback?code=kode-ok&state="+url.QueryEscape(state), nil)
	for _, c := range start.Result().Cookies() {
		req.AddCookie(c)
	}
	cb := httptest.NewRecorder()
	s.ServeHTTP(cb, req)
	return cb
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestGoogleLoginCreatesThenReusesAccount(t *testing.T) {
	pool := testPool(t)
	sub := "tes-sub-" + randomHex(4)
	email := "google." + randomHex(3) + "@gmail.com"
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM users WHERE google_sub = $1`, sub) })
	fakeGoogle(t, googleInfo{Sub: sub, Email: email, EmailVerified: true, Name: "Pengguna Google"})
	s := googleServer(pool)

	cb := googleLogin(t, s, "/profile")
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != "/profile" {
		t.Fatalf("callback: %d → %q, want 302 → /profile", cb.Code, cb.Header().Get("Location"))
	}
	c := sessionCookieFrom(cb)
	if c == nil || !c.HttpOnly {
		t.Fatal("callback tidak memasang cookie sesi HttpOnly")
	}

	// sesi baru dikenali oleh /api/auth/me
	me := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.AddCookie(c)
	s.ServeHTTP(me, req)
	var body struct{ User *User }
	json.NewDecoder(me.Body).Decode(&body)
	if body.User == nil || body.User.Email != email || !body.User.GoogleLinked || body.User.HasPassword || body.User.Role != "customer" {
		t.Fatalf("me: user tidak sesuai: %+v", body.User)
	}
	firstID := body.User.ID

	// login kedua dengan sub yang sama → akun yang sama, bukan akun baru
	googleLogin(t, s, "/")
	var count int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE google_sub = $1`, sub).Scan(&count)
	if count != 1 {
		t.Fatalf("akun dengan google_sub ini: %d, want 1", count)
	}
	var id string
	pool.QueryRow(context.Background(), `SELECT id FROM users WHERE google_sub = $1`, sub).Scan(&id)
	if id != firstID {
		t.Fatalf("login kedua memakai akun %s, want %s", id, firstID)
	}
}

func TestGoogleLoginLinksExistingEmail(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	userID, email := "u-tes"+randomHex(3), "tautkan."+randomHex(3)+"@gmail.com"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, email, name, password_hash) VALUES ($1,$2,$3,'Tes','x')`,
		userID, strings.TrimPrefix(userID, "u-"), email); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) })
	fakeGoogle(t, googleInfo{Sub: "tes-sub-" + randomHex(4), Email: strings.ToUpper(email), EmailVerified: true})

	if cb := googleLogin(t, googleServer(pool), "/"); sessionCookieFrom(cb) == nil {
		t.Fatalf("callback gagal: %d → %s", cb.Code, cb.Header().Get("Location"))
	}
	var linked bool
	pool.QueryRow(ctx, `SELECT google_sub IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&linked)
	if !linked {
		t.Fatal("akun dengan email yang sama tidak ditautkan ke Google")
	}
}

func TestGoogleCallbackRejectsBadStateAndUnverifiedEmail(t *testing.T) {
	pool := testPool(t)
	s := googleServer(pool)

	// state tidak cocok dengan cookie
	req := httptest.NewRequest("GET", "/api/auth/google/callback?code=kode-ok&state=palsu", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "asli|%2F"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login?error=google-state") {
		t.Fatalf("state palsu: redirect %q, want /login?error=google-state", loc)
	}

	// email belum diverifikasi Google → ditolak, tidak ada akun dibuat
	sub := "tes-sub-" + randomHex(4)
	fakeGoogle(t, googleInfo{Sub: sub, Email: "belum." + randomHex(3) + "@gmail.com", EmailVerified: false})
	cb := googleLogin(t, s, "/")
	if !strings.Contains(cb.Header().Get("Location"), "email-belum-terverifikasi") || sessionCookieFrom(cb) != nil {
		t.Fatalf("email tak terverifikasi diterima: %s", cb.Header().Get("Location"))
	}

	// next eksternal dinetralkan jadi "/"
	fakeGoogle(t, googleInfo{Sub: sub, Email: "aman." + randomHex(3) + "@gmail.com", EmailVerified: true})
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM users WHERE google_sub = $1`, sub) })
	if loc := googleLogin(t, s, "https://evil.example").Header().Get("Location"); loc != "/" {
		t.Fatalf("open redirect: %q, want /", loc)
	}
}
