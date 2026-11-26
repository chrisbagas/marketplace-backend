package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"karyakita/api/internal/db"
)

// Admin side of search: what shoppers look for (and don't find), the synonym
// list, and the attribute values creators can pick.

// reindexSearch refreshes the search index for the given listings (none = all).
// A failure is logged, not returned: the change itself already succeeded and
// the index is rebuilt at the next startup anyway.
func (s *Server) reindexSearch(ctx context.Context, listingIDs ...string) {
	var ids []string
	if len(listingIDs) > 0 {
		ids = listingIDs
	}
	if err := db.ReindexSearch(ctx, s.pool, ids); err != nil {
		log.Printf("indeks pencarian: %v", err)
	}
}

type SearchTerm struct {
	Term    string `json:"term"`
	Count   int    `json:"count"`
	Results *int   `json:"results,omitempty"` // nil = never searched through the catalog
}

type Synonym struct {
	ID    int64    `json:"id"`
	Terms []string `json:"terms"`
}

func (s *Server) getSearchInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	terms := func(where string) ([]SearchTerm, error) {
		rows, err := s.pool.Query(ctx, `SELECT term, count, results FROM search_terms WHERE `+where+` ORDER BY count DESC, last_at DESC LIMIT 15`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []SearchTerm{}
		for rows.Next() {
			var t SearchTerm
			if err := rows.Scan(&t.Term, &t.Count, &t.Results); err != nil {
				return nil, err
			}
			list = append(list, t)
		}
		return list, rows.Err()
	}
	top, err := terms(`count > 0`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	empty, err := terms(`results = 0`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id, terms FROM search_synonyms ORDER BY terms[1]`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	synonyms := []Synonym{}
	for rows.Next() {
		var sy Synonym
		if err := rows.Scan(&sy.ID, &sy.Terms); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		synonyms = append(synonyms, sy)
	}
	writeJSON(w, http.StatusOK, map[string]any{"top": top, "noResults": empty, "synonyms": synonyms})
}

// postSynonym adds a group of words that mean the same thing. Each term must
// be a single word: the search matches word by word.
func (s *Server) postSynonym(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Terms []string `json:"terms"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	seen := map[string]bool{}
	terms := []string{}
	for _, t := range body.Terms {
		words := searchTokens(t)
		if len(words) == 0 {
			continue
		}
		if len(words) > 1 {
			errJSON(w, http.StatusBadRequest, "Sinonim harus satu kata: “"+strings.TrimSpace(t)+"”")
			return
		}
		if !seen[words[0]] {
			seen[words[0]] = true
			terms = append(terms, words[0])
		}
	}
	if len(terms) < 2 || len(terms) > 12 {
		errJSON(w, http.StatusBadRequest, "Isi 2–12 kata yang artinya sama")
		return
	}
	sy := Synonym{Terms: terms}
	if err := s.pool.QueryRow(r.Context(), `INSERT INTO search_synonyms (terms) VALUES ($1) RETURNING id`, terms).Scan(&sy.ID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"synonym": sy})
}

func (s *Server) deleteSynonym(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM search_synonyms WHERE id = $1`, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Sinonim tidak ditemukan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func cleanLabel(v string) string { return strings.Join(strings.Fields(v), " ") }

// postAttributeValue adds an option to an attribute, e.g. a new "Tema".
func (s *Server) postAttributeValue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
		Emoji string `json:"emoji"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	label := cleanLabel(body.Label)
	if label == "" || len(label) > 40 || len(body.Emoji) > 16 {
		errJSON(w, http.StatusBadRequest, "Nama wajib diisi (maks 40 karakter)")
		return
	}
	ctx := r.Context()
	attr := r.PathValue("id")
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributes WHERE id = $1)`, attr).Scan(&exists); err != nil || !exists {
		errJSON(w, http.StatusNotFound, "Atribut tidak ditemukan")
		return
	}
	v := FacetValue{ID: slugify(label), Label: label, Emoji: strings.TrimSpace(body.Emoji)}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO attribute_values (id, attribute_id, label, emoji, sort)
		SELECT $1, $2, $3, $4, COALESCE(max(sort), 0) + 1 FROM attribute_values WHERE attribute_id = $2
		ON CONFLICT DO NOTHING`, v.ID, attr, v.Label, v.Emoji)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusConflict, "Nama itu sudah dipakai")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"value": v})
}

// patchAttributeValue renames an option or changes its emoji. The id stays,
// so designs and shared links keep working.
func (s *Server) patchAttributeValue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label *string `json:"label"`
		Emoji *string `json:"emoji"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if body.Label != nil {
		label := cleanLabel(*body.Label)
		if label == "" || len(label) > 40 {
			errJSON(w, http.StatusBadRequest, "Nama wajib diisi (maks 40 karakter)")
			return
		}
		tag, err := s.pool.Exec(ctx, `
			UPDATE attribute_values av SET label = $2 WHERE id = $1 AND NOT EXISTS (
				SELECT 1 FROM attribute_values o WHERE o.attribute_id = av.attribute_id AND o.label = $2 AND o.id <> av.id)`, id, label)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tag.RowsAffected() == 0 {
			errJSON(w, http.StatusConflict, "Tidak ditemukan, atau nama itu sudah dipakai")
			return
		}
	}
	if body.Emoji != nil && len(*body.Emoji) <= 16 {
		if _, err := s.pool.Exec(ctx, `UPDATE attribute_values SET emoji = $2 WHERE id = $1`, id, strings.TrimSpace(*body.Emoji)); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.reindexSearch(ctx) // the label is part of every tagged listing's search document
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteAttributeValue removes an option and untags every design that had it.
func (s *Server) deleteAttributeValue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tag, err := s.pool.Exec(ctx, `DELETE FROM attribute_values WHERE id = $1`, r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Tidak ditemukan")
		return
	}
	s.reindexSearch(ctx)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
