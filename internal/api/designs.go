package api

import (
	"fmt"
	"math/rand/v2"
	"net/http"
)

// DesignSubmission matches the frontend shape (t in epoch ms).
type DesignSubmission struct {
	ID       string `json:"id"`
	T        int64  `json:"t"`
	Title    string `json:"title"`
	Designer string `json:"designer"`
	Type     string `json:"type"`
	Color    string `json:"color"`
	Price    int    `json:"price"`
	URI      string `json:"uri"`
	Status   string `json:"status"`
	Note     string `json:"note,omitempty"`
}

const submissionCols = `
	SELECT id, (extract(epoch FROM created_at)*1000)::bigint, title, designer_name,
	       product_type_id, color_id, price, uri, status, COALESCE(note,'')
	FROM design_submissions`

func (s *Server) getDesigns(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	query := submissionCols + ` ORDER BY created_at DESC LIMIT 100`
	args := []any{}
	if status != "" {
		query = submissionCols + ` WHERE status = $1 ORDER BY created_at DESC LIMIT 100`
		args = append(args, status)
	}
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	designs := []DesignSubmission{}
	for rows.Next() {
		var d DesignSubmission
		if err := rows.Scan(&d.ID, &d.T, &d.Title, &d.Designer, &d.Type, &d.Color, &d.Price, &d.URI, &d.Status, &d.Note); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		designs = append(designs, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"designs": designs})
}

func (s *Server) postDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		Designer string `json:"designer"`
		Type     string `json:"type"`
		Color    string `json:"color"`
		Price    int    `json:"price"`
		URI      string `json:"uri"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "Ukuran gambar terlalu besar (maks ±1 MB)")
		return
	}
	if body.Title == "" || body.URI == "" {
		errJSON(w, http.StatusBadRequest, "Judul dan gambar desain wajib diisi")
		return
	}
	if len(body.URI) > 1_500_000 {
		errJSON(w, http.StatusRequestEntityTooLarge, "Ukuran gambar terlalu besar (maks ±1 MB)")
		return
	}
	if body.Designer == "" {
		body.Designer = "Kreator Demo"
	}
	if body.Type == "" {
		body.Type = "kaos"
	}
	if body.Color == "" {
		body.Color = "putih"
	}

	ctx := r.Context()
	id := fmt.Sprintf("ds-%06d", rand.IntN(1_000_000))
	// tautkan ke profil kreator bila namanya dikenal
	var designerID any
	var did string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM designers WHERE name = $1`, body.Designer).Scan(&did); err == nil {
		designerID = did
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO design_submissions (id, designer_id, designer_name, title, product_type_id, color_id, price, uri)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, designerID, body.Designer, body.Title, body.Type, body.Color, body.Price, body.URI); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	d, err := s.loadSubmission(r, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": d})
}

func (s *Server) patchDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	if body.ID == "" || (body.Status != "disetujui" && body.Status != "ditolak") {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	tag, err := s.pool.Exec(r.Context(),
		`UPDATE design_submissions SET status=$2, note=$3 WHERE id=$1`,
		body.ID, body.Status, nilIfEmpty(body.Note))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Desain tidak ditemukan")
		return
	}
	d, err := s.loadSubmission(r, body.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": d})
}

func (s *Server) loadSubmission(r *http.Request, id string) (*DesignSubmission, error) {
	var d DesignSubmission
	err := s.pool.QueryRow(r.Context(), submissionCols+` WHERE id = $1`, id).
		Scan(&d.ID, &d.T, &d.Title, &d.Designer, &d.Type, &d.Color, &d.Price, &d.URI, &d.Status, &d.Note)
	if err != nil {
		return nil, err
	}
	return &d, nil
}
