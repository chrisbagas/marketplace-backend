package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Login Google (OAuth 2.0 authorization code + OIDC userinfo).
// Aktif hanya bila GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, dan
// GOOGLE_REDIRECT_URL diisi. Redirect URL harus lewat proxy Next agar cookie
// sesi jatuh di origin web, mis. http://localhost:3000/api/auth/google/callback
// (daftarkan URL yang sama di Google Cloud Console → Credentials).

type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

func (g GoogleConfig) Enabled() bool {
	return g.ClientID != "" && g.ClientSecret != "" && g.RedirectURL != ""
}

// endpoint Google — variabel agar bisa diganti di tes
var (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	oauthHTTP         = &http.Client{Timeout: 10 * time.Second}
)

const oauthStateCookie = "kk_oauth_state"

// GET /api/auth/google/start?next=/profile → redirect ke layar persetujuan Google
func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	g := s.auth.Google
	if !g.Enabled() {
		errJSON(w, http.StatusNotFound, "Login Google belum dikonfigurasi")
		return
	}
	state := randomToken()
	next := safeNext(r.URL.Query().Get("next"))
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state + "|" + url.QueryEscape(next),
		Path: "/api/auth/google", MaxAge: 600, HttpOnly: true,
		Secure: s.auth.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{
		"client_id":     {g.ClientID},
		"redirect_uri":  {g.RedirectURL},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

// GET /api/auth/google/callback?code=...&state=...
func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string) {
		http.Redirect(w, r, "/login?error="+url.QueryEscape(reason), http.StatusFound)
	}
	if !s.auth.Google.Enabled() {
		fail("google-nonaktif")
		return
	}
	c, err := r.Cookie(oauthStateCookie)
	// cookie state hanya sekali pakai
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Path: "/api/auth/google", MaxAge: -1})
	if err != nil {
		fail("google-sesi-habis")
		return
	}
	wantState, rawNext, _ := strings.Cut(c.Value, "|")
	gotState := r.URL.Query().Get("state")
	if gotState == "" || subtle.ConstantTimeCompare([]byte(wantState), []byte(gotState)) != 1 {
		fail("google-state")
		return
	}
	if r.URL.Query().Get("error") != "" { // pengguna membatalkan
		fail("google-batal")
		return
	}
	next, _ := url.QueryUnescape(rawNext)
	next = safeNext(next)

	info, err := s.googleUserinfo(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		fail("google-gagal")
		return
	}
	if !info.EmailVerified || info.Sub == "" {
		fail("google-email-belum-terverifikasi")
		return
	}

	userID, err := s.findOrCreateGoogleUser(r.Context(), info)
	if err != nil {
		fail("google-gagal")
		return
	}
	if err := s.startSession(w, r, userID); err != nil {
		fail("google-gagal")
		return
	}
	http.Redirect(w, r, next, http.StatusFound)
}

type googleInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// googleUserinfo menukar authorization code ke access token lalu membaca
// profil dari endpoint userinfo (koneksi TLS langsung ke Google, jadi tidak
// perlu memverifikasi tanda tangan id_token sendiri).
func (s *Server) googleUserinfo(ctx context.Context, code string) (*googleInfo, error) {
	if code == "" {
		return nil, fmt.Errorf("code kosong")
	}
	g := s.auth.Google
	form := url.Values{
		"code":          {code},
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"redirect_uri":  {g.RedirectURL},
		"grant_type":    {"authorization_code"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint: %s", res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("token tidak valid")
	}

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res2, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo endpoint: %s", res2.Status)
	}
	var info googleInfo
	if err := json.NewDecoder(res2.Body).Decode(&info); err != nil {
		return nil, err
	}
	info.Email = strings.ToLower(strings.TrimSpace(info.Email))
	return &info, nil
}

// findOrCreateGoogleUser: cocokkan dengan google_sub dulu, lalu email
// (Google sudah memverifikasi email-nya, jadi aman ditautkan), atau buat
// akun pelanggan baru dengan username turunan dari email.
func (s *Server) findOrCreateGoogleUser(ctx context.Context, info *googleInfo) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE google_sub = $1`, info.Sub).Scan(&id)
	if err == nil {
		return id, nil
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE users SET google_sub = $2, email_verified = true
		WHERE lower(email) = $1 AND google_sub IS NULL RETURNING id`, info.Email, info.Sub).Scan(&id)
	if err == nil {
		return id, nil
	}

	name := strings.TrimSpace(info.Name)
	base := usernameFromEmail(info.Email)
	if name == "" {
		name = base
	}
	for attempt := 0; attempt < 5; attempt++ {
		username := base
		if attempt > 0 {
			username = fmt.Sprintf("%s%s", truncate(base, 24), randomHex(2))
		}
		id, err = s.createUser(ctx, newUser{
			Username: username, Email: info.Email, Name: name, Role: "customer",
			GoogleSub: info.Sub, EmailVerified: true,
		})
		if err == nil {
			return id, nil
		}
		if !strings.Contains(uniqueViolationMessage(err), "Username") {
			return "", err
		}
	}
	return "", fmt.Errorf("tidak bisa membuat username unik")
}

var nonUsernameRe = regexp.MustCompile(`[^a-z0-9_.]+`)

func usernameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	u := strings.Trim(nonUsernameRe.ReplaceAllString(strings.ToLower(local), ""), ".")
	if len(u) < 3 {
		u = "user" + u
	}
	return truncate(u, 30)
}
