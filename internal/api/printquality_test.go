package api

import (
	"context"
	"encoding/base64"
	"math"
	"net/http"
	"strings"
	"testing"
)

// rasterDataURI is a w×h PNG as an inline data URI.
func rasterDataURI(t *testing.T, w, h int) string {
	t.Helper()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes(t, w, h))
}

// Print quality tests: the DPI rule (>= 150) on submissions and custom
// checkout lines, driven by the pixel sizes recorded in the files table
// (integration part needs TEST_DATABASE_URL, see google_test.go).

func TestEffectiveDPIMath(t *testing.T) {
	if got := effectiveDPI(1000, 30); math.Abs(got-84.67) > 0.01 {
		t.Errorf("1000 px @ 30 cm = %.2f, want 84.67", got)
	}
	if got := effectiveDPI(1000, 15); math.Abs(got-169.33) > 0.01 {
		t.Errorf("1000 px @ 15 cm = %.2f, want 169.33", got)
	}
	// the widest print that still holds 150 / 300 DPI
	for _, c := range []struct {
		px   int
		dpi  float64
		want float64
	}{{1000, 150, 16.9}, {1000, 300, 8.4}, {1500, 150, 25.4}, {3000, 300, 25.4}, {4500, 300, 38.1}} {
		if got := maxWidthForDPI(c.px, c.dpi); got != c.want {
			t.Errorf("max width %d px @ %.0f DPI = %v, want %v", c.px, c.dpi, got, c.want)
		}
	}
	// the width it reports never falls below the threshold
	for px := 200; px <= 6000; px += 37 {
		w := maxWidthForDPI(px, minPrintDPI)
		if w >= minPrintWidthCm && effectiveDPI(px, w) < minPrintDPI {
			t.Fatalf("%d px: width %v gives %.2f DPI", px, w, effectiveDPI(px, w))
		}
	}
	err := checkResolution(1000, 30)
	if err == nil || !strings.Contains(err.Error(), "85 DPI") || !strings.Contains(err.Error(), "16.9 cm") {
		t.Errorf("pesan: %v", err)
	}
	if err := checkResolution(1000, 15); err != nil {
		t.Errorf("169 DPI harus lolos: %v", err)
	}
	if err := checkResolution(1000, 16.9); err != nil {
		t.Errorf("150.3 DPI harus lolos: %v", err)
	}
	if err := checkResolution(200, 30); err == nil || !strings.Contains(err.Error(), "terlalu rendah") {
		t.Errorf("gambar mungil: %v", err)
	}
}

// insertFile records an upload the way /api/uploads/complete does (widthPx 0 = SVG).
func insertFile(t *testing.T, f *checkoutFixture, purpose string, widthPx int) string {
	t.Helper()
	key := purpose + "/" + randomHex(16) + ".png"
	if widthPx == 0 {
		key = purpose + "/" + randomHex(16) + ".svg"
	}
	mime := "image/png"
	if widthPx == 0 {
		mime = "image/svg+xml"
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO files (key, mime, bytes, width_px, height_px, purpose) VALUES ($1,$2,1234,NULLIF($3,0),NULLIF($3,0),$4)`,
		key, mime, widthPx, purpose); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM files WHERE key = $1`, key) })
	return hostedFilePrefix + key
}

func useHostedFiles(t *testing.T) {
	hostedFilePrefix = "https://files.test/"
	t.Cleanup(func() { hostedFilePrefix = "" })
}

