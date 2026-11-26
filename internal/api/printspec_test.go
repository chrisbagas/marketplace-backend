package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Print spec integration tests (need TEST_DATABASE_URL, see google_test.go):
// the size a creator picks in Studio must reach the order item the vendor reads.

func TestPrintSpecFlowsFromStudioToOrderItem(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	t.Cleanup(func() { // runs before setupCheckout's cleanup (LIFO): orders first, then what the approval created
		f.pool.Exec(ctx, `DELETE FROM orders WHERE user_id = $1`, f.buyerID)
		f.pool.Exec(ctx, `DELETE FROM listings WHERE id IN (SELECT published_listing_id FROM design_submissions WHERE designer_id = $1)`, "d-co-"+f.tag)
		f.pool.Exec(ctx, `DELETE FROM designs WHERE designer_id = $1 AND id <> $2`, "d-co-"+f.tag, "dz-co-"+f.tag)
		f.pool.Exec(ctx, `DELETE FROM design_submissions WHERE designer_id = $1`, "d-co-"+f.tag)
	})

	submit := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{"title": "Desain Print " + f.tag, "type": "kaos", "color": "putih", "price": f.price, "uri": "data:image/svg+xml,x"}
		for k, v := range extra {
			body[k] = v
		}
		rec, out := call(f.s, "POST", "/api/designs", body, f.creator)
		return rec.Code, out
	}

	// out-of-range specs are refused: kaos print area is 30 x 40 cm
	for name, extra := range map[string]map[string]any{
		"lebar melebihi area":     {"widthCm": 30.5},
		"lebar nol":               {"widthCm": 0},
		"lebar negatif":           {"widthCm": -5},
		"geser melebihi setengah": {"widthCm": 20, "offsetYCm": 20.5},
		"geser ke atas berlebih":  {"widthCm": 20, "offsetYCm": -25},
	} {
		if code, out := submit(extra); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}

	// valid spec is stored on the submission
	code, out := submit(map[string]any{"widthCm": 18.5, "offsetYCm": 2})
	if code != http.StatusOK {
		t.Fatalf("ajukan desain: %d %v", code, out)
	}
	design := out["design"].(map[string]any)
	if design["widthCm"] != 18.5 || design["offsetYCm"] != 2.0 {
		t.Fatalf("pengajuan menyimpan %v / %v, want 18.5 / 2", design["widthCm"], design["offsetYCm"])
	}

	// omitted width = Studio default for the product type (kaos 24 cm)
	code, out = submit(nil)
	if code != http.StatusOK {
		t.Fatalf("ajukan tanpa ukuran: %d %v", code, out)
	}
	if d := out["design"].(map[string]any); d["widthCm"] != 24.0 || d["offsetYCm"] != 0.0 {
		t.Fatalf("default %v / %v, want 24 / 0", d["widthCm"], d["offsetYCm"])
	}

	// approve -> listing carries the spec
	rec, out := call(f.s, "PATCH", "/api/designs", map[string]any{"id": design["id"], "status": "disetujui"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("setujui: %d %v", rec.Code, out)
	}
	listing, _ := out["design"].(map[string]any)["listingId"].(string)
	if listing == "" {
		t.Fatalf("listing tidak terbit: %v", out)
	}
	rec, out = call(f.s, "GET", "/api/products/"+listing, nil)
	prod, _ := out["product"].(map[string]any)
	if rec.Code != http.StatusOK || prod["widthCm"] != 18.5 || prod["offsetYCm"] != 2.0 {
		t.Fatalf("listing: %d %v, want 18.5 / 2", rec.Code, prod)
	}

	// order it -> the order item returned by GET /api/orders/{id} carries the spec
	line := f.line("M", 1)
	line["productId"] = listing
	rec, out = f.order(t, f.buyer, "", line)
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	rec, out = call(f.s, "GET", "/api/orders/"+id, nil, f.buyer)
	if rec.Code != http.StatusOK {
		t.Fatalf("ambil pesanan: %d %v", rec.Code, out)
	}
	it := out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["widthCm"] != 18.5 || it["offsetYCm"] != 2.0 {
		t.Fatalf("item pesanan %v / %v, want 18.5 / 2", it["widthCm"], it["offsetYCm"])
	}

	// the item is a snapshot: a later change to the listing does not touch it
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET width_cm = 10, offset_y_cm = 0 WHERE id = $1`, listing); err != nil {
		t.Fatal(err)
	}
	_, out = call(f.s, "GET", "/api/orders/"+id, nil, f.admin)
	it = out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["widthCm"] != 18.5 || it["offsetYCm"] != 2.0 {
		t.Fatalf("snapshot berubah: %v / %v", it["widthCm"], it["offsetYCm"])
	}
}

func TestCustomDesignSnapshotsItsOwnPrintSpec(t *testing.T) {
	f := setupCheckout(t)
	useHostedFiles(t)
	design := insertFile(t, f, "custom", 4000) // sharp enough for every width the test uses
	custom := func(extra map[string]any) map[string]any {
		l := map[string]any{"productId": "custom-" + f.tag, "title": "Custom", "type": "kaos", "color": "putih", "size": "M", "qty": 1,
			"price": 1, "designUri": design}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}

	// out of range → 400 with a readable reason
	rec, out := f.order(t, f.buyer, "", custom(map[string]any{"widthCm": 35}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Lebar cetak") {
		t.Fatalf("lebar berlebih: %d %v", rec.Code, out)
	}
	rec, out = f.order(t, f.buyer, "", custom(map[string]any{"widthCm": 20, "offsetYCm": 21}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Posisi vertikal") {
		t.Fatalf("geser berlebih: %d %v", rec.Code, out)
	}

	rec, out = f.order(t, f.buyer, "", custom(map[string]any{"widthCm": 20.4, "offsetYCm": -3}))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan custom: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	_, out = call(f.s, "GET", "/api/orders/"+id, nil, f.buyer)
	it := out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["widthCm"] != 20.4 || it["offsetYCm"] != -3.0 {
		t.Fatalf("item custom %v / %v, want 20.4 / -3", it["widthCm"], it["offsetYCm"])
	}

	// a cart saved before the print spec existed has no width: product default (kaos 24 cm)
	rec, out = f.order(t, f.buyer, "", custom(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan custom lama: %d %v", rec.Code, out)
	}
	id = out["order"].(map[string]any)["id"].(string)
	_, out = call(f.s, "GET", "/api/orders/"+id, nil, f.buyer)
	it = out["order"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["widthCm"] != 24.0 || it["offsetYCm"] != 0.0 {
		t.Fatalf("default custom %v / %v, want 24 / 0", it["widthCm"], it["offsetYCm"])
	}
}
