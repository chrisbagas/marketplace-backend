package api

import (
	"context"
	"net/http"
	"strings"
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

func (s *Server) postTrack(w http.ResponseWriter, r *http.Request) {
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
				 ON CONFLICT (term) DO UPDATE SET count = search_terms.count + 1`, term)
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

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
