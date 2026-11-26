package api

import (
	"context"
	"net/http"
	"testing"
)

// Signup must be refused without explicit consent and must stamp the consent
// timestamps when accepted (creator agreement only for creators).
func TestSignupRequiresTermsConsent(t *testing.T) {
	s, _, pool := emailServer(t)
	ctx := context.Background()

	signup := func(username string, extra map[string]any) int {
		body := map[string]any{"username": username, "email": username + "@mail.test", "password": "rahasia123"}
		for k, v := range extra {
			body[k] = v
		}
		rec, _ := call(s, "POST", "/api/auth/signup", body)
		return rec.Code
	}
	stamps := func(username string) (terms, creator bool) {
		err := pool.QueryRow(ctx,
			`SELECT terms_accepted_at IS NOT NULL, creator_terms_accepted_at IS NOT NULL FROM users WHERE username = $1`,
			username).Scan(&terms, &creator)
		if err != nil {
			t.Fatalf("baca timestamp %s: %v", username, err)
		}
		return
	}

	none := "tesnoterms_" + randomHex(3)
	cleanupUser(t, pool, none)
	if code := signup(none, nil); code != http.StatusBadRequest {
		t.Fatalf("tanpa acceptTerms: %d, want 400", code)
	}
	if code := signup(none, map[string]any{"acceptTerms": false}); code != http.StatusBadRequest {
		t.Fatalf("acceptTerms=false: %d, want 400", code)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = $1`, none).Scan(&n)
	if n != 0 {
		t.Fatalf("akun dibuat tanpa persetujuan: %d", n)
	}

	cust := "tescust_" + randomHex(3)
	cleanupUser(t, pool, cust)
	if code := signup(cust, map[string]any{"acceptTerms": true}); code != http.StatusCreated {
		t.Fatalf("pelanggan dengan acceptTerms: %d, want 201", code)
	}
	if terms, creator := stamps(cust); !terms || creator {
		t.Fatalf("pelanggan: terms=%v creator=%v, want true/false", terms, creator)
	}

	creat := "tescreat_" + randomHex(3)
	cleanupUser(t, pool, creat)
	if code := signup(creat, map[string]any{"acceptTerms": true, "asCreator": true}); code != http.StatusBadRequest {
		t.Fatalf("kreator tanpa acceptCreatorTerms: %d, want 400", code)
	}
	if code := signup(creat, map[string]any{"acceptTerms": true, "asCreator": true, "acceptCreatorTerms": true}); code != http.StatusCreated {
		t.Fatalf("kreator lengkap: %d, want 201", code)
	}
	if terms, creator := stamps(creat); !terms || !creator {
		t.Fatalf("kreator: terms=%v creator=%v, want true/true", terms, creator)
	}
}
