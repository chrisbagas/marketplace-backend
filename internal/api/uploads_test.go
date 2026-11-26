package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"karyakita/api/internal/s3"
)

// fakeS3 is an in-memory S3 server: path-style PUT / HEAD / GET (with Range) / DELETE.
// It requires a SigV4 Authorization header or presigned query on every request.
type fakeS3 struct {
	mu      sync.Mutex
	srv     *httptest.Server
	objects map[string][]byte
	types   map[string]string
	deleted []string
}

const fakeBucket = "bkt"

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}, types: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		signed := strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=") ||
			(r.URL.Query().Get("X-Amz-Signature") != "" && r.URL.Query().Get("X-Amz-Algorithm") == "AWS4-HMAC-SHA256")
		if !signed {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		key, ok := strings.CutPrefix(r.URL.Path, "/"+fakeBucket+"/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			f.objects[key], f.types[key] = data, r.Header.Get("Content-Type")
		case http.MethodHead, http.MethodGet:
			data, ok := f.objects[key]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", f.types[key])
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method == http.MethodHead {
				return
			}
			if rg, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes=0-"); ok {
				end, _ := strconv.Atoi(rg)
				data = data[:min(end+1, len(data))]
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(http.StatusPartialContent)
			}
			w.Write(data)
		case http.MethodDelete:
			delete(f.objects, key)
			f.deleted = append(f.deleted, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) put(key, contentType string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key], f.types[key] = data, contentType
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func pngBytes(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckPresign(t *testing.T) {
	customer := &User{Role: "customer"}
	creator := &User{Role: "designer"}
	admin := &User{Role: "admin"}
	cases := []struct {
		name    string
		u       *User
		mime    string
		size    int64
		purpose string
		want    int
	}{
		{"ok png", creator, "image/png", 30 << 20, "designs", 0},
		{"ok exactly 50 MB", creator, "image/jpeg", 50 << 20, "designs", 0},
		{"ok webp", customer, "image/webp", 1000, "custom", 0},
		{"over 50 MB", creator, "image/png", 50<<20 + 1, "designs", http.StatusRequestEntityTooLarge},
		{"zero size", creator, "image/png", 0, "designs", http.StatusBadRequest},
		{"svg for creator", creator, "image/svg+xml", 1000, "designs", http.StatusForbidden},
		{"svg for customer", customer, "image/svg+xml", 1000, "avatars", http.StatusForbidden},
		{"svg for admin", admin, "image/svg+xml", 1000, "designs", 0},
		{"gif", creator, "image/gif", 1000, "designs", http.StatusBadRequest},
		{"pdf", creator, "application/pdf", 1000, "designs", http.StatusBadRequest},
		{"unknown purpose", creator, "image/png", 1000, "etc", http.StatusBadRequest},
		{"customer into designs", customer, "image/png", 1000, "designs", http.StatusForbidden},
	}
	for _, c := range cases {
		if got, msg := checkPresign(c.u, c.mime, c.size, c.purpose); got != c.want {
			t.Errorf("%s: status %d (%s), want %d", c.name, got, msg, c.want)
		}
	}
}

type uploadFixture struct {
	s3      *fakeS3
	s       *Server
	creator *http.Cookie
	creatID string
	other   *http.Cookie
}

func setupUploads(t *testing.T) *uploadFixture {
	s, _, pool := emailServer(t)
	f := &uploadFixture{s3: newFakeS3(t), s: s}
	client, err := s3.New(s3.Config{
		Endpoint: f.s3.srv.URL, Region: "auto", Bucket: fakeBucket,
		AccessKeyID: "AK", SecretAccessKey: "SK", PublicURL: "https://files.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFiles(client)
	t.Cleanup(func() { s.SetFiles(nil) })

	tag := randomHex(3)
	f.creatID = "up_creator_" + tag
	f.creator = newSessionUser(t, pool, f.creatID, "designer", "Kreator Unggah")
	f.other = newSessionUser(t, pool, "up_other_"+tag, "designer", "Kreator Lain")
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM files WHERE owner_id IN ($1,$2)`, f.creatID, "up_other_"+tag)
		pool.Exec(ctx, `DELETE FROM sessions WHERE user_id IN ($1,$2)`, f.creatID, "up_other_"+tag)
		pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1,$2)`, f.creatID, "up_other_"+tag)
	})
	return f
}

func TestPresignEndpoint(t *testing.T) {
	f := setupUploads(t)
	if rec, _ := call(f.s, "POST", "/api/uploads/presign", map[string]any{"mime": "image/png", "size": 10, "purpose": "designs"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login: %d, want 401", rec.Code)
	}
	for name, body := range map[string]map[string]any{
		"terlalu besar": {"mime": "image/png", "size": 50<<20 + 1, "purpose": "designs"},
		"svg":           {"mime": "image/svg+xml", "size": 100, "purpose": "designs"},
		"mime salah":    {"mime": "application/zip", "size": 100, "purpose": "designs"},
	} {
		if rec, out := call(f.s, "POST", "/api/uploads/presign", body, f.creator); rec.Code < 400 {
			t.Errorf("%s: %d %v, want error", name, rec.Code, out)
		}
	}
	rec, out := call(f.s, "POST", "/api/uploads/presign", map[string]any{"mime": "image/png", "size": 30 << 20, "purpose": "designs"}, f.creator)
	if rec.Code != http.StatusOK {
		t.Fatalf("presign: %d %v", rec.Code, out)
	}
	key, _ := out["key"].(string)
	url, _ := out["uploadUrl"].(string)
	if !strings.HasPrefix(key, "designs/") || !strings.HasSuffix(key, ".png") {
		t.Errorf("key %q", key)
	}
	for _, part := range []string{f.s3.srv.URL + "/" + fakeBucket + "/" + key, "X-Amz-Signature=", "content-length", "content-type"} {
		if !strings.Contains(url, part) {
			t.Errorf("uploadUrl %q lacks %q", url, part)
		}
	}
}

// uploadViaPresign performs presign → PUT (to the fake) → complete.
func uploadViaPresign(t *testing.T, f *uploadFixture, user *http.Cookie, mime, purpose string, data []byte) (string, int, map[string]any) {
	t.Helper()
	_, out := call(f.s, "POST", "/api/uploads/presign", map[string]any{"mime": mime, "size": len(data), "purpose": purpose}, user)
	key, _ := out["key"].(string)
	if key == "" {
		t.Fatalf("presign gagal: %v", out)
	}
	req, _ := http.NewRequest(http.MethodPut, out["uploadUrl"].(string), bytes.NewReader(data))
	req.Header.Set("Content-Type", mime)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %v %v", err, resp)
	}
	resp.Body.Close()
	rec, done := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": key}, user)
	return key, rec.Code, done
}

func TestCompleteStoresDimensions(t *testing.T) {
	f := setupUploads(t)
	key, code, out := uploadViaPresign(t, f, f.creator, "image/png", "designs", pngBytes(t, 640, 480))
	if code != http.StatusOK {
		t.Fatalf("complete: %d %v", code, out)
	}
	if out["url"] != "https://files.test/"+key || out["widthPx"] != float64(640) || out["heightPx"] != float64(480) {
		t.Errorf("respons %v", out)
	}
	var mime string
	var bytesN int64
	var w, h int
	var owner, purpose string
	if err := f.s.pool.QueryRow(context.Background(),
		`SELECT mime, bytes, width_px, height_px, owner_id, purpose FROM files WHERE key = $1`, key).
		Scan(&mime, &bytesN, &w, &h, &owner, &purpose); err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || w != 640 || h != 480 || owner != f.creatID || purpose != "designs" || bytesN == 0 {
		t.Errorf("baris files: %s %d %dx%d %s %s", mime, bytesN, w, h, owner, purpose)
	}
	// completing again is idempotent for the owner and invisible to others
	if rec, _ := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": key}, f.creator); rec.Code != http.StatusOK {
		t.Errorf("complete ulang: %d", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": key}, f.other); rec.Code != http.StatusNotFound {
		t.Errorf("complete oleh orang lain: %d, want 404", rec.Code)
	}
	// the bucket's own copy of the image is a checkout-valid custom design URL
	if !isHostedFile(out["url"].(string)) || isHostedFile("https://evil.test/"+key) {
		t.Error("isHostedFile salah")
	}
}

func TestCompleteRejectsNonImage(t *testing.T) {
	f := setupUploads(t)
	// claims PNG, is really a text/HTML file
	key, code, out := uploadViaPresign(t, f, f.creator, "image/png", "designs", []byte("<html><script>alert(1)</script></html>"))
	if code != http.StatusBadRequest {
		t.Fatalf("complete: %d %v, want 400", code, out)
	}
	if f.s3.has(key) {
		t.Error("objek yang ditolak tidak dihapus dari bucket")
	}
	var n int
	f.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM files WHERE key = $1`, key).Scan(&n)
	if n != 0 {
		t.Error("baris files tersimpan untuk objek yang ditolak")
	}

	// a truncated PNG (valid signature, no header chunk) is also rejected
	key, code, _ = uploadViaPresign(t, f, f.creator, "image/png", "designs", pngBytes(t, 10, 10)[:12])
	if code != http.StatusBadRequest || f.s3.has(key) {
		t.Errorf("PNG rusak: %d, masih ada=%v", code, f.s3.has(key))
	}

	// SVG content smuggled in under an image/png presign is rejected for non-admins
	key, code, _ = uploadViaPresign(t, f, f.creator, "image/png", "designs", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`))
	if code != http.StatusForbidden || f.s3.has(key) {
		t.Errorf("SVG tersembunyi: %d, masih ada=%v", code, f.s3.has(key))
	}

	// a key that was never uploaded
	if rec, _ := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": "designs/" + strings.Repeat("b", 32) + ".png"}, f.creator); rec.Code != http.StatusNotFound {
		t.Errorf("objek tidak ada: %d, want 404", rec.Code)
	}
	// keys outside the naming scheme
	if rec, _ := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": "../etc/passwd"}, f.creator); rec.Code != http.StatusBadRequest {
		t.Errorf("kunci liar: %d, want 400", rec.Code)
	}
}

func TestCompleteRejectsOversizeObject(t *testing.T) {
	f := setupUploads(t)
	key := "designs/" + strings.Repeat("c", 32) + ".png"
	// PUT done behind the server's back with a body larger than the limit
	big := append(pngBytes(t, 8, 8), make([]byte, 50<<20)...)
	f.s3.put(key, "image/png", big)
	rec, out := call(f.s, "POST", "/api/uploads/complete", map[string]any{"key": key}, f.creator)
	if rec.Code != http.StatusBadRequest || f.s3.has(key) {
		t.Errorf("objek >50 MB: %d %v, masih ada=%v", rec.Code, out, f.s3.has(key))
	}
}
