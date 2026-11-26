package api

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DesignSubmission matches the frontend shape (t in epoch ms).
type DesignSubmission struct {
	ID         string   `json:"id"`
	T          int64    `json:"t"`
	Title      string   `json:"title"`
	Designer   string   `json:"designer"`
	Type       string   `json:"type"`
	Color      string   `json:"color"`
	Price      int      `json:"price"`
	URI        string   `json:"uri"`
	Status     string   `json:"status"`
	Note       string   `json:"note,omitempty"`
	Tags       []string `json:"tags"`
	Attributes []string `json:"attributes"` // attribute value ids (tema, gaya, acara)
	ListingID  string   `json:"listingId,omitempty"`
}

const submissionCols = `
	SELECT id, (extract(epoch FROM created_at)*1000)::bigint, title, designer_name,
	       product_type_id, color_id, price, uri, status, COALESCE(note,''),
	       tags, attribute_ids, COALESCE(published_listing_id,'')
	FROM design_submissions`

func scanSubmission(row interface{ Scan(...any) error }) (DesignSubmission, error) {
	var d DesignSubmission
	err := row.Scan(&d.ID, &d.T, &d.Title, &d.Designer, &d.Type, &d.Color, &d.Price,
		&d.URI, &d.Status, &d.Note, &d.Tags, &d.Attributes, &d.ListingID)
	return d, err
}

func (s *Server) getDesigns(w http.ResponseWriter, r *http.Request) {
	// admin melihat semua pengajuan; kreator hanya miliknya sendiri
	where := []string{}
	args := []any{}
	if u := userFrom(r); u.Role != "admin" {
		designerID := ""
		if u.Designer != nil {
			designerID = u.Designer.ID
		}
		args = append(args, designerID)
		where = append(where, fmt.Sprintf("designer_id = $%d", len(args)))
	}
	if status := r.URL.Query().Get("status"); status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	query := submissionCols
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	designs := []DesignSubmission{}
	for rows.Next() {
		d, err := scanSubmission(rows)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		designs = append(designs, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"designs": designs})
}

// cleanTags: tag bebas ala hashtag — huruf kecil, tanpa '#', maks 13 tag.
const maxTags = 13

