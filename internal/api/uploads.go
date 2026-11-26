package api

import (
	"context"
	"errors"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/files"
	"karyakita/api/internal/s3"
)

// File uploads go straight from the browser to S3-compatible storage (R2 in
// production, a local S3 server in dev):
//
//	POST /api/uploads/presign  {mime, size, purpose} → {key, uploadUrl, headers}
//	browser: PUT uploadUrl with the given headers
//	POST /api/uploads/complete {key}                → {url, widthPx, heightPx}
//
// The server never trusts the client's claim: on complete it HEADs the object,
// sniffs the first bytes and reads the pixel size; objects that fail are deleted.

const presignTTL = 10 * time.Minute

// hostedFilePrefix is FILES_PUBLIC_URL + "/" once storage is configured. The
// checkout reads it to accept only our own files as custom print designs.
var hostedFilePrefix string

// SetFiles enables uploads (nil = disabled, endpoints answer 503).
func (s *Server) SetFiles(c *s3.Client) {
	s.files = c
	hostedFilePrefix = ""
	if c != nil {
		hostedFilePrefix = c.PublicBase() + "/"
	}
}

// isHostedFile reports whether uri points at a file in our bucket.
func isHostedFile(uri string) bool {
	if hostedFilePrefix == "" {
		return false
	}
	key, ok := strings.CutPrefix(uri, hostedFilePrefix)
	if !ok {
		return false
	}
	_, ok = files.ParseKey(key)
	return ok
}

// checkPresign validates a presign request; it returns an HTTP status and an
// Indonesian message when the request must be refused.
func checkPresign(u *User, fileMime string, size int64, purpose string) (int, string) {
	roles, known := files.Purposes[purpose]
	if !known {
		return http.StatusBadRequest, "Tujuan unggahan tidak dikenal"
	}
	if roles != nil && !hasRole(u, roles...) {
		return http.StatusForbidden, "Akun kamu tidak boleh mengunggah ke sini"
	}
	switch {
	case files.Raster(fileMime):
	case fileMime == "image/svg+xml":
		if !hasRole(u, "admin") {
			return http.StatusForbidden, "File SVG hanya boleh diunggah admin — gunakan PNG, JPG, atau WebP"
		}
	default:
		return http.StatusBadRequest, "Jenis file harus PNG, JPG, atau WebP"
	}
	limit, limitText := int64(files.MaxBytes), "50 MB"
	if fileMime == "image/svg+xml" {
		limit, limitText = files.MaxSVGBytes, "5 MB"
	}
	if size <= 0 {
		return http.StatusBadRequest, "Ukuran file tidak valid"
	}
	if size > limit {
		return http.StatusRequestEntityTooLarge, "File terlalu besar — maksimal " + limitText
	}
	return 0, ""
}

