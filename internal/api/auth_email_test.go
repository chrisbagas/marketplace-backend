package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"karyakita/api/internal/mail"
)

// Tes integrasi verifikasi email & reset password (butuh TEST_DATABASE_URL,
// lihat google_test.go). Email ditangkap oleh fakeMailer, bukan dikirim.

type fakeMailer struct{ sent chan mail.Message }

func (f *fakeMailer) Send(_ context.Context, m mail.Message) error {
	f.sent <- m
	return nil
}

func (f *fakeMailer) next(t *testing.T) mail.Message {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("email tidak terkirim dalam 5 detik")
		return mail.Message{}
	}
}

func (f *fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-f.sent:
		t.Fatalf("email tak terduga ke %s: %q", m.To, m.Subject)
	case <-time.After(300 * time.Millisecond):
	}
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

func tokenFrom(t *testing.T, m mail.Message, path string) string {
	t.Helper()
	if !strings.Contains(m.Text, "http://app.test"+path+"?token=") {
		t.Fatalf("email %q tidak berisi link ke %s:\n%s", m.Subject, path, m.Text)
	}
	return tokenRe.FindStringSubmatch(m.Text)[1]
}

func emailServer(t *testing.T) (*Server, *fakeMailer, *pgxpool.Pool) {
	pool := testPool(t)
	fm := &fakeMailer{sent: make(chan mail.Message, 10)}
	return NewServer(pool, os.TempDir(), AuthConfig{AppURL: "http://app.test", Mailer: fm}), fm, pool
}