func cleanTags(raw []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range raw {
		t = strings.ToLower(strings.Trim(strings.TrimSpace(t), "#,;. "))
		if t == "" || len(t) > 30 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

func (s *Server) postDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title      string   `json:"title"`
		Designer   string   `json:"designer"`
		Type       string   `json:"type"`
		Color      string   `json:"color"`
		Price      int      `json:"price"`
		URI        string   `json:"uri"`
		Tags       []string `json:"tags"`
		Attributes []string `json:"attributes"`
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
	// kreator selalu mengajukan atas nama profil tokonya sendiri;
	// admin boleh mengisi nama kreator bebas (mis. untuk kurasi titipan)
	u := userFrom(r)
	// karya yang dijual atas nama kreator butuh kontak yang terbukti valid
	if u.Role != "admin" && !u.EmailVerified {
		errJSON(w, http.StatusForbidden, "Verifikasi email kamu dulu sebelum mengajukan desain — cek kotak masuk atau kirim ulang link dari banner di atas")
		return
	}
	var designerID any
	if u.Designer != nil {
		designerID = u.Designer.ID
		body.Designer = u.Designer.Name
	} else if u.Role != "admin" {
		errJSON(w, http.StatusForbidden, "Akun kreator belum punya profil toko")
		return
	}
	if body.Designer == "" {
		body.Designer = u.Name
	}
	if body.Type == "" {
		body.Type = "kaos"
	}
	if body.Color == "" {
		body.Color = "putih"
	}

	ctx := r.Context()
	attrs, err := s.cleanAttributes(ctx, body.Attributes)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	id := fmt.Sprintf("ds-%06d", rand.IntN(1_000_000))
	if designerID == nil { // admin: tautkan ke profil kreator bila namanya dikenal
		var did string
		if err := s.pool.QueryRow(ctx, `SELECT id FROM designers WHERE name = $1`, body.Designer).Scan(&did); err == nil {
			designerID = did
		}
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO design_submissions (id, designer_id, designer_name, title, product_type_id, color_id, price, uri, tags, attribute_ids)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, designerID, body.Designer, body.Title, body.Type, body.Color, body.Price, body.URI,
		cleanTags(body.Tags), attrs); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	d, err := s.loadSubmission(ctx, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": d})
}

// cleanAttributes keeps the attribute values that exist, at most
// attributes.max_per_design per attribute, in the order they were sent.
func (s *Server) cleanAttributes(ctx context.Context, ids []string) ([]string, error) {
	out := []string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT av.id, av.attribute_id, a.max_per_design
		FROM attribute_values av JOIN attributes a ON a.id = av.attribute_id
		WHERE av.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type info struct {
		attr string
		max  int
	}
	known := map[string]info{}
	for rows.Next() {
		var id string
		var in info
		if err := rows.Scan(&id, &in.attr, &in.max); err != nil {
			return nil, err
		}
		known[id] = in
	}
	used := map[string]int{}
	seen := map[string]bool{}
	for _, id := range ids {
		in, ok := known[id]
		if !ok || seen[id] || used[in.attr] >= in.max {
			continue
		}
		seen[id] = true
		used[in.attr]++
		out = append(out, id)
	}
	return out, rows.Err()
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
	ctx := r.Context()
	// status lama dibaca di baris yang sama dengan update-nya (terkunci): email
	// hanya dikirim bila status benar-benar berubah, bukan saat PATCH diulang.
	var prev string
	err := s.pool.QueryRow(ctx, `
		WITH old AS (SELECT id, status FROM design_submissions WHERE id=$1 FOR UPDATE)
		UPDATE design_submissions d SET status=$2, note=$3 FROM old WHERE d.id = old.id
		RETURNING old.status::text`,
		body.ID, body.Status, nilIfEmpty(body.Note)).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Desain tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// disetujui → terbitkan sebagai listing di katalog
	switch body.Status {
	case "disetujui":
		listingID, created, err := s.publishSubmission(ctx, body.ID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "Gagal menerbitkan listing: "+err.Error())
			return
		}
		// email terkirim tepat saat listing terbit (juga bila percobaan pertama gagal lalu diulang)
		if created {
			s.mailDesignReviewed(ctx, body.ID, body.Status, listingID)
		}
	case "ditolak":
		if prev != "ditolak" {
			s.mailDesignReviewed(ctx, body.ID, body.Status, "")
		}
	}
	d, err := s.loadSubmission(ctx, body.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": d})
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "karya"
	}
	return s
}

// publishSubmission membuat design + listing dari pengajuan yang disetujui.
// Idempoten: kalau sudah pernah terbit, kembalikan listing yang sama
// (created=false).
func (s *Server) publishSubmission(ctx context.Context, subID string) (listingID string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	var designerID *string
	var published *string
	var title, ptID, uri string
	var price int
	var tags, cats []string
	if err := tx.QueryRow(ctx, `
		SELECT designer_id, published_listing_id, title, product_type_id, uri, price, tags, attribute_ids
		FROM design_submissions WHERE id=$1 FOR UPDATE`, subID).
		Scan(&designerID, &published, &title, &ptID, &uri, &price, &tags, &cats); err != nil {
		return "", false, err
	}
	if published != nil && *published != "" {
		return *published, false, nil
	}
	if designerID == nil {
		return "", false, nil // kreator tak dikenal — tidak bisa menerima royalti, biarkan manual
	}

	slug := slugify(title)
	designID := fmt.Sprintf("%s-%04d", slug, rand.IntN(10000))
	listingID = fmt.Sprintf("%s-%s-%04d", ptID, slug, rand.IntN(10000))

	if _, err := tx.Exec(ctx,
		`INSERT INTO designs (id, designer_id, title, uri, tags) VALUES ($1,$2,$3,$4,$5)`,
		designID, *designerID, title, uri, tags); err != nil {
		return "", false, err
	}
	for _, c := range cats {
		if _, err := tx.Exec(ctx,
			`INSERT INTO design_attributes (design_id, value_id)
			 SELECT $1, id FROM attribute_values WHERE id=$2 ON CONFLICT DO NOTHING`, designID, c); err != nil {
			return "", false, err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO listings (id, design_id, product_type_id, price, sold, rating) VALUES ($1,$2,$3,$4,0,0)`,
		listingID, designID, ptID, price); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE design_submissions SET published_listing_id=$2 WHERE id=$1`, subID, listingID); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	s.reindexSearch(ctx, listingID)
	return listingID, true, nil
}

func (s *Server) loadSubmission(ctx context.Context, id string) (*DesignSubmission, error) {
	d, err := scanSubmission(s.pool.QueryRow(ctx, submissionCols+` WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	return &d, nil
}
