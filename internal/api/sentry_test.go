package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"karyakita/api/internal/obs"
)

// fakeTransport records Sentry events instead of sending them.
type fakeTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (f *fakeTransport) Configure(sentry.ClientOptions) {}
func (f *fakeTransport) SendEvent(e *sentry.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}
func (f *fakeTransport) Flush(time.Duration) bool              { return true }
func (f *fakeTransport) FlushWithContext(context.Context) bool { return true }
func (f *fakeTransport) Close()                                {}
func (f *fakeTransport) captured() []*sentry.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*sentry.Event(nil), f.events...)
}

// obsServer builds a Server whose pool never connects (it points at a closed
// port; the metrics insert fails and is only logged) with three test routes.
func obsServer(t *testing.T) *Server {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Server{pool: pool, mux: http.NewServeMux()}
	s.route("GET /boom", "/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	s.route("GET /late-boom", "/late-boom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("partial"))
		panic("late boom")
	})
	s.route("GET /fail", "/fail", func(w http.ResponseWriter, r *http.Request) { errJSON(w, http.StatusBadGateway, "gateway down") })
	s.route("GET /bad", "/bad", func(w http.ResponseWriter, r *http.Request) { errJSON(w, http.StatusBadRequest, "nope") })
	return s
}

func initFakeSentry(t *testing.T) *fakeTransport {
	t.Helper()
	ft := &fakeTransport{}
	if _, err := obs.InitSentry(obs.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "r1", Transport: ft}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sentry.CurrentHub().BindClient(nil) })
	return ft
}

func TestPanicBecomes500JSONAndOneSentryEvent(t *testing.T) {
	ft := initFakeSentry(t)
	s := obsServer(t)

	req := httptest.NewRequest("GET", "/boom?code=secret-oauth-code", nil)
	req.Header.Set("Cookie", "kk_session=secret-session")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("body = %q, want {\"error\": ...}", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("panic value leaked to the client: %s", rec.Body.String())
	}

	evs := ft.captured()
	if len(evs) != 1 {
		t.Fatalf("captured %d events, want 1", len(evs))
	}
	e := evs[0]
	if len(e.Exception) == 0 || !strings.Contains(e.Exception[0].Value, "boom") {
		t.Errorf("exception = %+v, want the panic value", e.Exception)
	}
	if e.Environment != "test" || e.Release != "r1" {
		t.Errorf("environment/release = %q/%q", e.Environment, e.Release)
	}
	if e.Tags["route"] != "/boom" {
		t.Errorf("route tag = %q", e.Tags["route"])
	}
	if e.Request == nil {
		t.Fatal("event has no request context")
	}
	if e.Request.QueryString != "" || e.Request.Cookies != "" {
		t.Errorf("secrets not scrubbed: query=%q cookies=%q", e.Request.QueryString, e.Request.Cookies)
	}
}

func TestPanicAfterHeadersKeepsResponseAndReports(t *testing.T) {
	ft := initFakeSentry(t)
	rec := httptest.NewRecorder()
	obsServer(t).ServeHTTP(rec, httptest.NewRequest("GET", "/late-boom", nil))
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Errorf("response = %d %q, want the already-sent 202 partial", rec.Code, rec.Body.String())
	}
	if n := len(ft.captured()); n != 1 {
		t.Errorf("captured %d events, want 1", n)
	}
}

func TestServerErrorResponseIsCapturedOnce(t *testing.T) {
	ft := initFakeSentry(t)
	s := obsServer(t)

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/bad", nil))
	if n := len(ft.captured()); n != 0 {
		t.Fatalf("a 400 captured %d events, want 0", n)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/fail", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	evs := ft.captured()
	if len(evs) != 1 {
		t.Fatalf("a 502 captured %d events, want 1", len(evs))
	}
	if !strings.Contains(evs[0].Message, "502") || !strings.Contains(evs[0].Message, "GET /fail") {
		t.Errorf("message = %q", evs[0].Message)
	}
}

func TestPanicWithoutSentryStillReturns500(t *testing.T) {
	// DSN unset: nothing is initialised, but panics are still turned into 500 JSON.
	flush, err := obs.InitSentry(obs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	flush()
	if obs.Enabled() {
		t.Fatal("Sentry enabled without a DSN")
	}
	rec := httptest.NewRecorder()
	obsServer(t).ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("response = %d %q", rec.Code, rec.Body.String())
	}
}
