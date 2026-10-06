package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/db"
)

// Autentikasi berbasis sesi: token acak di cookie HttpOnly, hanya sha256-nya
// yang disimpan di tabel sessions. Frontend memanggil API lewat proxy Next
// (origin yang sama), jadi cookie SameSite=Lax cukup dan tidak perlu CORS.

const (
	sessionCookie = "kk_session"
	sessionTTL    = 30 * 24 * time.Hour
	minPassword   = 8
	maxPassword   = 72 // batas input bcrypt (byte)
)

type AuthConfig struct {
	CookieSecure bool // true di produksi (HTTPS)
	Google       GoogleConfig
}

type DesignerRef struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	City      string  `json:"city"`
	Hue       int     `json:"hue"`
	Followers int     `json:"followers"`
	Rating    float64 `json:"rating"`
	AvatarURI string  `json:"avatarUri"`
}

// User adalah pengguna yang sedang login (bentuk respons /api/auth/me).
type User struct {
	ID           string       `json:"id"`
	Username     string       `json:"username"`
	Email        string       `json:"email"`
	Name         string       `json:"name"`
	Role         string       `json:"role"` // customer | designer | admin
	HasPassword  bool         `json:"hasPassword"`
	GoogleLinked bool         `json:"googleLinked"`
	Designer     *DesignerRef `json:"designer,omitempty"`
}

// ---- sesi ---------------------------------------------------------------

type ctxUserKey struct{}

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUserKey{}).(*User)
	return u
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand tidak pernah gagal di platform yang didukung
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionUser membaca cookie sesi dan memuat user-nya; nil bila tidak login.
func (s *Server) sessionUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	return s.loadUser(r.Context(), `
		JOIN sessions s ON s.user_id = u.id
		WHERE s.id = $1 AND s.expires_at > now()`, hashToken(c.Value))
}