// POST /api/uploads/presign
func (s *Server) postPresign(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		errJSON(w, http.StatusServiceUnavailable, "Penyimpanan file belum dikonfigurasi")
		return
	}
	var body struct {
		Mime    string `json:"mime"`
		Size    int64  `json:"size"`
		Purpose string `json:"purpose"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Permintaan tidak valid")
		return
	}
	if code, msg := checkPresign(userFrom(r), body.Mime, body.Size, body.Purpose); code != 0 {
		errJSON(w, code, msg)
		return
	}
	key, err := files.NewKey(body.Purpose, body.Mime)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Gagal membuat kunci file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":       key,
		"uploadUrl": s.files.PresignPut(key, body.Mime, body.Size, presignTTL),
		"headers":   map[string]string{"Content-Type": body.Mime},
		"expiresIn": int(presignTTL.Seconds()),
	})
}

type uploadedFile struct {
	URL      string `json:"url"`
	WidthPx  *int   `json:"widthPx"` // null for SVG
	HeightPx *int   `json:"heightPx"`
}

// POST /api/uploads/complete {key}
func (s *Server) postUploadComplete(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		errJSON(w, http.StatusServiceUnavailable, "Penyimpanan file belum dikonfigurasi")
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Permintaan tidak valid")
		return
	}
	purpose, ok := files.ParseKey(body.Key)
	if _, known := files.Purposes[purpose]; !ok || !known { // legacy keys are never uploadable
		errJSON(w, http.StatusBadRequest, "Kunci file tidak valid")
		return
	}
	u := userFrom(r)
	if roles := files.Purposes[purpose]; roles != nil && !hasRole(u, roles...) {
		errJSON(w, http.StatusForbidden, "Akun kamu tidak boleh mengunggah ke sini")
		return
	}
	ctx := r.Context()

	// Completing twice is harmless for the owner; anyone else sees nothing.
	var ownerID *string
	var done uploadedFile
	err := s.pool.QueryRow(ctx, `SELECT owner_id, width_px, height_px FROM files WHERE key = $1`, body.Key).
		Scan(&ownerID, &done.WidthPx, &done.HeightPx)
	if err == nil {
		if ownerID == nil || *ownerID != u.ID {
			errJSON(w, http.StatusNotFound, "File tidak ditemukan")
			return
		}
		done.URL = s.files.PublicURL(body.Key)
		writeJSON(w, http.StatusOK, done)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	info, status, msg := s.inspectObject(ctx, body.Key, u)
	if status != 0 {
		if status != http.StatusNotFound && status != http.StatusBadGateway {
			s.deleteObject(body.Key)
		}
		errJSON(w, status, msg)
		return
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO files (key, mime, bytes, width_px, height_px, owner_id, purpose)
		VALUES ($1,$2,$3,NULLIF($4,0),NULLIF($5,0),$6,$7) ON CONFLICT (key) DO NOTHING`,
		body.Key, info.Mime, info.Bytes, info.Width, info.Height, u.ID, purpose); err != nil {
		errJSON(w, http.StatusInternalServerError, "Gagal menyimpan data file: "+err.Error())
		return
	}
	out := uploadedFile{URL: s.files.PublicURL(body.Key)}
	if info.Width > 0 {
		out.WidthPx, out.HeightPx = &info.Width, &info.Height
	}
	writeJSON(w, http.StatusOK, out)
}

type inspected struct {
	files.Info
	Bytes int64
}

// inspectObject checks the uploaded object. On refusal it returns the HTTP
// status and message; status 404/502 mean "nothing to delete / try again".
func (s *Server) inspectObject(ctx context.Context, key string, u *User) (inspected, int, string) {
	head, err := s.files.Head(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return inspected{}, http.StatusNotFound, "File belum terunggah ke penyimpanan"
	}
	if err != nil {
		log.Printf("uploads: head %s: %v", key, err)
		return inspected{}, http.StatusBadGateway, "Penyimpanan file tidak terjangkau"
	}
	n := min(head.Size, files.PrefixBytes)
	if n <= 0 {
		return inspected{}, http.StatusBadRequest, "File kosong"
	}
	prefix, err := s.files.GetPrefix(ctx, key, n)
	if err != nil {
		log.Printf("uploads: get %s: %v", key, err)
		return inspected{}, http.StatusBadGateway, "Penyimpanan file tidak terjangkau"
	}
	info, err := files.Inspect(prefix, head.Size)
	if err != nil {
		return inspected{}, http.StatusBadRequest, "File ditolak: " + err.Error()
	}
	if info.Mime == "image/svg+xml" && !hasRole(u, "admin") {
		return inspected{}, http.StatusForbidden, "File SVG hanya boleh diunggah admin"
	}
	// the type the browser PUT with becomes the Content-Type the bucket serves
	if claimed, _, _ := mime.ParseMediaType(head.ContentType); claimed != info.Mime {
		return inspected{}, http.StatusBadRequest, "Jenis file tidak cocok dengan isinya"
	}
	return inspected{Info: info, Bytes: head.Size}, 0, ""
}

func (s *Server) deleteObject(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.files.Delete(ctx, key); err != nil {
		log.Printf("uploads: hapus %s: %v", key, err)
	}
}
