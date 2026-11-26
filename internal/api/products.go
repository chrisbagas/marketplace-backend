package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Product matches FullListing on the frontend so the catalog can be served
// from the database (the FE currently ships the same seed statically).
type Product struct {
	ID             string   `json:"id"`
	DesignID       string   `json:"designId"`
	Type           string   `json:"type"`
	Price          int      `json:"price"`
	Sold           int      `json:"sold"`
	Rating         float64  `json:"rating"`
	Badge          string   `json:"badge,omitempty"`
	Title          string   `json:"title"`
	DesignURI      string   `json:"designUri"`
	DesignerName   string   `json:"designerName"`
	DesignerID     string   `json:"designerId"`
	DesignerAvatar string   `json:"designerAvatar"`
	TypeLabel      string   `json:"typeLabel"`
	Sizes          []string `json:"sizes"`
	ColorIds       []string `json:"colorIds"`
	Tags           []string `json:"tags"`
	Category       string   `json:"category"`   // product category id (Pakaian, ...)
	Attributes     []string `json:"attributes"` // attribute value ids (tema, gaya, acara)
	Active         bool     `json:"active"`
}

// effectiveColors: a listing's own colour subset, or every colour of its product type.
const effectiveColors = `CASE WHEN l.color_ids IS NOT NULL AND cardinality(l.color_ids) > 0 THEN l.color_ids
	            ELSE (SELECT COALESCE(array_agg(color_id ORDER BY sort), '{}') FROM product_type_colors WHERE product_type_id = pt.id)
	       END`

const productCols = `
	SELECT l.id, l.design_id, l.product_type_id, l.price, l.sold, l.rating, COALESCE(l.badge,''),
	       COALESCE(l.title_override, pt.label || ' ' || d.title), d.uri, dr.name, dr.id, dr.avatar_uri, pt.label, d.tags,
	       (SELECT COALESCE(array_agg(size ORDER BY sort), '{}') FROM product_type_sizes WHERE product_type_id = pt.id),
	       ` + effectiveColors + `,
	       COALESCE(pt.category_id, ''),
	       (SELECT COALESCE(array_agg(value_id), '{}') FROM design_attributes WHERE design_id = d.id),
	       l.active`

const productJoins = `
	FROM listings l
	JOIN designs d ON d.id = l.design_id
	JOIN designers dr ON dr.id = d.designer_id
	JOIN product_types pt ON pt.id = l.product_type_id`

const productQuery = productCols + productJoins + `
	WHERE true`

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.DesignID, &p.Type, &p.Price, &p.Sold, &p.Rating, &p.Badge,
		&p.Title, &p.DesignURI, &p.DesignerName, &p.DesignerID, &p.DesignerAvatar, &p.TypeLabel, &p.Tags,
		&p.Sizes, &p.ColorIds, &p.Category, &p.Attributes, &p.Active)
	return p, err
}

// getProducts lists listings unpaged, best sellers first: the home page and
// the creator's "Kelola produk". The storefront catalog uses GET /api/search.
func (s *Server) getProducts(w http.ResponseWriter, r *http.Request) {
	query := productQuery
	args := []any{}
	// tanpa filter designer: katalog publik hanya listing aktif;
	// dengan designer: semua listing miliknya (utk dashboard kelola produk)
	if dsg := r.URL.Query().Get("designer"); dsg != "" {
		args = append(args, dsg)
		query += fmt.Sprintf(` AND dr.id = $%d`, len(args))
	} else {
		query += ` AND l.active`
	}
	if t := r.URL.Query().Get("type"); t != "" {
		args = append(args, t)
		query += fmt.Sprintf(` AND l.product_type_id = $%d`, len(args))
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
