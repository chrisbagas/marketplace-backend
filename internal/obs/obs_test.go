package obs

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggingFormatByEnvironment(t *testing.T) {
	prevSlog, prevFlags, prevPrefix := slog.Default(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	var prod bytes.Buffer
	slog.SetDefault(newLogger(true, &prod))
	log.Printf("pesanan %s dibayar", "KK-1") // plain log call must flow through slog
	var rec map[string]any
	if err := json.Unmarshal(prod.Bytes(), &rec); err != nil {
		t.Fatalf("production output is not JSON: %q (%v)", prod.String(), err)
	}
	if rec["msg"] != "pesanan KK-1 dibayar" || rec["level"] != "INFO" {
		t.Errorf("record = %v", rec)
	}

	var dev bytes.Buffer
	slog.SetDefault(newLogger(false, &dev))
	log.Printf("halo")
	if out := dev.String(); strings.HasPrefix(out, "{") || !strings.Contains(out, "msg=halo") {
		t.Errorf("dev output should be text, got %q", out)
	}
}

func TestSentryDisabledWithoutDSN(t *testing.T) {
	flush, err := InitSentry(Config{})
	if err != nil {
		t.Fatal(err)
	}
	flush() // must not panic
	if Enabled() {
		t.Error("Sentry enabled without a DSN")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	t.Setenv("SENTRY_ENVIRONMENT", "")
	t.Setenv("APP_ENV", "production")
	if c := ConfigFromEnv("abc123"); c.DSN != "" || c.Environment != "production" || c.Release != "abc123" {
		t.Errorf("config = %+v", c)
	}
	t.Setenv("SENTRY_ENVIRONMENT", "staging")
	if c := ConfigFromEnv(""); c.Environment != "staging" {
		t.Errorf("environment = %q", c.Environment)
	}
}
