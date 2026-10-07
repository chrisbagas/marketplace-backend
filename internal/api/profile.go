package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Semua handler di file ini dibungkus s.authed — userFrom(r) selalu terisi.

type Profile struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Email            string         `json:"email"`
	Phone            string         `json:"phone"`
	Address          string         `json:"address"`
	City             string         `json:"city"`
	PreferredPayment string         `json:"preferredPayment"`
	PreferredCourier string         `json:"preferredCourier"`
	Settings         map[string]any `json:"settings"`
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	var p Profile
	var settings []byte
	err := s.pool.QueryRow(r.Context(), `
		SELECT id, name, email, phone, address, city, preferred_payment, preferred_courier, settings
		FROM users WHERE id = $1`, userFrom(r).ID).
		Scan(&p.ID, &p.Name, &p.Email, &p.Phone, &p.Address, &p.City, &p.PreferredPayment, &p.PreferredCourier, &settings)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Settings = map[string]any{}
	_ = json.Unmarshal(settings, &p.Settings)
	writeJSON(w, http.StatusOK, map[string]any{"profile": p})
}

func (s *Server) patchProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             *string        `json:"name"`
		Phone            *string        `json:"phone"`
		Address          *string        `json:"address"`
		City             *string        `json:"city"`
		PreferredPayment *string        `json:"preferredPayment"`
		PreferredCourier *string        `json:"preferredCourier"`
		Settings         map[string]any `json:"settings"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ctx := r.Context()
	userID := userFrom(r).ID
	set := func(col string, v *string) error {
		if v == nil {
			return nil
		}
		_, err := s.pool.Exec(ctx, `UPDATE users SET `+col+` = $2 WHERE id = $1`, userID, strings.TrimSpace(*v))
		return err
	}
	for col, v := range map[string]*string{
		"name": body.Name, "phone": body.Phone, "address": body.Address, "city": body.City,
		"preferred_payment": body.PreferredPayment, "preferred_courier": body.PreferredCourier,
	} {
		if err := set(col, v); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.Settings != nil {
		raw, _ := json.Marshal(body.Settings)
		if _, err := s.pool.Exec(ctx, `UPDATE users SET settings = $2 WHERE id = $1`, userID, raw); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.getProfile(w, r)
}

// ---- desain custom pribadi ------------------------------------------------

type UserDesign struct {
	ID        int64   `json:"id"`
	T         int64   `json:"t"`
	Title     string  `json:"title"`
	Type      string  `json:"type"`
	Color     string  `json:"color"`
	URI       string  `json:"uri"`
	WidthCm   float64 `json:"widthCm"`
	OffsetYCm float64 `json:"offsetYCm"`
}

func (s *Server) getUserDesigns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, (extract(epoch FROM created_at)*1000)::bigint, title, product_type_id, color_id, uri, width_cm, offset_y_cm
		FROM user_designs WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50`, userFrom(r).ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	designs := []UserDesign{}
	for rows.Next() {
		var d UserDesign
		if err := rows.Scan(&d.ID, &d.T, &d.Title, &d.Type, &d.Color, &d.URI, &d.WidthCm, &d.OffsetYCm); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		designs = append(designs, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"designs": designs})
}

func (s *Server) postUserDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string  `json:"title"`
		Type      string  `json:"type"`
		Color     string  `json:"color"`
		URI       string  `json:"uri"`
		WidthCm   float64 `json:"widthCm"`
		OffsetYCm float64 `json:"offsetYCm"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "Gambar terlalu besar")
		return
	}
	if body.Title == "" || body.URI == "" {
		errJSON(w, http.StatusBadRequest, "Judul dan gambar wajib diisi")
		return
	}
	if len(body.URI) > 1_500_000 {
		errJSON(w, http.StatusRequestEntityTooLarge, "Gambar terlalu besar — unggah lewat /api/uploads dulu")
		return
	}
	if body.Type == "" {
		body.Type = "kaos"
	}
	if body.Color == "" {
		body.Color = "putih"
	}
	var d UserDesign
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO user_designs (user_id, title, product_type_id, color_id, uri, width_cm, offset_y_cm)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, (extract(epoch FROM created_at)*1000)::bigint, title, product_type_id, color_id, uri, width_cm, offset_y_cm`,
		userFrom(r).ID, body.Title, body.Type, body.Color, body.URI, body.WidthCm, body.OffsetYCm).
		Scan(&d.ID, &d.T, &d.Title, &d.Type, &d.Color, &d.URI, &d.WidthCm, &d.OffsetYCm)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "Jenis produk atau warna tidak valid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": d})
}

func (s *Server) deleteUserDesign(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "ID tidak valid")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM user_designs WHERE id = $1 AND user_id = $2`, id, userFrom(r).ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Desain tidak ditemukan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
