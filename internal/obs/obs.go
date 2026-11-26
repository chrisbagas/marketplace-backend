// Package obs wires the API's observability: structured logging (log/slog) and
// error reporting to Sentry. Everything is a no-op until configured, so dev and
// tests run without any of it.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/getsentry/sentry-go"
)

// SetupLogging installs the process-wide slog default. Production logs are JSON
// (one object per line, easy to ship); dev logs stay human-readable text. As a
// side effect of slog.SetDefault, every existing log.Printf call goes through
// the same handler at INFO level, so no call site has to change.
func SetupLogging(production bool) {
	slog.SetDefault(newLogger(production, os.Stderr))
}

func newLogger(production bool, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if production {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Config for InitSentry. An empty DSN disables Sentry entirely.
type Config struct {
	DSN         string
	Environment string
	Release     string
	// Transport overrides the HTTP transport (tests use a fake one).
	Transport sentry.Transport
}

// ConfigFromEnv reads SENTRY_DSN and SENTRY_ENVIRONMENT. Release is passed in
// because it is a build-time value (see `release` in cmd/api/main.go); when
// empty the SDK falls back to the SENTRY_RELEASE env var.
func ConfigFromEnv(release string) Config {
	env := os.Getenv("SENTRY_ENVIRONMENT")
	if env == "" {
		env = os.Getenv("APP_ENV")
	}
	if env == "" {
		env = "development"
	}
	return Config{DSN: os.Getenv("SENTRY_DSN"), Environment: env, Release: release}
}

// InitSentry starts the Sentry client when cfg.DSN is set. It returns a flush
// function to call before the process exits (it waits up to 2 s for queued
// events); the function is safe to call when Sentry is disabled.
func InitSentry(cfg Config) (flush func(), err error) {
	if cfg.DSN == "" {
		return func() {}, nil
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:         cfg.DSN,
		Environment: cfg.Environment,
		Release:     cfg.Release,
		Transport:   cfg.Transport,
		// Never attach cookies, auth headers or client IPs automatically.
		SendDefaultPII: false,
		BeforeSend:     scrub,
	})
	if err != nil {
		return func() {}, fmt.Errorf("sentry init: %w", err)
	}
	return func() { sentry.Flush(2 * time.Second) }, nil
}

// Enabled reports whether a Sentry client has been initialised.
func Enabled() bool { return sentry.CurrentHub().Client() != nil }

// scrub drops request parts that can carry secrets: query strings hold OAuth
// codes and one-time tokens, and cookies hold the session.
func scrub(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event.Request != nil {
		event.Request.QueryString = ""
		event.Request.Cookies = ""
		event.Request.Data = ""
	}
	return event
}

func hubFor(r *http.Request, route string) *sentry.Hub {
	hub := sentry.CurrentHub().Clone()
	hub.Scope().SetRequest(r)
	hub.Scope().SetTag("route", route)
	return hub
}

// CapturePanic reports a recovered handler panic and returns the event id (nil
// when Sentry is disabled). It also logs the panic with its stack trace.
func CapturePanic(r *http.Request, route string, recovered any) *sentry.EventID {
	slog.Error("panic in handler",
		"route", route, "method", r.Method, "path", r.URL.Path,
		"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
	if !Enabled() {
		return nil
	}
	// Always report an exception (not a bare message) so Sentry keeps the stack and groups by it.
	hub := hubFor(r, route)
	hub.Scope().SetLevel(sentry.LevelFatal)
	return hub.CaptureException(fmt.Errorf("panic: %v", recovered))
}

// CaptureServerError reports a 5xx response a handler wrote on its own (handlers
// answer with errJSON rather than returning an error, so there is no error value
// to attach; the event is a message tagged with route and status).
func CaptureServerError(r *http.Request, route string, status int) {
	if !Enabled() {
		return
	}
	hub := hubFor(r, route)
	hub.Scope().SetTag("http.status", fmt.Sprint(status))
	endpoint := r.Pattern // e.g. "PATCH /api/orders/{id}": stable across ids, so events group per endpoint
	if endpoint == "" {
		endpoint = r.Method + " " + route
	}
	hub.CaptureMessage(fmt.Sprintf("HTTP %d: %s", status, endpoint))
}