// call menjalankan satu request JSON; cookies dari respons dikembalikan.
func call(s *Server, method, path string, body any, cookies ...*http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	out := map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func cleanupUser(t *testing.T, pool *pgxpool.Pool, username string) {
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM design_submissions WHERE designer_id IN
			(SELECT d.id FROM designers d JOIN users u ON u.id = d.user_id WHERE u.username = $1)`, username)
		pool.Exec(ctx, `DELETE FROM designers WHERE user_id IN (SELECT id FROM users WHERE username = $1)`, username)
		pool.Exec(ctx, `DELETE FROM users WHERE username = $1`, username)
	})
}

func meVerified(t *testing.T, s *Server, c *http.Cookie) bool {
	t.Helper()
	_, out := call(s, "GET", "/api/auth/me", nil, c)
	u, ok := out["user"].(map[string]any)
	if !ok {
		t.Fatal("me: tidak login")
	}
	return u["emailVerified"] == true
}

func TestEmailVerificationFlow(t *testing.T) {
	s, fm, pool := emailServer(t)
	username := "tesverif_" + randomHex(3)
	cleanupUser(t, pool, username)

	rec, _ := call(s, "POST", "/api/auth/signup", map[string]any{
		"username": username, "email": username + "@mail.test", "password": "rahasia123",
		"name": "Tes Verif", "asCreator": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body)
	}
	session := sessionCookieFrom(rec)
	msg := fm.next(t)
	if msg.To != username+"@mail.test" || !strings.Contains(msg.Subject, "Verifikasi") {
		t.Fatalf("email verifikasi salah: ke %s, %q", msg.To, msg.Subject)
	}
	token := tokenFrom(t, msg, "/verifikasi-email")
	if meVerified(t, s, session) {
		t.Fatal("akun baru sudah terverifikasi sebelum link dibuka")
	}

	// kreator belum terverifikasi tidak boleh mengajukan desain
	design := map[string]any{"title": "Tes", "uri": "data:image/svg+xml,x"}
	if rec, _ := call(s, "POST", "/api/designs", design, session); rec.Code != http.StatusForbidden {
		t.Fatalf("ajukan desain sebelum verifikasi: %d, want 403", rec.Code)
	}

	if rec, _ := call(s, "POST", "/api/auth/verify-email", map[string]string{"token": "palsu"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("token palsu: %d, want 400", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/auth/verify-email", map[string]string{"token": token}); rec.Code != http.StatusOK {
		t.Fatalf("verifikasi: %d %s", rec.Code, rec.Body)
	}
	if !meVerified(t, s, session) {
		t.Fatal("akun belum terverifikasi setelah link dibuka")
	}
	// link dibuka lagi (mis. oleh pemindai email) → tetap sukses, bukan galat
	if rec, out := call(s, "POST", "/api/auth/verify-email", map[string]string{"token": token}); rec.Code != http.StatusOK || out["alreadyVerified"] != true {
		t.Fatalf("verifikasi ulang: %d %v", rec.Code, out)
	}
	if rec, _ := call(s, "POST", "/api/designs", design, session); rec.Code != http.StatusOK {
		t.Fatalf("ajukan desain setelah verifikasi: %d %s", rec.Code, rec.Body)
	}
	// sudah terverifikasi → kirim ulang tidak mengirim email
	if _, out := call(s, "POST", "/api/auth/verify-email/resend", nil, session); out["alreadyVerified"] != true {
		t.Fatalf("resend setelah verifikasi: %v", out)
	}
	fm.none(t)
}

func TestResendInvalidatesOldVerificationLink(t *testing.T) {
	s, fm, pool := emailServer(t)
	username := "tesresend_" + randomHex(3)
	cleanupUser(t, pool, username)
	rec, _ := call(s, "POST", "/api/auth/signup", map[string]any{
		"username": username, "email": username + "@mail.test", "password": "rahasia123",
	})
	session := sessionCookieFrom(rec)
	oldToken := tokenFrom(t, fm.next(t), "/verifikasi-email")

	if rec, _ := call(s, "POST", "/api/auth/verify-email/resend", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("resend tanpa login: %d, want 401", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/auth/verify-email/resend", nil, session); rec.Code != http.StatusOK {
		t.Fatalf("resend: %d", rec.Code)
	}
	newToken := tokenFrom(t, fm.next(t), "/verifikasi-email")
	if rec, _ := call(s, "POST", "/api/auth/verify-email", map[string]string{"token": oldToken}); rec.Code != http.StatusBadRequest {
		t.Fatalf("link lama setelah kirim ulang: %d, want 400", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/auth/verify-email", map[string]string{"token": newToken}); rec.Code != http.StatusOK {
		t.Fatalf("link baru: %d", rec.Code)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	s, fm, pool := emailServer(t)
	username := "tesreset_" + randomHex(3)
	email := username + "@mail.test"
	cleanupUser(t, pool, username)

	rec, _ := call(s, "POST", "/api/auth/signup", map[string]any{"username": username, "email": email, "password": "passwordlama"})
	oldSession := sessionCookieFrom(rec)
	fm.next(t) // email verifikasi dari signup

	// email tidak terdaftar → respons identik, tidak ada email
	recUnknown, outUnknown := call(s, "POST", "/api/auth/password/forgot", map[string]string{"email": "tidakada_" + randomHex(4) + "@mail.test"})
	fm.none(t)
	recForgot, outForgot := call(s, "POST", "/api/auth/password/forgot", map[string]string{"email": strings.ToUpper(email)})
	if recUnknown.Code != http.StatusOK || recForgot.Code != http.StatusOK || outUnknown["message"] != outForgot["message"] {
		t.Fatalf("respons lupa password membocorkan keberadaan akun: %v vs %v", outUnknown, outForgot)
	}
	firstToken := tokenFrom(t, fm.next(t), "/reset-password")

	// permintaan kedua membatalkan link pertama
	call(s, "POST", "/api/auth/password/forgot", map[string]string{"email": email})
	token := tokenFrom(t, fm.next(t), "/reset-password")
	if rec, _ := call(s, "POST", "/api/auth/password/reset", map[string]string{"token": firstToken, "password": "passwordbaru"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("link reset lama: %d, want 400", rec.Code)
	}

	if rec, _ := call(s, "POST", "/api/auth/password/reset", map[string]string{"token": token, "password": "pendek"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("password terlalu pendek: %d, want 400", rec.Code)
	}
	rec, out := call(s, "POST", "/api/auth/password/reset", map[string]string{"token": token, "password": "passwordbaru"})
	if rec.Code != http.StatusOK || sessionCookieFrom(rec) == nil {
		t.Fatalf("reset: %d %v", rec.Code, out)
	}
	if u := out["user"].(map[string]any); u["emailVerified"] != true {
		t.Error("reset lewat email tidak menandai email terverifikasi")
	}
	if changed := fm.next(t); !strings.Contains(changed.Subject, "diubah") {
		t.Fatalf("email pemberitahuan salah: %q", changed.Subject)
	}

	// sesi lama dicabut, password lama tidak berlaku, link tidak bisa dipakai ulang
	if _, out := call(s, "GET", "/api/auth/me", nil, oldSession); out["user"] != nil {
		t.Fatal("sesi lama masih aktif setelah reset password")
	}
	if rec, _ := call(s, "POST", "/api/auth/login", map[string]string{"identifier": username, "password": "passwordlama"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login dengan password lama: %d, want 401", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/auth/login", map[string]string{"identifier": username, "password": "passwordbaru"}); rec.Code != http.StatusOK {
		t.Fatalf("login dengan password baru: %d", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/auth/password/reset", map[string]string{"token": token, "password": "passwordlain"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("link reset dipakai ulang: %d, want 400", rec.Code)
	}
}

func TestPasswordResetExpiredToken(t *testing.T) {
	s, _, pool := emailServer(t)
	ctx := context.Background()
	userID, username := "u-tes"+randomHex(3), "tesexp_"+randomHex(3)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, email, name) VALUES ($1,$2,$3,'Tes')`,
		userID, username, username+"@mail.test"); err != nil {
		t.Fatal(err)
	}
	cleanupUser(t, pool, username)
	token := randomToken()
	pool.Exec(ctx, `INSERT INTO auth_tokens (id, user_id, purpose, expires_at) VALUES ($1,$2,'reset_password', now() - interval '1 minute')`,
		hashToken(token), userID)
	if rec, _ := call(s, "POST", "/api/auth/password/reset", map[string]string{"token": token, "password": "passwordbaru"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("token kedaluwarsa: %d, want 400", rec.Code)
	}
}
