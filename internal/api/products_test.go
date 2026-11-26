package api

import (
	"net/http"
	"testing"
)

func listsListing(out map[string]any, id string) bool {
	products, _ := out["products"].([]any)
	for _, p := range products {
		if m, ok := p.(map[string]any); ok && m["id"] == id {
			return true
		}
	}
	return false
}

// GET /api/products?designer= exposes deactivated listings only to the shop
// owner and admins (needs TEST_DATABASE_URL, see google_test.go).
func TestProductsHideInactiveFromPublic(t *testing.T) {
	f := setupCheckout(t) // listing2 is deactivated
	path := "/api/products?designer=d-co-" + f.tag

	cases := []struct {
		name    string
		cookie  *http.Cookie
		wantOff bool
	}{
		{"logged out", nil, false},
		{"customer", f.buyer, false},
		{"other creator", f.otherCreator, false},
		{"owner", f.creator, true},
		{"admin", f.admin, true},
	}
	for _, c := range cases {
		rec, out := call(f.s, "GET", path, nil, c.cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", c.name, rec.Code)
		}
		if !listsListing(out, f.listing) {
			t.Errorf("%s: active listing missing", c.name)
		}
		if got := listsListing(out, f.listing2); got != c.wantOff {
			t.Errorf("%s: inactive listing visible = %v, want %v", c.name, got, c.wantOff)
		}
	}

	// without a designer filter the public catalog never lists inactive items, even for the owner
	_, out := call(f.s, "GET", "/api/products", nil, f.creator)
	if listsListing(out, f.listing2) {
		t.Error("unfiltered catalog lists an inactive listing")
	}
}
