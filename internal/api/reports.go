package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/mail"
)

// Design reports: anyone can flag a listing (copyright / trademark /
// inappropriate content); an admin reviews and either deactivates the listing
// (creator is emailed) or rejects the report.

const (
	reportDescMin     = 10
	reportDescMax     = 2000
	reportEvidenceMax = 500
	reportNoteMax     = 500
	reportsPerHour    = 5 // per IP
)

var reportReasons = map[string]string{
	"hak-cipta":           "pelanggaran hak cipta",
	"merek-dagang":        "pelanggaran merek dagang",
	"konten-tidak-pantas": "konten tidak pantas",
	"lainnya":             "alasan lain",
}

var reportStatuses = []string{"baru", "ditinjau", "ditindak", "ditolak"}

type Report struct {
	ID            int64  `json:"id"`
	ListingID     string `json:"listingId"`
	ListingTitle  string `json:"listingTitle"`
	ListingActive bool   `json:"listingActive"`
	DesignID      string `json:"designId"`
	DesignURI     string `json:"designUri"`
	DesignerID    string `json:"designerId"`
	DesignerName  string `json:"designerName"`
	Reason        string `json:"reason"`
	Email         string `json:"email"`
	Description   string `json:"description"`
	EvidenceURL   string `json:"evidenceUrl,omitempty"`
	Status        string `json:"status"`
	AdminNote     string `json:"adminNote,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

const reportCols = `
	r.id, r.listing_id, COALESCE(l.title_override, pt.label || ' ' || d.title), l.active,
	r.design_id, d.uri, dr.id, dr.name, r.reason, r.email, r.description,
	COALESCE(r.evidence_url,''), r.status, COALESCE(r.admin_note,''),
	(extract(epoch FROM r.created_at)*1000)::bigint,
	(extract(epoch FROM r.updated_at)*1000)::bigint`

const reportFrom = `
	FROM design_reports r
	JOIN listings l       ON l.id = r.listing_id
	JOIN product_types pt ON pt.id = l.product_type_id
	JOIN designs d        ON d.id = r.design_id
	JOIN designers dr     ON dr.id = d.designer_id`

func scanReport(row pgx.Row) (Report, error) {
	var r Report
	err := row.Scan(&r.ID, &r.ListingID, &r.ListingTitle, &r.ListingActive, &r.DesignID, &r.DesignURI,
		&r.DesignerID, &r.DesignerName, &r.Reason, &r.Email, &r.Description, &r.EvidenceURL,
		&r.Status, &r.AdminNote, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// POST /api/reports — public; rate limited per IP.
func (s *Server) postReport(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("report-ip:"+clientIP(r), reportsPerHour, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, "Terlalu banyak laporan dari perangkat ini. Coba lagi nanti.")
		return
	}
	var body struct {
		ListingID   string `json:"listingId"`
		Reason      string `json:"reason"`
		Email       string `json:"email"`
		Description string `json:"description"`
		EvidenceURL string `json:"evidenceUrl"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	u := userFrom(r)
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" && u != nil {
		email = strings.ToLower(u.Email)
	}
	desc := strings.TrimSpace(body.Description)
	evidence := strings.TrimSpace(body.EvidenceURL)
	switch {
	case body.ListingID == "" || len(body.ListingID) > 100:
		errJSON(w, http.StatusBadRequest, "Produk yang dilaporkan tidak valid")
		return
	case reportReasons[body.Reason] == "":
		errJSON(w, http.StatusBadRequest, "Pilih alasan laporan")
		return
	case !validEmail(email):
		errJSON(w, http.StatusBadRequest, "Email tidak valid")
		return
	case utf8.RuneCountInString(desc) < reportDescMin:
		errJSON(w, http.StatusBadRequest, "Jelaskan laporanmu minimal 10 karakter")
		return
	case utf8.RuneCountInString(desc) > reportDescMax:
		errJSON(w, http.StatusBadRequest, "Penjelasan maksimal 2000 karakter")
		return
	case evidence != "" && !validEvidenceURL(evidence):
		errJSON(w, http.StatusBadRequest, "Link bukti harus diawali http:// atau https://")
		return
	}

	var designID string
	err := s.pool.QueryRow(r.Context(), `SELECT design_id FROM listings WHERE id = $1`, body.ListingID).Scan(&designID)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Produk tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var reporter any
	if u != nil {
		reporter = u.ID
	}
	var id int64
	if err := s.pool.QueryRow(r.Context(), `
		INSERT INTO design_reports (listing_id, design_id, reporter_user_id, reason, email, description, evidence_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		body.ListingID, designID, reporter, body.Reason, email, desc, nilIfEmpty(evidence)).Scan(&id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
}

func validEvidenceURL(raw string) bool {
	if len(raw) > reportEvidenceMax {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// GET /api/reports?status=&limit= — admin; also returns the count per status.
func (s *Server) getReports(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	where, args := "true", []any{}
	if status != "" {
		known := false
		for _, st := range reportStatuses {
			known = known || st == status
		}
		if !known {
			errJSON(w, http.StatusBadRequest, "Status tidak dikenal")
			return
		}
		where, args = "r.status = $1", []any{status}
	}
	// ?limit=0 returns only the counts (cheap enough for the admin tab badge)
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n >= 0 && n < limit {
		limit = n
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+reportCols+reportFrom+`
		WHERE `+where+` ORDER BY r.created_at DESC, r.id DESC LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	list := []Report{}
	for rows.Next() {
		rep, err := scanReport(rows)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		list = append(list, rep)
	}
	if err := rows.Err(); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts := map[string]int{}
	crow, err := s.pool.Query(r.Context(), `SELECT status, count(*)::int FROM design_reports GROUP BY status`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer crow.Close()
	for crow.Next() {
		var st string
		var n int
		if crow.Scan(&st, &n) == nil {
			counts[st] = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": list, "counts": counts})
}

// PATCH /api/reports/{id} — admin:
//
//	{action:"review"}                       — baru → ditinjau
//	{action:"takedown", note?}              — deactivate the listing, close every open report on it
//	                                          as ditindak and email the creator
//	{action:"reject", note}                 — baru/ditinjau → ditolak (note required)
func (s *Server) patchReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusNotFound, "Laporan tidak ditemukan")
		return
	}
	var body struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > reportNoteMax {
		errJSON(w, http.StatusBadRequest, "Catatan maksimal 500 karakter")
		return
	}
	ctx := r.Context()

	var listingID, status, reason string
	err = s.pool.QueryRow(ctx, `SELECT listing_id, status, reason FROM design_reports WHERE id = $1`, id).Scan(&listingID, &status, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Laporan tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch body.Action {
	case "review":
		if status != "baru" {
			errJSON(w, http.StatusConflict, "Hanya laporan baru yang bisa ditandai ditinjau")
			return
		}
		_, err = s.pool.Exec(ctx, `UPDATE design_reports SET status='ditinjau', updated_at=now() WHERE id=$1`, id)
	case "reject":
		if status == "ditindak" || status == "ditolak" {
			errJSON(w, http.StatusConflict, "Laporan ini sudah selesai diproses")
			return
		}
		if note == "" {
			errJSON(w, http.StatusBadRequest, "Tulis alasan penolakan")
			return
		}
		_, err = s.pool.Exec(ctx, `UPDATE design_reports SET status='ditolak', admin_note=$2, updated_at=now() WHERE id=$1`, id, note)
	case "takedown":
		if status == "ditindak" || status == "ditolak" {
			errJSON(w, http.StatusConflict, "Laporan ini sudah selesai diproses")
			return
		}
		err = s.takedownReport(ctx, id, listingID, reason, note)
	default:
		errJSON(w, http.StatusBadRequest, "Aksi tidak dikenal")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	rep, err := scanReport(s.pool.QueryRow(ctx, `SELECT `+reportCols+reportFrom+` WHERE r.id = $1`, id))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": rep})
}

// takedownReport deactivates the reported listing (same path as a creator
// switching it off, so search is reindexed), closes all open reports on it and
// emails the creator.
func (s *Server) takedownReport(ctx context.Context, reportID int64, listingID, reason, note string) error {
	if err := s.setListingActive(ctx, listingID, false); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE design_reports SET status='ditindak', updated_at=now(),
		       admin_note = CASE WHEN id = $2 THEN NULLIF($3,'') ELSE admin_note END
		WHERE listing_id = $1 AND status IN ('baru','ditinjau')`, listingID, reportID, note); err != nil {
		return err
	}
	var title, name string
	var email *string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(l.title_override, pt.label || ' ' || d.title), dr.name, u.email
		FROM listings l
		JOIN product_types pt ON pt.id = l.product_type_id
		JOIN designs d        ON d.id = l.design_id
		JOIN designers dr     ON dr.id = d.designer_id
		LEFT JOIN users u     ON u.id = dr.user_id
		WHERE l.id = $1`, listingID).Scan(&title, &name, &email)
	if err != nil {
		return err
	}
	if email != nil && *email != "" {
		s.sendMail(mail.ListingTakenDown(*email, name, title, reportReasons[reason], note, s.auth.AppURL+"/designer"))
	}
	return nil
}