func (s *Server) loadUser(ctx context.Context, where string, args ...any) (*User, error) {
	var u User
	var dID, dName, dCity, dAvatar *string
	var dHue, dFollowers *int
	var dRating *float64
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.email, u.name, u.role::text,
		       u.password_hash IS NOT NULL, u.google_sub IS NOT NULL,
		       d.id, d.name, d.city, d.hue, d.followers, d.rating::float8, d.avatar_uri
		FROM users u
		LEFT JOIN designers d ON d.user_id = u.id
		`+where, args...).
		Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Role, &u.HasPassword, &u.GoogleLinked,
			&dID, &dName, &dCity, &dHue, &dFollowers, &dRating, &dAvatar)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if dID != nil {
		u.Designer = &DesignerRef{ID: *dID, Name: *dName, City: *dCity, Hue: *dHue,
			Followers: *dFollowers, Rating: *dRating, AvatarURI: *dAvatar}
	}
	return &u, nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token := randomToken()
	ctx := r.Context()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, user_agent, ip) VALUES ($1,$2,$3,$4,$5)`,
		hashToken(token), userID, time.Now().Add(sessionTTL), truncate(r.UserAgent(), 300), clientIP(r)); err != nil {
		return err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	// bersihkan sesi kedaluwarsa milik user ini sekalian
	_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND expires_at < now()`, userID)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: s.auth.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.auth.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// authed membungkus handler yang butuh login. Tanpa roles = peran apa pun.
// 401 bila belum login, 403 bila perannya tidak cocok.
func (s *Server) authed(h http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.sessionUser(r)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if u == nil {
			errJSON(w, http.StatusUnauthorized, "Silakan masuk terlebih dahulu")
			return
		}
		if len(roles) > 0 && !hasRole(u, roles...) {
			errJSON(w, http.StatusForbidden, "Akun kamu tidak punya akses ke fitur ini")
			return
		}
		next := r.WithContext(context.WithValue(r.Context(), ctxUserKey{}, u))
		h(w, next)
	}
}

// optionalUser menempelkan user ke context bila login, tanpa mewajibkan.
func (s *Server) optionalUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, err := s.sessionUser(r); err == nil && u != nil {
			r = r.WithContext(context.WithValue(r.Context(), ctxUserKey{}, u))
		}
		h(w, r)
	}
}

func hasRole(u *User, roles ...string) bool {
	for _, role := range roles {
		if u.Role == role {
			return true
		}
	}
	return false
}

// ---- handler ------------------------------------------------------------

var usernameRe = regexp.MustCompile(`^[a-z0-9_.]{3,30}$`)

// dummyHash dipakai saat user tidak ditemukan supaya waktu respons login
// tidak membocorkan apakah username/email terdaftar.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("karyakita-dummy-password"), bcrypt.DefaultCost)

// GET /api/auth/me → {user} atau {user: null}
func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.sessionUser(r)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// GET /api/auth/providers → metode login yang aktif (tombol Google hanya
// muncul bila kredensial OAuth sudah dikonfigurasi).
func (s *Server) getProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"password": true, "google": s.auth.Google.Enabled()})
}

// POST /api/auth/signup {username, email, password, name, asCreator, city}
func (s *Server) postSignup(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("signup:"+clientIP(r), 10, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu banyak percobaan. Coba lagi nanti.")
		return
	}
	var body struct {
		Username  string `json:"username"`
		Email     string `json:"email"`
		Password  string `json:"password"`
		Name      string `json:"name"`
		AsCreator bool   `json:"asCreator"`
		City      string `json:"city"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	username := strings.ToLower(strings.TrimSpace(body.Username))
	email := strings.ToLower(strings.TrimSpace(body.Email))
	name := strings.TrimSpace(body.Name)
	city := strings.TrimSpace(body.City)
	switch {
	case !usernameRe.MatchString(username):
		errJSON(w, http.StatusBadRequest, "Username 3–30 karakter: huruf kecil, angka, titik, atau garis bawah")
		return
	case !validEmail(email):
		errJSON(w, http.StatusBadRequest, "Format email tidak valid")
		return
	case len(body.Password) < minPassword:
		errJSON(w, http.StatusBadRequest, fmt.Sprintf("Password minimal %d karakter", minPassword))
		return
	case len(body.Password) > maxPassword:
		errJSON(w, http.StatusBadRequest, fmt.Sprintf("Password maksimal %d karakter", maxPassword))
		return
	case len(name) > 80 || len(city) > 60:
		errJSON(w, http.StatusBadRequest, "Nama atau kota terlalu panjang")
		return
	}
	if name == "" {
		name = username
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	role := "customer"
	if body.AsCreator {
		role = "designer"
	}
	userID, err := s.createUser(r.Context(), newUser{
		Username: username, Email: email, Name: name, Role: role,
		PasswordHash: string(hash), City: city,
	})
	if err != nil {
		if msg := uniqueViolationMessage(err); msg != "" {
			errJSON(w, http.StatusConflict, msg)
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondWithSession(w, r, userID, http.StatusCreated)
}

// POST /api/auth/login {identifier, password} — identifier = username atau email
func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ident := strings.ToLower(strings.TrimSpace(body.Identifier))
	if ident == "" || body.Password == "" {
		errJSON(w, http.StatusBadRequest, "Isi username/email dan password")
		return
	}
	if !s.limiter.allow("login-ip:"+clientIP(r), 30, 15*time.Minute) ||
		!s.limiter.allow("login-id:"+ident, 10, 15*time.Minute) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu banyak percobaan masuk. Coba lagi dalam 15 menit.")
		return
	}

	var userID string
	var hash *string
	err := s.pool.QueryRow(r.Context(),
		`SELECT id, password_hash FROM users WHERE lower(username) = $1 OR lower(email) = $1 LIMIT 1`, ident).
		Scan(&userID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(body.Password))
		errJSON(w, http.StatusUnauthorized, "Username/email atau password salah")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hash == nil {
		errJSON(w, http.StatusUnauthorized, "Akun ini terdaftar lewat Google — silakan masuk dengan Google")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(body.Password)) != nil {
		errJSON(w, http.StatusUnauthorized, "Username/email atau password salah")
		return
	}
	s.respondWithSession(w, r, userID, http.StatusOK)
}

// POST /api/auth/logout — hapus sesi di server + cookie di browser
func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_, _ = s.pool.Exec(r.Context(), `DELETE FROM sessions WHERE id = $1`, hashToken(c.Value))
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) respondWithSession(w http.ResponseWriter, r *http.Request, userID string, code int) {
	if err := s.startSession(w, r, userID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, err := s.loadUser(r.Context(), `WHERE u.id = $1`, userID)
	if err != nil || u == nil {
		errJSON(w, http.StatusInternalServerError, "Gagal memuat akun")
		return
	}
	writeJSON(w, code, map[string]any{"user": u})
}

// ---- pembuatan akun (dipakai signup password & Google) --------------------

type newUser struct {
	Username, Email, Name, Role, City string
	PasswordHash                      string // kosong = tanpa password (akun Google)
	GoogleSub                         string
	EmailVerified                     bool
}

func (s *Server) createUser(ctx context.Context, nu newUser) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	userID := "u-" + randomHex(6)
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, username, email, name, role, password_hash, google_sub, email_verified)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		userID, nu.Username, nu.Email, nu.Name, nu.Role,
		nilIfEmpty(nu.PasswordHash), nilIfEmpty(nu.GoogleSub), nu.EmailVerified); err != nil {
		return "", err
	}
	// akun kreator langsung mendapat profil toko (designers) yang tertaut
	if nu.Role == "designer" {
		hue := mrand.IntN(360)
		if _, err := tx.Exec(ctx, `
			INSERT INTO designers (id, user_id, name, city, hue, avatar_uri) VALUES ($1,$2,$3,$4,$5,$6)`,
			"d-"+randomHex(6), userID, nu.Name, nu.City, hue, db.AvatarURI(hue, nu.Name)); err != nil {
			return "", err
		}
	}
	return userID, tx.Commit(ctx)
}

func uniqueViolationMessage(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return ""
	}
	switch {
	case strings.Contains(pgErr.ConstraintName, "username"):
		return "Username sudah dipakai"
	case strings.Contains(pgErr.ConstraintName, "email"):
		return "Email sudah terdaftar — silakan masuk"
	case strings.Contains(pgErr.ConstraintName, "google_sub"):
		return "Akun Google ini sudah tertaut ke akun lain"
	}
	return "Data sudah terdaftar"
}

// ---- utilitas -----------------------------------------------------------

func validEmail(s string) bool {
	if len(s) > 254 || !strings.Contains(s, "@") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// clientIP: request datang lewat proxy Next, jadi ambil X-Forwarded-For pertama.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// safeNext hanya mengizinkan path internal ("/..."), mencegah open redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// rateLimiter: sliding window sederhana di memori (cukup untuk satu instance;
// pindahkan ke Redis bila API dijalankan lebih dari satu replika).
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{hits: map[string][]time.Time{}} }

func (l *rateLimiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= max {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	if len(l.hits) > 10_000 { // cegah memori tumbuh tanpa batas
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}