func TestPostDesignEnforcesMinimumDPI(t *testing.T) {
	f := setupCheckout(t)
	useHostedFiles(t)
	ctx := context.Background()
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM design_submissions WHERE designer_id = $1`, "d-co-"+f.tag) })

	png1000 := insertFile(t, f, "designs", 1000)
	svg := insertFile(t, f, "designs", 0)
	unknown := hostedFilePrefix + "designs/" + strings.Repeat("b", 32) + ".png"

	submit := func(uri string, width float64) (int, map[string]any) {
		rec, out := call(f.s, "POST", "/api/designs", map[string]any{
			"title": "Resolusi " + f.tag, "type": "kaos", "color": "putih", "price": f.price, "uri": uri, "widthCm": width,
		}, f.creator)
		return rec.Code, out
	}

	// 1000 px at 30 cm = 85 DPI → refused, message carries the DPI and the widest print
	code, out := submit(png1000, 30)
	msg, _ := out["error"].(string)
	if code != http.StatusBadRequest || !strings.Contains(msg, "85 DPI") || !strings.Contains(msg, "16.9 cm") {
		t.Fatalf("1000 px @ 30 cm: %d %v, want 400 with 85 DPI / 16.9 cm", code, out)
	}
	// same image at 15 cm = 169 DPI → accepted
	if code, out := submit(png1000, 15); code != http.StatusOK {
		t.Fatalf("1000 px @ 15 cm: %d %v, want 200", code, out)
	}
	// exactly at the limit: 16.9 cm = 150.3 DPI
	if code, out := submit(png1000, 16.9); code != http.StatusOK {
		t.Fatalf("1000 px @ 16.9 cm: %d %v, want 200", code, out)
	}
	if code, out := submit(png1000, 17); code != http.StatusBadRequest {
		t.Fatalf("1000 px @ 17 cm: %d %v, want 400 (149.4 DPI)", code, out)
	}
	// default width (kaos 24 cm) is checked too when the client omits it
	rec, out := call(f.s, "POST", "/api/designs", map[string]any{"title": "Default " + f.tag, "uri": png1000}, f.creator)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("lebar default: %d %v, want 400", rec.Code, out)
	}
	// vector designs have no resolution: any width, data URI or SVG upload
	for name, uri := range map[string]string{"data uri": "data:image/svg+xml,x", "svg upload": svg} {
		if code, out := submit(uri, 30); code != http.StatusOK {
			t.Errorf("vektor (%s) @ 30 cm: %d %v, want 200", name, code, out)
		}
	}
	// raster that is not in the files table, or not ours, is refused — never trusted
	for name, uri := range map[string]string{
		"tidak tercatat": unknown,
		"host asing":     "https://evil.example/designs/" + strings.Repeat("c", 32) + ".png",
		"path lama":      "/uploads/lama.png",
	} {
		if code, out := submit(uri, 10); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	// a raster data URI is measured by the server, not exempted
	if code, out := submit(rasterDataURI(t, 1000, 10), 30); code != http.StatusBadRequest {
		t.Errorf("data URI PNG 1000 px @ 30 cm: %d %v, want 400", code, out)
	}
	if code, out := submit(rasterDataURI(t, 1000, 10), 15); code != http.StatusOK {
		t.Errorf("data URI PNG 1000 px @ 15 cm: %d %v, want 200", code, out)
	}
}

func TestCustomCheckoutLineEnforcesMinimumDPI(t *testing.T) {
	f := setupCheckout(t)
	useHostedFiles(t)
	png1000 := insertFile(t, f, "custom", 1000)
	svg := insertFile(t, f, "custom", 0)
	unknown := hostedFilePrefix + "custom/" + strings.Repeat("d", 32) + ".png"

	quote := func(uri string, width float64) (int, map[string]any) {
		line := map[string]any{"productId": "custom-1", "type": "kaos", "color": "putih", "size": "M", "qty": 1, "designUri": uri, "widthCm": width}
		rec, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{line}}, f.buyer)
		return rec.Code, out
	}
	code, out := quote(png1000, 30)
	if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "85 DPI") || !strings.Contains(msg, "16.9 cm") {
		t.Fatalf("quote 1000 px @ 30 cm: %d %v, want 400 with 85 DPI", code, out)
	}
	if code, out := quote(png1000, 15); code != http.StatusOK {
		t.Fatalf("quote 1000 px @ 15 cm: %d %v, want 200", code, out)
	}
	if code, out := quote(svg, 30); code != http.StatusOK {
		t.Fatalf("quote vektor @ 30 cm: %d %v, want 200", code, out)
	}
	if code, out := quote("data:image/svg+xml,x", 30); code != http.StatusOK {
		t.Fatalf("quote data URI SVG @ 30 cm: %d %v, want 200", code, out)
	}
	if code, out := quote(unknown, 10); code != http.StatusBadRequest {
		t.Fatalf("quote raster tak tercatat: %d %v, want 400", code, out)
	}

	// creating the order enforces the same rule (it goes through quoteOrder)
	line := func(uri string, width float64) map[string]any {
		return map[string]any{"productId": "custom-1", "title": "Custom", "type": "kaos", "color": "putih", "size": "M", "qty": 1, "designUri": uri, "widthCm": width}
	}
	if rec, out := f.order(t, f.buyer, "", line(png1000, 30)); rec.Code != http.StatusBadRequest {
		t.Fatalf("pesanan 1000 px @ 30 cm: %d %v, want 400", rec.Code, out)
	}
	rec, out := f.order(t, f.buyer, "", line(png1000, 15))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan 1000 px @ 15 cm: %d %v, want 200", rec.Code, out)
	}

	// the order carries each item's DPI for production (admin order page)
	id := out["order"].(map[string]any)["id"].(string)
	_, out = call(f.s, "GET", "/api/orders/"+id, nil, f.admin)
	it := out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["dpi"] != 169.0 {
		t.Errorf("dpi item %v, want 169", it["dpi"])
	}
}

func TestOrderItemDPIOmittedForVectorDesigns(t *testing.T) {
	f := setupCheckout(t)
	useHostedFiles(t)
	svg := insertFile(t, f, "custom", 0)
	line := map[string]any{"productId": "custom-1", "title": "Custom", "type": "kaos", "color": "putih", "size": "M", "qty": 1, "designUri": svg, "widthCm": 30}
	rec, out := f.order(t, f.buyer, "", line)
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan vektor: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	_, out = call(f.s, "GET", "/api/orders/"+id, nil, f.admin)
	it := out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if _, has := it["dpi"]; has {
		t.Errorf("item vektor tidak boleh punya dpi: %v", it["dpi"])
	}
}

func TestSavedDesignsReportPixelWidth(t *testing.T) {
	f := setupCheckout(t)
	useHostedFiles(t)
	png := insertFile(t, f, "custom", 2000)
	t.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM user_designs WHERE user_id = $1`, f.buyerID) })

	for _, uri := range []string{png, "data:image/svg+xml,x"} {
		rec, out := call(f.s, "POST", "/api/user-designs", map[string]any{"title": "Simpanan", "type": "kaos", "color": "putih", "uri": uri, "widthCm": 20}, f.buyer)
		if rec.Code != http.StatusOK {
			t.Fatalf("simpan desain: %d %v", rec.Code, out)
		}
	}
	_, out := call(f.s, "GET", "/api/user-designs", nil, f.buyer)
	list, _ := out["designs"].([]any)
	if len(list) != 2 {
		t.Fatalf("desain tersimpan: %v", out)
	}
	for _, d := range list {
		d := d.(map[string]any)
		want := any(nil)
		if d["uri"] == png {
			want = 2000.0
		}
		if d["widthPx"] != want {
			t.Errorf("widthPx %v untuk %v, want %v", d["widthPx"], d["uri"], want)
		}
	}
}
