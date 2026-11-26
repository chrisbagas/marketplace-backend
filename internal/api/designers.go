package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// PublicDesigner is the storefront view of a creator. It carries public
// fields only: never the linked user, bank details, balances or e-mail.
type PublicDesigner struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	City     string `json:"city"`
	Bio      string `json:"bio"`
	Hue      int    `json:"hue"`
	Avatar   string `json:"avatar"`
	Products int    `json:"products"` // active listings
	Sold     int    `json:"sold"`     // units sold across all of the creator's listings
	Since    int64  `json:"since"`    // epoch ms
}

// getDesigner serves the public creator page header: GET /api/designers/{id}.
func (s *Server) getDesigner(w http.ResponseWriter, r *http.Request) {
	var d PublicDesigner
	err := s.pool.QueryRow(r.Context(), `
		SELECT dr.id, dr.name, dr.city, dr.bio, dr.hue, dr.avatar_uri,
		       COALESCE(agg.products, 0), COALESCE(agg.sold, 0),
		       (extract(epoch FROM dr.created_at) * 1000)::bigint
		FROM designers dr
		LEFT JOIN LATERAL (
		  SELECT count(*) FILTER (WHERE l.active)::int AS products, COALESCE(sum(l.sold), 0)::int AS sold
		  FROM designs d JOIN listings l ON l.design_id = d.id
		  WHERE d.designer_id = dr.id
		) agg ON true
		WHERE dr.id = $1`, r.PathValue("id")).
		Scan(&d.ID, &d.Name, &d.City, &d.Bio, &d.Hue, &d.Avatar, &d.Products, &d.Sold, &d.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Kreator tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"designer": d})
}
