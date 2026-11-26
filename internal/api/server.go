package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"karyakita/api/internal/mail"
)

type Server struct {
	pool      *pgxpool.Pool
	mux       *http.ServeMux
	uploadDir string
	auth      AuthConfig
	limiter   *rateLimiter
	payments  PaymentsConfig
}

func NewServer(pool *pgxpool.Pool, uploadDir string, auth AuthConfig) *Server {
	if auth.Mailer == nil {
		auth.Mailer = mail.Log{ShowBody: true}
	}
	if auth.AppURL == "" {
		auth.AppURL = "http://localhost:3000"
	}
	s := &Server{pool: pool, mux: http.NewServeMux(), uploadDir: uploadDir, auth: auth, limiter: newRateLimiter()}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// Akses: publik kecuali dibungkus s.authed(handler, peran...).
	// s.authed tanpa peran = cukup login.
	const designer, admin = "designer", "admin"

	s.route("GET /api/auth/me", "/api/auth", s.getMe)
	s.route("GET /api/auth/providers", "/api/auth", s.getProviders)
	s.route("POST /api/auth/signup", "/api/auth", s.postSignup)
	s.route("POST /api/auth/login", "/api/auth", s.postLogin)
	s.route("POST /api/auth/logout", "/api/auth", s.postLogout)
	s.route("POST /api/auth/verify-email", "/api/auth", s.postVerifyEmail)
	s.route("POST /api/auth/verify-email/resend", "/api/auth", s.authed(s.postResendVerification))
	s.route("POST /api/auth/password/forgot", "/api/auth", s.postForgotPassword)
	s.route("POST /api/auth/password/reset", "/api/auth", s.postResetPassword)
	s.route("GET /api/auth/google/start", "/api/auth/google", s.googleStart)
	s.route("GET /api/auth/google/callback", "/api/auth/google", s.googleCallback)

	s.route("POST /api/track", "/api/track", s.postTrack)
	s.route("GET /api/track", "/api/track", s.authed(s.getTrack, admin))
	s.route("POST /api/vitals", "/api/vitals", s.postVitals)
	s.route("GET /api/stats", "/api/stats", s.authed(s.getStats, designer, admin))
	// checkout wajib login: pesanan selalu tertaut ke akun, dan hanya pemilik/admin yang bisa melihatnya
	s.route("POST /api/checkout/quote", "/api/checkout", s.authed(s.postQuote))
	s.route("POST /api/orders", "/api/orders", s.authed(s.postOrder))
	s.route("GET /api/orders", "/api/orders", s.authed(s.getOrders, admin))
	s.route("GET /api/orders/mine", "/api/orders", s.authed(s.getMyOrders))
	s.route("GET /api/payments/config", "/api/payments", s.getPaymentConfig)
	s.route("POST /api/payments/midtrans/notification", "/api/payments", s.postMidtransNotification) // dipanggil Midtrans; dijaga tanda tangan
	s.route("POST /api/orders/{id}/payment", "/api/orders", s.authed(s.postPaymentSession))
	s.route("POST /api/orders/{id}/payment/sync", "/api/orders", s.authed(s.postPaymentSync))
	s.route("GET /api/orders/{id}", "/api/orders", s.authed(s.getOrder))     // pemilik / admin
	s.route("PATCH /api/orders/{id}", "/api/orders", s.authed(s.patchOrder)) // pay: pemilik/admin · advance: admin
	s.route("GET /api/designer/orders", "/api/designer", s.authed(s.getDesignerOrders, designer, admin))
	// penarikan saldo kreator: royalti bisa ditarik setelah pesanan selesai; transfer manual oleh admin
	s.route("GET /api/designer/payouts", "/api/designer", s.authed(s.getDesignerPayouts, designer, admin))
	s.route("POST /api/designer/payouts", "/api/designer", s.authed(s.postDesignerPayout, designer))
	s.route("PATCH /api/designer/bank", "/api/designer", s.authed(s.patchDesignerBank, designer))
	s.route("GET /api/payouts", "/api/payouts", s.authed(s.getPayouts, admin))
	s.route("PATCH /api/payouts/{id}", "/api/payouts", s.authed(s.patchPayout, admin))
	// keuangan: pembagian uang per pesanan + tagihan produsen (vendor cetak yang juga mengirim paket)
	s.route("GET /api/finance", "/api/finance", s.authed(s.getFinance, admin))
	s.route("GET /api/producers", "/api/producers", s.authed(s.getProducers, admin))
	s.route("PATCH /api/producers/{id}", "/api/producers", s.authed(s.patchProducer, admin))
	s.route("POST /api/producers/{id}/payouts", "/api/producers", s.authed(s.postProducerPayout, admin))
	s.route("PATCH /api/product-types/{id}", "/api/product-types", s.authed(s.patchProductType, admin))
	s.route("GET /api/vouchers", "/api/vouchers", s.authed(s.getVouchers, admin))
	s.route("POST /api/vouchers", "/api/vouchers", s.authed(s.postVoucher, admin))
	s.route("PATCH /api/vouchers/{code}", "/api/vouchers", s.authed(s.patchVoucher, admin))
	s.route("GET /api/designs", "/api/designs", s.authed(s.getDesigns, designer, admin))
	s.route("POST /api/designs", "/api/designs", s.authed(s.postDesign, designer, admin))
	s.route("PATCH /api/designs", "/api/designs", s.authed(s.patchDesign, admin))
	s.route("GET /api/products", "/api/products", s.getProducts)
	s.route("GET /api/products/{id}", "/api/products", s.getProduct)
	s.route("GET /api/search", "/api/search", s.getSearch)
	s.route("GET /api/search/suggest", "/api/search/suggest", s.getSuggest)
	s.route("GET /api/taxonomy", "/api/taxonomy", s.getTaxonomy)
	s.route("GET /api/search/insights", "/api/search/admin", s.authed(s.getSearchInsights, admin))
	s.route("POST /api/search/synonyms", "/api/search/admin", s.authed(s.postSynonym, admin))
	s.route("DELETE /api/search/synonyms/{id}", "/api/search/admin", s.authed(s.deleteSynonym, admin))
	s.route("POST /api/attributes/{id}/values", "/api/attributes", s.authed(s.postAttributeValue, admin))
	s.route("PATCH /api/attribute-values/{id}", "/api/attributes", s.authed(s.patchAttributeValue, admin))
	s.route("DELETE /api/attribute-values/{id}", "/api/attributes", s.authed(s.deleteAttributeValue, admin))
	s.route("PATCH /api/listings/{id}", "/api/listings", s.authed(s.patchListing, designer, admin))
	s.route("POST /api/uploads", "/api/uploads", s.authed(s.postUpload))
	s.route("GET /api/profile", "/api/profile", s.authed(s.getProfile))
	s.route("PATCH /api/profile", "/api/profile", s.authed(s.patchProfile))
	s.route("GET /api/user-designs", "/api/user-designs", s.authed(s.getUserDesigns))
	s.route("POST /api/user-designs", "/api/user-designs", s.authed(s.postUserDesign))
	s.route("DELETE /api/user-designs/{id}", "/api/user-designs", s.authed(s.deleteUserDesign))
	s.mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadDir))))
	s.route("GET /api/reviews", "/api/reviews", s.getReviews)
	s.route("POST /api/reviews", "/api/reviews", s.authed(s.postReview)) // bukti: pesanan selesai milik akun ini
	s.route("PATCH /api/reviews", "/api/reviews", s.authed(s.patchReview, admin))

	return s
}

// route registers a handler wrapped with latency/error recording (api_metrics).
func (s *Server) route(pattern, metricRoute string, h http.HandlerFunc) {
	s.mux.Handle(pattern, s.withMetrics(metricRoute, h))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No CORS: the browser only talks to the Next proxy (same origin) and the
	// Midtrans webhook is server-to-server.
	s.mux.ServeHTTP(w, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) withMetrics(route string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		t0 := time.Now()
		next(rec, r)
		ms := float64(time.Since(t0).Microseconds()) / 1000
		go func(method string, status int) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := s.pool.Exec(ctx,
				`INSERT INTO api_metrics (route, method, status, ms, ok) VALUES ($1,$2,$3,$4,$5)`,
				route, method, status, ms, status < 400)
			if err != nil {
				log.Printf("api_metrics insert: %v", err)
			}
		}(r.Method, rec.status)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2_000_000)) // 2 MB cap
	return dec.Decode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
