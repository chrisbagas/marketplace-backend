package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PATCH /api/listings/{id} — kreator mengubah listing miliknya:
// nama tampilan, harga (tidak boleh di bawah harga dasar produksi),
// subset warna, dan aktif/nonaktif. Desain tidak bisa diubah dari sini.
func (s *Server) patchListing(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title    *string   `json:"title"`
		Price    *int      `json:"price"`
		ColorIds *[]string `json:"colorIds"`
		Active   *bool     `json:"active"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}

	ctx := r.Context()
	var ptID string
	var basePrice int
	err := s.pool.QueryRow(ctx, `
		SELECT l.product_type_id, pt.base_price
		FROM listings l JOIN product_types pt ON pt.id = l.product_type_id
		WHERE l.id = $1`, id).Scan(&ptID, &basePrice)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Produk tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if body.Price != nil {
		if *body.Price < basePrice {
			errJSON(w, http.StatusBadRequest, "Harga tidak boleh di bawah harga dasar produksi")
			return
		}
		if *body.Price > 10_000_000 {
			errJSON(w, http.StatusBadRequest, "Harga terlalu tinggi")
			return
		}
		if _, err := s.pool.Exec(ctx, `UPDATE listings SET price=$2 WHERE id=$1`, id, *body.Price); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if len(title) > 80 {
			errJSON(w, http.StatusBadRequest, "Nama produk maksimal 80 karakter")
			return
		}
		// nama kosong = kembali ke nama otomatis "<jenis> <judul desain>"
		if _, err := s.pool.Exec(ctx, `UPDATE listings SET title_override=$2 WHERE id=$1`, id, nilIfEmpty(title)); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if body.ColorIds != nil {
		// warna harus subset dari warna yang tersedia untuk jenis produknya
		valid := map[string]bool{}
		rows, err := s.pool.Query(ctx, `SELECT color_id FROM product_type_colors WHERE product_type_id=$1`, ptID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				valid[c] = true
			}
		}
		rows.Close()
		clean := []string{}
		for _, c := range *body.ColorIds {
			if valid[c] {
				clean = append(clean, c)
			}
		}
		// kosong = tampilkan semua warna jenis produk (reset)
		var val any
		if len(clean) > 0 {
			val = clean
		}
		if _, err := s.pool.Exec(ctx, `UPDATE listings SET color_ids=$2 WHERE id=$1`, id, val); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if body.Active != nil {
		if _, err := s.pool.Exec(ctx, `UPDATE listings SET active=$2 WHERE id=$1`, id, *body.Active); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	p, err := scanProduct(s.pool.QueryRow(ctx, productQuery+` AND l.id = $1`, id))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"product": p})
}
