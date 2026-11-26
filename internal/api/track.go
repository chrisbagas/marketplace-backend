package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TrackEvent mirrors the frontend's shape: t in epoch milliseconds.
type TrackEvent struct {
	ID      int64  `json:"id"`
	T       int64  `json:"t"`
	Session string `json:"session"`
	Type    string `json:"type"`
	Page    string `json:"page,omitempty"`
	Label   string `json:"label,omitempty"`
	Device  string `json:"device,omitempty"`
	Value   *int64 `json:"value,omitempty"`
}

// Per-IP limits for the unauthenticated analytics writes. Over the limit the
// insert is skipped but the answer stays 200, so beacons never see an error.
const (
	trackLimit   = 300
	vitalsLimit  = 60
	analyticsWin = 15 * time.Minute
	vitalsBatch  = 20 // max samples read from one vitals request
)

func (s *Server) postTrack(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("track:"+clientIP(r), trackLimit, analyticsWin) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	var body struct {
		Type    string `json:"type"`
		Session string `json:"session"`
		Page    string `json:"page"`
		Label   string `json:"label"`
		Device  string `json:"device"`
		Value   *int64 `json:"value"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]bool{"ok": false})
		return
	}
	if body.Type == "" {
		body.Type = "unknown"
	}
	if body.Session == "" {
		body.Session = "anon"
	}
	ctx := r.Context()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO track_events (session_id, type, page, label, device, value) VALUES ($1,$2,$3,$4,$5,$6)`,
		body.Session, body.Type, nilIfEmpty(body.Page), nilIfEmpty(body.Label), nilIfEmpty(body.Device), body.Value); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]bool{"ok": false})
		return
	}
	if body.Type == "search" {
		if term := strings.ToLower(strings.TrimSpace(body.Label)); term != "" {
			_, _ = s.pool.Exec(ctx,
				`INSERT INTO search_terms (term, count) VALUES ($1, 1)
				 ON CONFLICT (term) DO UPDATE SET count = search_terms.count + 1, last_at = now()`, term)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getTrack(w http.ResponseWriter, r *http.Request) {
	events, err := s.recentEvents(r.Context(), 60)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) recentEvents(ctx context.Context, limit int) ([]TrackEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, (extract(epoch FROM created_at)*1000)::bigint, session_id, type,
		       COALESCE(page,''), COALESCE(label,''), COALESCE(device,''), value
		FROM track_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []TrackEvent{}
	for rows.Next() {
		var e TrackEvent
		if err := rows.Scan(&e.ID, &e.T, &e.Session, &e.Type, &e.Page, &e.Label, &e.Device, &e.Value); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Server) postVitals(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("vitals:"+clientIP(r), vitalsLimit, analyticsWin) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": 0})
		return
	}
	var body []struct {
		Name  string   `json:"name"`
		Value *float64 `json:"value"`
		Page  string   `json:"page"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]bool{"ok": false})
		return
	}
	valid := map[string]bool{"LCP": true, "INP": true, "CLS": true, "TTFB": true}
	if len(body) > vitalsBatch {
		body = body[:vitalsBatch]
	}
	received := 0
	for _, v := range body {
		if !valid[v.Name] || v.Value == nil {
			continue
		}
		page := v.Page
		if page == "" {
			page = "?"
		}
		if _, err := s.pool.Exec(r.Context(),
			`INSERT INTO web_vitals (name, value, page) VALUES ($1,$2,$3)`, v.Name, *v.Value, page); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]bool{"ok": false})
			return
		}
		received++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": received})
}

// AnalyticsRetention is how long raw analytics rows are kept. The admin charts
// read the 14-day day_stats table plus today's live events, so nothing shown
// there depends on older raw rows; day_stats and search_terms are never purged.
const AnalyticsRetention = 90 * 24 * time.Hour

// PurgeOldAnalytics deletes track_events, web_vitals and api_metrics rows older
// than the given age and returns how many rows were removed in total.
func (s *Server) PurgeOldAnalytics(ctx context.Context, older time.Duration) (int64, error) {
	var total int64
	for _, table := range []string{"track_events", "web_vitals", "api_metrics"} {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM `+table+` WHERE created_at < now() - make_interval(secs => $1)`, older.Seconds())
		if err != nil {
			return total, fmt.Errorf("%s: %w", table, err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
