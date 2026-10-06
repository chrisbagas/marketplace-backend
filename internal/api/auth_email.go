package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/mail"
)

// Verifikasi email & reset password. Token sekali pakai di tabel auth_tokens
// (hanya sha256-nya yang disimpan), dikirim sebagai link ke halaman frontend:
//   {APP_URL}/verifikasi-email?token=...  → POST /api/auth/verify-email
//   {APP_URL}/reset-password?token=...    → POST /api/auth/password/reset
// Token reset baru terpakai saat password baru dikirim, bukan saat link dibuka,
// karena pemindai keamanan email (mis. Outlook) membuka link secara otomatis.

const (
	purposeVerify = "verify_email"
	purposeReset  = "reset_password"
	verifyTTL     = 24 * time.Hour
	resetTTL      = 30 * time.Minute
)

var errBadToken = errors.New("token tidak valid")

// newAuthToken membuat token baru dan membatalkan token lama yang belum
// dipakai untuk tujuan yang sama (hanya link terbaru yang berlaku).
func (s *Server) newAuthToken(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error) {
	token := randomToken()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_tokens WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_tokens (id, user_id, purpose, expires_at) VALUES ($1,$2,$3,$4)`,
		hashToken(token), userID, purpose, time.Now().Add(ttl)); err != nil {
		return "", err
	}
	return token, tx.Commit(ctx)
}

// consumeAuthToken menandai token terpakai secara atomik dan mengembalikan pemiliknya.
func consumeAuthToken(ctx context.Context, tx pgx.Tx, token, purpose string) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		UPDATE auth_tokens SET used_at = now()
		WHERE id = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`, hashToken(token), purpose).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errBadToken
	}
	return userID, err
}

func (s *Server) link(path, token string) string {
	return s.auth.AppURL + path + "?token=" + url.QueryEscape(token)
}

// sendMail mengirim di latar belakang supaya respons API tidak menunggu SMTP.
func (s *Server) sendMail(m mail.Message) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.auth.Mailer.Send(ctx, m); err != nil {
			log.Printf("kirim email %q ke %s gagal: %v", m.Subject, m.To, err)
		}
	}()
}

func (s *Server) sendVerification(ctx context.Context, userID, email, name string) error {
	token, err := s.newAuthToken(ctx, userID, purposeVerify, verifyTTL)
	if err != nil {
		return err
	}
	s.sendMail(mail.VerifyEmail(email, name, s.link("/verifikasi-email", token)))
	return nil
}

// POST /api/auth/verify-email {token}
func (s *Server) postVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil || body.Token == "" {
		errJSON(w, http.StatusBadRequest, "Link verifikasi tidak valid")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	userID, err := consumeAuthToken(ctx, tx, body.Token, purposeVerify)
	if errors.Is(err, errBadToken) {
		// link yang sama dibuka dua kali (atau oleh pemindai email) → tetap sukses bila sudah terverifikasi
		var verified bool
		_ = s.pool.QueryRow(ctx, `
			SELECT u.email_verified FROM auth_tokens t JOIN users u ON u.id = t.user_id
			WHERE t.id = $1 AND t.purpose = $2`, hashToken(body.Token), purposeVerify).Scan(&verified)
		if verified {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alreadyVerified": true})
			return
		}
		errJSON(w, http.StatusBadRequest, "Link verifikasi tidak valid atau sudah kedaluwarsa — minta link baru")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET email_verified = true WHERE id = $1`, userID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/auth/verify-email/resend — butuh login
func (s *Server) postResendVerification(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alreadyVerified": true})
		return
	}
	if !s.limiter.allow("verify-resend:"+u.ID, 5, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu sering meminta link. Coba lagi dalam satu jam.")
		return
	}
	if err := s.sendVerification(r.Context(), u.ID, u.Email, u.Name); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/auth/password/forgot {email}
// Respons selalu sama, terdaftar atau tidak, supaya endpoint ini tidak bisa
// dipakai untuk menebak email mana yang punya akun.
func (s *Server) postForgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !validEmail(email) {
		errJSON(w, http.StatusBadRequest, "Format email tidak valid")
		return
	}
	if !s.limiter.allow("forgot-ip:"+clientIP(r), 10, time.Hour) ||
		!s.limiter.allow("forgot-email:"+email, 3, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu banyak permintaan. Coba lagi nanti.")
		return
	}

	ctx := r.Context()
	var userID, name string
	err := s.pool.QueryRow(ctx, `SELECT id, name FROM users WHERE lower(email) = $1`, email).Scan(&userID, &name)
	if err == nil {
		token, err := s.newAuthToken(ctx, userID, purposeReset, resetTTL)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.sendMail(mail.ResetPassword(email, name, s.link("/reset-password", token)))
	} else if !errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Jika email terdaftar, link untuk mengatur ulang password sudah dikirim.",
	})
}

// POST /api/auth/password/reset {token, password} → password baru, semua sesi
// lama dicabut, lalu langsung masuk dengan sesi baru.
func (s *Server) postResetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Token == "" {
		errJSON(w, http.StatusBadRequest, "Link reset tidak valid")
		return
	}
	if msg := passwordProblem(body.Password); msg != "" {
		errJSON(w, http.StatusBadRequest, msg)
		return
	}
	if !s.limiter.allow("reset-ip:"+clientIP(r), 20, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu banyak percobaan. Coba lagi nanti.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	userID, err := consumeAuthToken(ctx, tx, body.Token, purposeReset)
	if errors.Is(err, errBadToken) {
		errJSON(w, http.StatusBadRequest, "Link reset tidak valid, sudah dipakai, atau kedaluwarsa — minta link baru")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var email, name string
	// membuka link dari email = bukti kepemilikan email, jadi sekalian terverifikasi
	if err := tx.QueryRow(ctx, `
		UPDATE users SET password_hash = $2, email_verified = true WHERE id = $1
		RETURNING email, name`, userID, string(hash)).Scan(&email, &name); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// keluarkan semua perangkat (mungkin akun ini sedang dibajak) + matikan link reset lain
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_tokens WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purposeReset); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendMail(mail.PasswordChanged(email, name, s.auth.AppURL+"/lupa-password"))
	s.respondWithSession(w, r, userID, http.StatusOK)
}

func passwordProblem(p string) string {
	switch {
	case len(p) < minPassword:
		return fmt.Sprintf("Password minimal %d karakter", minPassword)
	case len(p) > maxPassword:
		return fmt.Sprintf("Password maksimal %d karakter", maxPassword)
	}
	return ""
}
