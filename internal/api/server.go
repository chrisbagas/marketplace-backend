package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	pool *pgxpool.Pool
	mux  *http.ServeMux
}

func NewServer(pool *pgxpool.Pool) *Server {
	s := &Server{pool: pool, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	s.route("POST /api/track", "/api/track", s.postTrack)
	s.route("GET /api/track", "/api/track", s.getTrack)
	s.route("POST /api/vitals", "/api/vitals", s.postVitals)
	s.route("GET /api/stats", "/api/stats", s.getStats)
	s.route("POST /api/orders", "/api/orders", s.postOrder)
	s.route("GET /api/orders", "/api/orders", s.getOrders)
	s.route("GET /api/orders/{id}", "/api/orders", s.getOrder)
	s.route("PATCH /api/orders/{id}", "/api/orders", s.patchOrder)
	s.route("GET /api/designs", "/api/designs", s.getDesigns)
	s.route("POST /api/designs", "/api/designs", s.postDesign)
	s.route("PATCH /api/designs", "/api/designs", s.patchDesign)
	s.route("GET /api/products", "/api/products", s.getProducts)
	s.route("GET /api/products/{id}", "/api/products", s.getProduct)
	s.route("GET /api/categories", "/api/categories", s.getCategories)
	s.route("PATCH /api/listings/{id}", "/api/listings", s.patchListing)
	s.route("GET /api/reviews", "/api/reviews", s.getReviews)
	s.route("POST /api/reviews", "/api/reviews", s.postReview)
	s.route("PATCH /api/reviews", "/api/reviews", s.patchReview)

	return s
}

// route registers a handler wrapped with latency/error recording (api_metrics).
func (s *Server) route(pattern, metricRoute string, h http.HandlerFunc) {
	s.mux.Handle(pattern, s.withMetrics(metricRoute, h))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS: dev berjalan lintas port (Next 3000 → API 8080)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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
