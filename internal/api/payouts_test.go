package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Tes integrasi penarikan saldo kreator (butuh TEST_DATABASE_URL, lihat google_test.go).

// paidOrder membuat pesanan qty×listing dan menandainya lunas (royalti tercatat).
func (f *checkoutFixture) paidOrder(t *testing.T, qty int) string {
	t.Helper()
	rec, out := f.order(t, f.buyer, "", f.line("M", qty))
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	if err := f.s.payOrder(context.Background(), id, "QRIS"); err != nil {
		t.Fatal(err)
	}
	return id
}

// complete: paket tiba (langsung lewat SQL) lalu pembeli menekan "Pesanan diterima".
func (f *checkoutFixture) complete(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE orders SET status='tiba', shipped_courier='JNE REG', tracking_number=$2,
		shipped_at=now() - interval '2 days', delivered_at=now() - interval '1 hour' WHERE id=$1`,
		id, "PO"+strings.ToUpper(f.tag)+id[len(id)-4:]); err != nil {
		t.Fatal(err)
	}
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]any{"action": "confirm"}, f.buyer); rec.Code != http.StatusOK {
		t.Fatalf("konfirmasi: %d %v", rec.Code, out)
	}
}

func (f *checkoutFixture) balance(t *testing.T) map[string]any {
	t.Helper()
	rec, out := call(f.s, "GET", "/api/designer/payouts", nil, f.creator)
	if rec.Code != http.StatusOK {
		t.Fatalf("saldo: %d %v", rec.Code, out)
	}
	return out["balance"].(map[string]any)
}

func (f *checkoutFixture) withdraw(amount int) (int, map[string]any) {
	rec, out := call(f.s, "POST", "/api/designer/payouts", map[string]any{"amount": amount}, f.creator)
	return rec.Code, out
}

func wantBalance(t *testing.T, b map[string]any, pending, available, inProcess, withdrawn int) {
	t.Helper()
	if num(b["pending"]) != pending || num(b["available"]) != available || num(b["inProcess"]) != inProcess || num(b["withdrawn"]) != withdrawn {
		t.Errorf("saldo %v, want pending=%d available=%d inProcess=%d withdrawn=%d", b, pending, available, inProcess, withdrawn)
	}
}

func TestPayoutOnlyFromCompletedOrders(t *testing.T) {
	f := setupCheckout(t)
	base := kaosBase(t, f.pool)
	r1 := 3 * (f.price - base) // kreator = harga jual − harga dasar
	r2 := 2 * (f.price - base)

	wantBalance(t, f.balance(t), 0, 0, 0, 0)

	// dibayar: royalti tercatat tapi masih tertahan
	o1 := f.paidOrder(t, 3)
	wantBalance(t, f.balance(t), r1, 0, 0, 0)

	// tanpa rekening tidak bisa menarik
	if code, out := f.withdraw(PayoutMinimum); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "rekening") {
		t.Errorf("tanpa rekening: %d %v", code, out)
	}
	for _, bad := range []map[string]any{
		{"bankName": "Bank Antah", "account": "1234567890", "holder": "Kreator Tes"},
		{"bankName": "BCA", "account": "12AB", "holder": "Kreator Tes"},
		{"bankName": "BCA", "account": "1234567890", "holder": " "},
	} {
		if rec, out := call(f.s, "PATCH", "/api/designer/bank", bad, f.creator); rec.Code != http.StatusBadRequest {
			t.Errorf("rekening tidak valid %v diterima: %d %v", bad, rec.Code, out)
		}
	}
	if rec, out := call(f.s, "PATCH", "/api/designer/bank", map[string]any{"bankName": "BCA", "account": "123-456 7890", "holder": "Kreator  Tes"}, f.creator); rec.Code != http.StatusOK {
		t.Fatalf("simpan rekening: %d %v", rec.Code, out)
	}
	// pembeli bukan kreator
	if rec, _ := call(f.s, "PATCH", "/api/designer/bank", map[string]any{"bankName": "BCA", "account": "1234567890", "holder": "Budi"}, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli mengisi rekening: %d, want 403", rec.Code)
	}

	// pesanan belum selesai → saldo tersedia 0
	if code, out := f.withdraw(PayoutMinimum); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Melebihi") {
		t.Errorf("menarik saldo tertahan: %d %v", code, out)
	}

	// selesai → bisa ditarik
	f.complete(t, o1)
	o2 := f.paidOrder(t, 2)
	f.complete(t, o2)
	total := r1 + r2
	wantBalance(t, f.balance(t), 0, total, 0, 0)

	if code, out := f.withdraw(PayoutMinimum - 1); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Minimal") {
		t.Errorf("di bawah minimum: %d %v", code, out)
	}
	if code, _ := f.withdraw(total + 1); code != http.StatusBadRequest {
		t.Errorf("melebihi saldo: %d, want 400", code)
	}
	code, out := f.withdraw(60_000)
	if code != http.StatusCreated {
		t.Fatalf("tarik: %d %v", code, out)
	}
	p := out["payout"].(map[string]any)
	bank := p["bank"].(map[string]any)
	if p["status"] != "diminta" || bank["account"] != "1234567890" || bank["holder"] != "Kreator Tes" {
		t.Errorf("pengajuan: %v", p)
	}
	payoutID := int64(num(p["id"]))
	wantBalance(t, f.balance(t), 0, total-60_000, 60_000, 0)

	// satu pengajuan terbuka per kreator
	if code, out := f.withdraw(PayoutMinimum); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "diproses") {
		t.Errorf("pengajuan kedua: %d %v", code, out)
	}

	// admin: kreator & pembeli tidak boleh memproses
	path := "/api/payouts/" + strconv.FormatInt(payoutID, 10)
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "paid", "reference": "X"}, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator memproses sendiri: %d, want 403", rec.Code)
	}
	rec, list := call(f.s, "GET", "/api/payouts?status=terbuka", nil, f.admin)
	if rec.Code != http.StatusOK || !containsPayout(list["payouts"], payoutID) {
		t.Fatalf("antrean admin: %d %v", rec.Code, list)
	}

	// tolak → dana kembali
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "reject"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("tolak tanpa alasan: %d, want 400", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", path, map[string]any{"action": "reject", "note": "Nama rekening tidak cocok"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("tolak: %d %v", rec.Code, out)
	}
	wantBalance(t, f.balance(t), 0, total, 0, 0)
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "paid", "reference": "TRX1"}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("bayar pengajuan yang ditolak: %d, want 409", rec.Code)
	}

	// ajukan ulang seluruh saldo → diproses → dibayar
	code, out = f.withdraw(total)
	if code != http.StatusCreated {
		t.Fatalf("tarik ulang: %d %v", code, out)
	}
	path = "/api/payouts/" + strconv.FormatInt(int64(num(out["payout"].(map[string]any)["id"])), 10)
	if rec, out := call(f.s, "PATCH", path, map[string]any{"action": "process"}, f.admin); rec.Code != http.StatusOK || out["payout"].(map[string]any)["status"] != "diproses" {
		t.Fatalf("proses: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "PATCH", path, map[string]any{"action": "paid"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("dibayar tanpa referensi: %d, want 400", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", path, map[string]any{"action": "paid", "reference": "BCA-TRX-001"}, f.admin); rec.Code != http.StatusOK || out["payout"].(map[string]any)["reference"] != "BCA-TRX-001" {
		t.Fatalf("dibayar: %d %v", rec.Code, out)
	}
	wantBalance(t, f.balance(t), 0, 0, 0, total)
}

func TestPayoutConcurrentRequestsCannotOverdraw(t *testing.T) {
	f := setupCheckout(t)
	o := f.paidOrder(t, 5) // royalti 5 × (123.000 − harga dasar)
	f.complete(t, o)
	if rec, out := call(f.s, "PATCH", "/api/designer/bank", map[string]any{"bankName": "GoPay", "account": "081234567890", "holder": "Kreator Tes"}, f.creator); rec.Code != http.StatusOK {
		t.Fatalf("rekening: %d %v", rec.Code, out)
	}
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = f.withdraw(PayoutMinimum)
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("pengajuan bersamaan lolos %d (kode %v), want tepat 1", ok, codes)
	}
}

func TestPayoutAccess(t *testing.T) {
	f := setupCheckout(t)
	if rec, _ := call(f.s, "GET", "/api/designer/payouts", nil, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli melihat saldo: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/designer/payouts", map[string]any{"amount": PayoutMinimum}, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli menarik: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "GET", "/api/payouts", nil, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator membuka antrean admin: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "GET", "/api/payouts", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tanpa login: %d, want 401", rec.Code)
	}
	// admin boleh melihat saldo kreator tertentu
	if rec, out := call(f.s, "GET", "/api/designer/payouts?designer=d-co-"+f.tag, nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("admin melihat saldo kreator: %d %v", rec.Code, out)
	}
}

func containsPayout(list any, id int64) bool {
	items, _ := list.([]any)
	for _, it := range items {
		if int64(num(it.(map[string]any)["id"])) == id {
			return true
		}
	}
	return false
}
