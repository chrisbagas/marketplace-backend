package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Integration test for the public creator profile (needs TEST_DATABASE_URL,
// see google_test.go).

func TestPublicDesignerProfile(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	id := "d-co-" + f.tag
	if _, err := f.pool.Exec(ctx, `UPDATE designers SET bio='Ilustrator batik modern', bank_name='BCA', bank_account='1234567890', bank_holder='Rahasia Pribadi' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET sold = 7 WHERE id = $1`, f.listing); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET sold = 3 WHERE id = $1`, f.listing2); err != nil {
		t.Fatal(err)
	}

	// no login needed
	rec, out := call(f.s, "GET", "/api/designers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("profil: %d %v", rec.Code, out)
	}
	d := out["designer"].(map[string]any)
	if d["id"] != id || d["name"] != "Kreator" || d["city"] != "Bandung" || d["bio"] != "Ilustrator batik modern" {
		t.Errorf("field publik salah: %v", d)
	}
	if num(d["products"]) != 1 || num(d["sold"]) != 10 {
		t.Errorf("products=%v sold=%v, want 1 aktif dan 10 terjual (semua listing)", d["products"], d["sold"])
	}
	// only the public allow-list may appear in the payload
	allowed := map[string]bool{"id": true, "name": true, "city": true, "bio": true, "hue": true, "avatar": true, "products": true, "sold": true, "since": true}
	for k := range d {
		if !allowed[k] {
			t.Errorf("field non-publik bocor: %q", k)
		}
	}
	if body := rec.Body.String(); strings.Contains(body, "1234567890") || strings.Contains(body, "Rahasia Pribadi") || strings.Contains(body, "BCA") {
		t.Errorf("data bank bocor: %s", body)
	}

	if rec, _ := call(f.s, "GET", "/api/designers/tidak-ada-"+f.tag, nil); rec.Code != http.StatusNotFound {
		t.Errorf("kreator tak dikenal: %d, want 404", rec.Code)
	}

	// the shop listing (/api/search?designer=) only shows this creator's active products
	rec, out = call(f.s, "GET", "/api/search?"+url.Values{"designer": {id}}.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %v", rec.Code, out)
	}
	products := out["products"].([]any)
	if len(products) != 1 || products[0].(map[string]any)["id"] != f.listing {
		t.Errorf("etalase: %v, want hanya listing aktif %s", products, f.listing)
	}
}
