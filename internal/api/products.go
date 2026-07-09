package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Product matches FullListing on the frontend so the catalog can be served
// from the database (the FE currently ships the same seed statically).
type Product struct {
	ID           string   `json:"id"`
	DesignID     string   `json:"designId"`
	Type         string   `json:"type"`
	Price        int      `json:"price"`
	Sold         int      `json:"sold"`
	Rating       float64  `json:"rating"`
	Badge        string   `json:"badge,omitempty"`
	Title        string   `json:"title"`
	DesignURI    string   `json:"designUri"`
	DesignerName string   `json:"designerName"`
	DesignerID   string   `json:"designerId"`
	TypeLabel    string   `json:"typeLabel"`
	Sizes        []string `json:"sizes"`
	ColorIds     []string `json:"colorIds"`
	Tags         []string `json:"tags"`
	Categories   []string `json:"categories"`
}

const productQuery = `
	SELECT l.id, l.design_id, l.product_type_id, l.price, l.sold, l.rating, COALESCE(l.badge,''),
	       pt.label || ' ' || d.title, d.uri, dr.name, dr.id, pt.label, d.tags,
	       (SELECT COALESCE(array_agg(size ORDER BY sort), '{}') FROM product_type_sizes WHERE product_type_id = pt.id),
	       (SELECT COALESCE(array_agg(color_id ORDER BY sort), '{}') FROM product_type_colors WHERE product_type_id = pt.id),
	       (SELECT COALESCE(array_agg(category_id), '{}') FROM design_categories WHERE design_id = d.id)
	FROM listings l
	JOIN designs d ON d.id = l.design_id
	JOIN designers dr ON dr.id = d.designer_id
	JOIN product_types pt ON pt.id = l.product_type_id
	WHERE l.active`

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.DesignID, &p.Type, &p.Price, &p.Sold, &p.Rating, &p.Badge,
		&p.Title, &p.DesignURI, &p.DesignerName, &p.DesignerID, &p.TypeLabel, &p.Tags,
		&p.Sizes, &p.ColorIds, &p.Categories)
	return p, err
}

func (s *Server) getProducts(w http.ResponseWriter, r *http.Request) {
	query := productQuery
	args := []any{}
	if t := r.URL.Query().Get("type"); t != "" {
		args = append(args, t)
		query += fmt.Sprintf(` AND l.product_type_id = $%d`, len(args))
	}
	if c := r.URL.Query().Get("category"); c != "" {
		args = append(args, c)
		query += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM design_categories dc WHERE dc.design_id = d.id AND dc.category_id = $%d)`, len(args))
	}
	// pencarian: judul produk/desain, nama kreator, atau tag
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		args = append(args, "%"+q+"%")
		n := len(args)
		query += fmt.Sprintf(` AND (pt.label || ' ' || d.title ILIKE $%d OR dr.name ILIKE $%d
			OR EXISTS (SELECT 1 FROM unnest(d.tags) tag WHERE tag ILIKE $%d))`, n, n, n)
	}
	query += ` ORDER BY l.sold DESC`
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	products := []Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		products = append(products, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": products})
}

// getCategories: daftar kategori terkurasi + jumlah produk aktif per kategori.
func (s *Server) getCategories(w http.ResponseWriter, r *http.Request) {
	type Category struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Emoji string `json:"emoji"`
		Count int    `json:"count"`
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT c.id, c.label, c.emoji, COALESCE(n.cnt, 0)
		FROM categories c
		LEFT JOIN (
			SELECT dc.category_id, count(DISTINCT l.id) AS cnt
			FROM design_categories dc
			JOIN listings l ON l.design_id = dc.design_id AND l.active
			GROUP BY dc.category_id
		) n ON n.category_id = c.id
		ORDER BY c.sort`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	cats := []Category{}
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Label, &c.Emoji, &c.Count); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		cats = append(cats, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	p, err := scanProduct(s.pool.QueryRow(r.Context(), productQuery+` AND l.id = $1`, r.PathValue("id")))
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Produk tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"product": p})
}
