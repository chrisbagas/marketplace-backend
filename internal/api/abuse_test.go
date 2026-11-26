package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tests for the unauthenticated-write limits and the analytics retention job.
// The DB tests need TEST_DATABASE_URL (see google_test.go).

func abuseServer(t *testing.T) (*Server, *pgxpool.Pool) {
	pool := testPool(t)
	return NewServer(pool, os.TempDir(), AuthConfig{AppURL: "http://app.test"}), pool
}

// fromIP sends one request as the given client IP and returns the status code.
func fromIP(s *Server, ip, method, path, body string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

func TestTrackRateLimited(t *testing.T) {
	s, pool := abuseServer(t)
	ctx := context.Background()
	session := fmt.Sprintf("abuse-%d", time.Now().UnixNano())
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM track_events WHERE session_id = $1`, session) })

	body := fmt.Sprintf(`{"type":"page_view","session":%q,"page":"/"}`, session)
	for i := range 400 {
		if code := fromIP(s, "198.51.100.7", "POST", "/api/track", body); code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 even over the limit", i+1, code)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM track_events WHERE session_id = $1`, session).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != trackLimit {
		t.Fatalf("rows inserted = %d, want %d", n, trackLimit)
	}

	// another client is not affected
	if code := fromIP(s, "198.51.100.8", "POST", "/api/track", body); code != http.StatusOK {
		t.Fatalf("other IP: status %d", code)
	}
	pool.QueryRow(ctx, `SELECT count(*) FROM track_events WHERE session_id = $1`, session).Scan(&n)
	if n != trackLimit+1 {
		t.Fatalf("rows after other IP = %d, want %d", n, trackLimit+1)
	}
}

func TestVitalsRateLimited(t *testing.T) {
	s, pool := abuseServer(t)
	ctx := context.Background()
	page := fmt.Sprintf("/abuse-%d", time.Now().UnixNano())
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM web_vitals WHERE page = $1`, page) })

	body := fmt.Sprintf(`[{"name":"LCP","value":1200,"page":%q}]`, page)
	for i := range vitalsLimit + 20 {
		if code := fromIP(s, "198.51.100.9", "POST", "/api/vitals", body); code != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, code)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM web_vitals WHERE page = $1`, page).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != vitalsLimit {
		t.Fatalf("rows inserted = %d, want %d", n, vitalsLimit)
	}
}

func TestSearchRateLimited(t *testing.T) {
	s, _ := abuseServer(t)
	for i := 1; i <= 120; i++ {
		if code := fromIP(s, "198.51.100.10", "GET", "/api/search", ""); code != http.StatusOK {
			t.Fatalf("search %d: status %d, want 200", i, code)
		}
	}
	if code := fromIP(s, "198.51.100.10", "GET", "/api/search", ""); code != http.StatusTooManyRequests {
		t.Fatalf("search 121: status %d, want 429", code)
	}
	if code := fromIP(s, "198.51.100.11", "GET", "/api/search", ""); code != http.StatusOK {
		t.Fatalf("other IP: status %d, want 200", code)
	}
}

func TestSuggestRateLimited(t *testing.T) {
	s, _ := abuseServer(t)
	for i := 1; i <= 600; i++ {
		if code := fromIP(s, "198.51.100.12", "GET", "/api/search/suggest", ""); code != http.StatusOK {
			t.Fatalf("suggest %d: status %d, want 200", i, code)
		}
	}
	if code := fromIP(s, "198.51.100.12", "GET", "/api/search/suggest", ""); code != http.StatusTooManyRequests {
		t.Fatalf("suggest 601: status %d, want 429", code)
	}
}

func TestPurgeOldAnalytics(t *testing.T) {
	s, pool := abuseServer(t)
	ctx := context.Background()
	tag := fmt.Sprintf("purge-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM track_events WHERE session_id = $1`, tag)
		pool.Exec(ctx, `DELETE FROM web_vitals WHERE page = $1`, tag)
		pool.Exec(ctx, `DELETE FROM api_metrics WHERE route = $1`, tag)
	})
	for _, age := range []string{"91 days", "30 days"} {
		for _, q := range []string{
			`INSERT INTO track_events (session_id, type, created_at) VALUES ($1,'page_view', now() - $2::interval)`,
			`INSERT INTO web_vitals (name, value, page, created_at) VALUES ('LCP', 1000, $1, now() - $2::interval)`,
			`INSERT INTO api_metrics (route, ms, created_at) VALUES ($1, 5, now() - $2::interval)`,
		} {
			if _, err := pool.Exec(ctx, q, tag, age); err != nil {
				t.Fatal(err)
			}
		}
	}
	var statsBefore int
	pool.QueryRow(ctx, `SELECT count(*) FROM day_stats`).Scan(&statsBefore)

	if _, err := s.PurgeOldAnalytics(ctx, AnalyticsRetention); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ table, col string }{
		{"track_events", "session_id"}, {"web_vitals", "page"}, {"api_metrics", "route"},
	} {
		var old, recent int
		pool.QueryRow(ctx, `SELECT count(*) FROM `+c.table+` WHERE `+c.col+` = $1 AND created_at < now() - interval '90 days'`, tag).Scan(&old)
		pool.QueryRow(ctx, `SELECT count(*) FROM `+c.table+` WHERE `+c.col+` = $1 AND created_at >= now() - interval '90 days'`, tag).Scan(&recent)
		if old != 0 || recent != 1 {
			t.Errorf("%s: old=%d recent=%d, want 0 and 1", c.table, old, recent)
		}
	}
	var statsAfter int
	pool.QueryRow(ctx, `SELECT count(*) FROM day_stats`).Scan(&statsAfter)
	if statsAfter != statsBefore {
		t.Errorf("day_stats rows %d → %d, must be untouched", statsBefore, statsAfter)
	}
}

func TestRateLimiterPrunesPerKeyWindow(t *testing.T) {
	l := newRateLimiter()
	if !l.allow("short", 1, time.Millisecond) || !l.allow("long", 1, time.Hour) {
		t.Fatal("first hits must be allowed")
	}
	time.Sleep(5 * time.Millisecond)
	l.mu.Lock()
	l.prune(time.Now().Add(2 * time.Minute)) // force a scan
	_, shortLeft := l.hits["short"]
	_, longLeft := l.hits["long"]
	l.mu.Unlock()
	if shortLeft {
		t.Error("expired key was not pruned")
	}
	if !longLeft {
		t.Error("key still inside its own window was pruned")
	}
	if l.allow("long", 1, time.Hour) {
		t.Error("limit forgotten after prune")
	}
}
