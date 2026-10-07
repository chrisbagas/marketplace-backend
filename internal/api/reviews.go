package api

import (
	"net/http"
	"strings"
)

type Review struct {
	ID        int64  `json:"id"`
	T         int64  `json:"t"`
	ListingID string `json:"listingId"`
	OrderID   string `json:"orderId"`
	Author    string `json:"author"`
	Rating    int    `json:"rating"`
	Comment   string `json:"comment"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
}

const reviewCols = `
	SELECT id, (extract(epoch FROM created_at)*1000)::bigint, listing_id, order_id,
	       author, rating, comment, status, COALESCE(note,'')
	FROM reviews`

// GET /api/reviews?listing=xxx     → ulasan tayang (disetujui) + ringkasan
// GET /api/reviews?status=review   → antrean moderasi (admin)
func (s *Server) getReviews(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if listing := r.URL.Query().Get("listing"); listing != "" {
		rows, err := s.pool.Query(ctx,
			reviewCols+` WHERE listing_id=$1 AND status='disetujui' ORDER BY created_at DESC LIMIT 20`, listing)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		reviews := []Review{}
		for rows.Next() {
			var rv Review
			if err := rows.Scan(&rv.ID, &rv.T, &rv.ListingID, &rv.OrderID, &rv.Author, &rv.Rating, &rv.Comment, &rv.Status, &rv.Note); err != nil {
				errJSON(w, http.StatusInternalServerError, err.Error())
				return
			}
			reviews = append(reviews, rv)
		}
		var avg float64
		var count int
		_ = s.pool.QueryRow(ctx,
			`SELECT COALESCE(round(avg(rating)::numeric,1),0), count(*) FROM reviews WHERE listing_id=$1 AND status='disetujui'`,
			listing).Scan(&avg, &count)
		writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews, "avg": avg, "count": count})
		return
	}

	status := r.URL.Query().Get("status")
	query := reviewCols + ` ORDER BY created_at DESC LIMIT 100`
	args := []any{}
	if status != "" {
		query = reviewCols + ` WHERE status=$1 ORDER BY created_at DESC LIMIT 100`
		args = append(args, status)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var rv Review
		if err := rows.Scan(&rv.ID, &rv.T, &rv.ListingID, &rv.OrderID, &rv.Author, &rv.Rating, &rv.Comment, &rv.Status, &rv.Note); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		reviews = append(reviews, rv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

// POST /api/reviews — pembeli menilai item dari pesanannya yang sudah selesai.
func (s *Server) postReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OrderID   string `json:"orderId"`
		ListingID string `json:"listingId"`
		Rating    int    `json:"rating"`
		Comment   string `json:"comment"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	body.Comment = strings.TrimSpace(body.Comment)
	if body.OrderID == "" || body.ListingID == "" || body.Rating < 1 || body.Rating > 5 {
		errJSON(w, http.StatusBadRequest, "Rating 1–5 dan pesanan wajib diisi")
		return
	}
	if len(body.Comment) > 1000 {
		errJSON(w, http.StatusBadRequest, "Komentar terlalu panjang (maks 1000 karakter)")
		return
	}

	ctx := r.Context()
	var status, author string
	if err := s.pool.QueryRow(ctx, `SELECT status, cust_name FROM orders WHERE id=$1 AND user_id=$2`, body.OrderID, userFrom(r).ID).
		Scan(&status, &author); err != nil {
		errJSON(w, http.StatusNotFound, "Pesanan tidak ditemukan")
		return
	}
	if status != "selesai" {
		errJSON(w, http.StatusBadRequest, "Ulasan hanya bisa diberikan setelah pesanan selesai")
		return
	}
	var inOrder bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM order_items WHERE order_id=$1 AND listing_id=$2)`,
		body.OrderID, body.ListingID).Scan(&inOrder); err != nil || !inOrder {
		errJSON(w, http.StatusBadRequest, "Produk ini tidak ada di pesanan tersebut")
		return
	}

	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reviews (listing_id, order_id, author, rating, comment)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (order_id, listing_id) DO NOTHING
		RETURNING id`,
		body.ListingID, body.OrderID, author, body.Rating, body.Comment).Scan(&id)
	if err != nil { // no row returned → sudah pernah menilai
		errJSON(w, http.StatusConflict, "Kamu sudah menilai produk ini untuk pesanan tersebut")
		return
	}
	rv, err := s.loadReview(r, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": rv})
}

// PATCH /api/reviews — moderasi admin. Saat disetujui, rating listing
// diperbarui dengan smoothing (rating lama berbobot 50 penilaian) supaya
// satu ulasan baru tidak menjungkirbalikkan rating produk lama.
func (s *Server) patchReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	if body.ID == 0 || (body.Status != "disetujui" && body.Status != "ditolak") {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	ctx := r.Context()
	var listingID string
	if err := s.pool.QueryRow(ctx,
		`UPDATE reviews SET status=$2, note=$3 WHERE id=$1 RETURNING listing_id`,
		body.ID, body.Status, nilIfEmpty(body.Note)).Scan(&listingID); err != nil {
		errJSON(w, http.StatusNotFound, "Ulasan tidak ditemukan")
		return
	}
	if body.Status == "disetujui" {
		_, _ = s.pool.Exec(ctx, `
			UPDATE listings l
			SET rating = round(((l.rating*50 + s.sum) / (50 + s.cnt))::numeric, 1)
			FROM (SELECT COALESCE(sum(rating),0) AS sum, count(*) AS cnt
			      FROM reviews WHERE listing_id=$1 AND status='disetujui') s
			WHERE l.id=$1 AND s.cnt > 0`, listingID)
	}
	rv, err := s.loadReview(r, body.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": rv})
}

func (s *Server) loadReview(r *http.Request, id int64) (*Review, error) {
	var rv Review
	err := s.pool.QueryRow(r.Context(), reviewCols+` WHERE id=$1`, id).
		Scan(&rv.ID, &rv.T, &rv.ListingID, &rv.OrderID, &rv.Author, &rv.Rating, &rv.Comment, &rv.Status, &rv.Note)
	if err != nil {
		return nil, err
	}
	return &rv, nil
}
