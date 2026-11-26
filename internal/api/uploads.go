package api

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Penyimpanan file lokal untuk prototipe: gambar desain disimpan ke folder
// uploads/ dan disajikan sebagai /uploads/<nama>. Di produksi, ganti isi
// saveUpload dengan put ke object storage (S3 / Cloudflare R2) — pola
// "URL di database" tetap sama.

var allowedMime = map[string]string{
	"image/png":     ".png",
	"image/jpeg":    ".jpg",
	"image/webp":    ".webp",
	"image/svg+xml": ".svg",
}

// POST /api/uploads {name, uri: dataURL} → {url: "/uploads/xxx.png"}
func (s *Server) postUpload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "File terlalu besar (maks ±5 MB)")
		return
	}
	mime, rest, ok := strings.Cut(strings.TrimPrefix(body.URI, "data:"), ";base64,")
	if !ok {
		errJSON(w, http.StatusBadRequest, "Format harus data-URL base64")
		return
	}
	ext, allowed := allowedMime[mime]
	if !allowed {
		errJSON(w, http.StatusBadRequest, "Jenis file harus PNG, JPG, WebP, atau SVG")
		return
	}
	data, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "Data base64 tidak valid")
		return
	}
	if len(data) > 5_000_000 {
		errJSON(w, http.StatusRequestEntityTooLarge, "File terlalu besar (maks 5 MB)")
		return
	}

	name := fmt.Sprintf("d-%06d-%04d%s", rand.IntN(1_000_000), rand.IntN(10000), ext)
	if err := os.WriteFile(filepath.Join(s.uploadDir, name), data, 0o644); err != nil {
		errJSON(w, http.StatusInternalServerError, "Gagal menyimpan file: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/uploads/" + name})
}

// uploadsHandler serves user uploads. SVG is allowed and would otherwise run
// its scripts on the app origin, so every response is locked down: no script
// or subresource may load, and the document is sandboxed (opaque origin, no
// access to cookies). nosniff stops browsers from guessing a more dangerous
// type. The permanent fix is serving uploads from a separate origin.
func uploadsHandler(dir string) http.Handler {
	files := http.StripPrefix("/uploads/", http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		h.Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}
