package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Integration tests for design reports (need TEST_DATABASE_URL, see google_test.go).

func (f *searchFixture) report(t *testing.T, listing string, extra map[string]any, cookies ...*http.Cookie) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"listingId":   listing,
		"reason":      "hak-cipta",
		"email":       "pelapor" + f.tag + "@mail.test",
		"description": "Desain ini menjiplak karya saya tanpa izin.",
	}
	for k, v := range extra {
		body[k] = v
	}
	rec, out := call(f.s, "POST", "/api/reports", body, cookies...)
	return rec.Code, out
}

func TestReportValidation(t *testing.T) {
	f := setupSearch(t)
	l := f.ids["naga-kaos"]
	cases := []struct {
		name  string
		extra map[string]any
		code  int
	}{
		{"alasan tak dikenal", map[string]any{"reason": "spam"}, http.StatusBadRequest},
		{"email salah", map[string]any{"email": "bukan-email"}, http.StatusBadRequest},
		{"deskripsi pendek", map[string]any{"description": "jelek"}, http.StatusBadRequest},
		{"deskripsi terlalu panjang", map[string]any{"description": strings.Repeat("a", reportDescMax+1)}, http.StatusBadRequest},
		{"bukti bukan http", map[string]any{"evidenceUrl": "javascript:alert(1)"}, http.StatusBadRequest},
		{"bukti terlalu panjang", map[string]any{"evidenceUrl": "https://x.test/" + strings.Repeat("a", reportEvidenceMax)}, http.StatusBadRequest},
		{"produk tak ada", map[string]any{"listingId": "tidak-ada"}, http.StatusNotFound},
		{"valid dengan bukti", map[string]any{"evidenceUrl": "https://contoh.test/karya-asli"}, http.StatusCreated},
	}
	for _, c := range cases {
		f.s.limiter = newRateLimiter() // validation cases must not trip the IP limit
		if code, out := f.report(t, l, c.extra); code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.code)
		}
	}
}

func TestReportStoredAndAdminOnly(t *testing.T) {
	f := setupSearch(t)
	l := f.ids["naga-kaos"]

	if code, out := f.report(t, l, map[string]any{"reason": "merek-dagang"}); code != http.StatusCreated {
		t.Fatalf("lapor anonim: %d %v", code, out)
	}
	// logged in with the email left out: falls back to the account email and stores the reporter
	if code, out := f.report(t, l, map[string]any{"email": ""}, f.buyer); code != http.StatusCreated {
		t.Fatalf("lapor login: %d %v", code, out)
	}

	for name, c := range map[string]*http.Cookie{"anon": nil, "pembeli": f.buyer, "kreator": f.creator} {
		want := http.StatusForbidden
		if c == nil {
			want = http.StatusUnauthorized
		}
		if rec, _ := call(f.s, "GET", "/api/reports", nil, c); rec.Code != want {
			t.Errorf("GET /api/reports sebagai %s: %d, want %d", name, rec.Code, want)
		}
		if rec, _ := call(f.s, "PATCH", "/api/reports/1", map[string]any{"action": "review"}, c); rec.Code != want {
			t.Errorf("PATCH /api/reports sebagai %s: %d, want %d", name, rec.Code, want)
		}
	}

	rec, out := call(f.s, "GET", "/api/reports?status=baru&limit=200", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list: %d %v", rec.Code, out)
	}
	var mine []map[string]any
	for _, r := range out["reports"].([]any) {
		if m := r.(map[string]any); m["listingId"] == l {
			mine = append(mine, m)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("laporan untuk listing: %d, want 2", len(mine))
	}
	for _, m := range mine {
		if m["status"] != "baru" || m["designerName"] != "Studio Wx"+f.tag || m["listingActive"] != true {
			t.Errorf("laporan: %v", m)
		}
	}
	var reporter *string
	if err := f.pool.QueryRow(context.Background(), `SELECT reporter_user_id FROM design_reports WHERE listing_id=$1 AND email=$2`, l, f.buyerID+"@mail.test").Scan(&reporter); err != nil || reporter == nil || *reporter != f.buyerID {
		t.Errorf("reporter_user_id: %v %v", reporter, err)
	}
	if rec, _ := call(f.s, "GET", "/api/reports?status=aneh", nil, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("status tak dikenal: %d", rec.Code)
	}
	// limit=0: counts only (admin tab badge)
	if rec, out := call(f.s, "GET", "/api/reports?limit=0", nil, f.admin); rec.Code != http.StatusOK || len(out["reports"].([]any)) != 0 || num(out["counts"].(map[string]any)["baru"]) < 2 {
		t.Errorf("limit=0: %d %v", rec.Code, out)
	}
}

func TestReportTakedownRemovesFromSearch(t *testing.T) {
	f := setupSearch(t)
	fm := f.s.auth.Mailer.(*fakeMailer)
	l := f.ids["naga-kaos"]
	q := url.Values{"q": {"mitologi" + f.tag}}

	ids, _ := f.search(t, q)
	f.want(t, "sebelum", ids, "naga-mug", "naga-kaos") // order is by sales; only membership matters below
	if len(ids) != 2 {
		t.Fatalf("sebelum: %v", ids)
	}

	post := func() int64 {
		code, out := f.report(t, l, nil)
		if code != http.StatusCreated {
			t.Fatalf("lapor: %d %v", code, out)
		}
		return int64(out["id"].(float64))
	}
	first, second := post(), post()

	if rec, out := call(f.s, "PATCH", "/api/reports/"+itoa(first), map[string]any{"action": "review"}, f.admin); rec.Code != http.StatusOK || out["report"].(map[string]any)["status"] != "ditinjau" {
		t.Fatalf("review: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "PATCH", "/api/reports/"+itoa(first), map[string]any{"action": "review"}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("review dua kali: %d, want 409", rec.Code)
	}

	rec, out := call(f.s, "PATCH", "/api/reports/"+itoa(first), map[string]any{"action": "takedown", "note": "Terbukti menjiplak"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("takedown: %d %v", rec.Code, out)
	}
	if r := out["report"].(map[string]any); r["status"] != "ditindak" || r["listingActive"] != false || r["adminNote"] != "Terbukti menjiplak" {
		t.Errorf("laporan setelah takedown: %v", r)
	}

	ids, _ = f.search(t, q)
	f.want(t, "sesudah", ids, "naga-mug")

	// the other open report on the same listing is closed with it
	var st string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM design_reports WHERE id=$1`, second).Scan(&st); err != nil || st != "ditindak" {
		t.Errorf("laporan kedua: %q %v", st, err)
	}
	if rec, _ := call(f.s, "PATCH", "/api/reports/"+itoa(second), map[string]any{"action": "takedown"}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("takedown ulang: %d, want 409", rec.Code)
	}

	// the creator is emailed, the reporter's identity is not part of it
	m := fm.next(t)
	if m.To != "co_creator_"+f.tag+"@mail.test" || !strings.Contains(m.Text, "Terbukti menjiplak") || !strings.Contains(m.Text, "hak cipta") || strings.Contains(m.Text, "pelapor"+f.tag) {
		t.Errorf("email kreator: to=%s\n%s", m.To, m.Text)
	}
	fm.none(t)
}

func TestReportReject(t *testing.T) {
	f := setupSearch(t)
	l := f.ids["naga-kaos"]
	fm := f.s.auth.Mailer.(*fakeMailer)
	code, out := f.report(t, l, nil)
	if code != http.StatusCreated {
		t.Fatalf("lapor: %d %v", code, out)
	}
	path := "/api/reports/" + itoa(int64(out["id"].(float64)))

	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "reject"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("tolak tanpa catatan: %d, want 400", rec.Code)
	}
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "hapus"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("aksi aneh: %d, want 400", rec.Code)
	}
	rec, out := call(f.s, "PATCH", path, map[string]any{"action": "reject", "note": "Karya orisinal"}, f.admin)
	if rec.Code != http.StatusOK || out["report"].(map[string]any)["status"] != "ditolak" {
		t.Fatalf("tolak: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "takedown"}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("takedown setelah ditolak: %d, want 409", rec.Code)
	}
	if rec, _ := call(f.s, "PATCH", "/api/reports/999999999", map[string]any{"action": "review"}, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("laporan tak ada: %d, want 404", rec.Code)
	}
	// still on sale, creator not emailed
	ids, _ := f.search(t, url.Values{"q": {"mitologi" + f.tag}})
	if len(ids) != 2 {
		t.Errorf("listing tetap tampil: %v", ids)
	}
	fm.none(t)
}

func TestReportRateLimit(t *testing.T) {
	f := setupSearch(t)
	l := f.ids["naga-kaos"]
	for i := 0; i < reportsPerHour; i++ {
		if code, out := f.report(t, l, nil); code != http.StatusCreated {
			t.Fatalf("laporan %d: %d %v", i+1, code, out)
		}
	}
	if code, _ := f.report(t, l, nil); code != http.StatusTooManyRequests {
		t.Errorf("laporan berikutnya: %d, want 429", code)
	}
}
