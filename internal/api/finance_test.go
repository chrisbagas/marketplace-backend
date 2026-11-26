package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"karyakita/api/internal/db"
)

// Tes integrasi pembagian uang per pesanan & pembayaran produsen
// (butuh TEST_DATABASE_URL, lihat google_test.go).

func TestGatewayFeeEstimate(t *testing.T) {
	cases := []struct {
		method, paymentType string
		want                int
	}{
		{"QRIS", "qris", 700},
		{"VA BCA", "bank_transfer", 4000},
		{"VA Mandiri", "", 4000},
		{"GoPay", "gopay", 2000},
		{"OVO", "", 2000},
		{"Manual (admin) manual", "", 0},
		{"Kartu kredit", "credit_card", 2900 + 2000},
	}
	for _, c := range cases {
		if got := db.GatewayFee(c.method, c.paymentType, 100_000); got != c.want {
			t.Errorf("GatewayFee(%q, %q) = %d, want %d", c.method, c.paymentType, got, c.want)
		}
	}
}

func TestOrderMoneySplitAndProducerPayout(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	producer := "pr-test-" + f.tag
	if _, err := f.pool.Exec(ctx, `INSERT INTO producers (id, name) VALUES ($1, 'Vendor Tes')`, producer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM producer_charges WHERE producer_id = $1`, producer)
		f.pool.Exec(ctx, `DELETE FROM producer_payouts WHERE producer_id = $1`, producer)
		f.pool.Exec(ctx, `UPDATE order_items SET producer_id = NULL WHERE producer_id = $1`, producer)
		f.pool.Exec(ctx, `DELETE FROM producers WHERE id = $1`, producer)
	})
	var base, cost int
	if err := f.pool.QueryRow(ctx, `SELECT base_price, production_cost FROM product_types WHERE id='kaos'`).Scan(&base, &cost); err != nil {
		t.Fatal(err)
	}

	rec, out := f.order(t, f.buyer, "", f.line("M", 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan: %d %v", rec.Code, out)
	}
	o := out["order"].(map[string]any)
	id := o["id"].(string)
	shipping := num(o["shipping"].(map[string]any)["cost"])
	total := num(o["total"])

	// tarif tersalin ke item saat checkout; arahkan ke vendor tes supaya terpisah dari data lain
	var snapBase, snapCost int
	if err := f.pool.QueryRow(ctx, `UPDATE order_items SET producer_id=$2 WHERE order_id=$1 RETURNING base_price, production_cost`, id, producer).Scan(&snapBase, &snapCost); err != nil {
		t.Fatal(err)
	}
	if snapBase != base || snapCost != cost {
		t.Errorf("salinan tarif: base %d cost %d, want %d %d", snapBase, snapCost, base, cost)
	}

	for range 2 { // pembayaran ganda (notifikasi berulang) tidak menggandakan apa pun
		if err := f.s.payOrder(ctx, id, "QRIS"); err != nil {
			t.Fatal(err)
		}
	}
	var royalty, production, ship, charges, fee int
	f.pool.QueryRow(ctx, `SELECT COALESCE(sum(r.amount),0) FROM royalties r JOIN order_items oi ON oi.id=r.order_item_id WHERE oi.order_id=$1`, id).Scan(&royalty)
	f.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER (WHERE kind='produksi'),0), COALESCE(sum(amount) FILTER (WHERE kind='ongkir'),0), count(*)
		FROM producer_charges WHERE order_id=$1`, id).Scan(&production, &ship, &charges)
	f.pool.QueryRow(ctx, `SELECT gateway_fee FROM payments WHERE order_id=$1`, id).Scan(&fee)
	if royalty != 2*(f.price-base) {
		t.Errorf("kreator %d, want 2×(%d−%d)", royalty, f.price, base)
	}
	if production != 2*cost || ship != shipping || charges != 2 {
		t.Errorf("produsen: produksi %d ongkir %d (%d baris), want %d %d (2 baris)", production, ship, charges, 2*cost, shipping)
	}
	if fee != db.GatewayFee("QRIS", "", total) {
		t.Errorf("biaya gateway %d", fee)
	}
	// sisanya milik platform: margin harga dasar dikurangi biaya gateway
	if platform := total - royalty - production - ship - fee; platform != 2*(base-cost)-fee {
		t.Errorf("platform %d, want %d", platform, 2*(base-cost)-fee)
	}

	// laporan admin memuat pesanan ini dan rumusnya konsisten
	rec, rep := call(f.s, "GET", "/api/finance", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("laporan: %d %v", rec.Code, rep)
	}
	flow := rep["flow"].(map[string]any)
	if num(flow["platformIncome"]) != num(flow["received"])-num(flow["production"])-num(flow["shippingCost"])-num(flow["creators"])-num(flow["gatewayFees"]) {
		t.Errorf("rumus laporan tidak konsisten: %v", flow)
	}
	if rec, _ := call(f.s, "GET", "/api/finance", nil, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator membuka laporan: %d, want 403", rec.Code)
	}

	// belum dikirim → belum bisa dibayar ke produsen
	payPath := "/api/producers/" + producer + "/payouts"
	if rec, out := call(f.s, "POST", payPath, map[string]any{"reference": "TRX-1"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bayar sebelum dikirim: %d %v", rec.Code, out)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status='dikirim', shipped_courier='JNE REG', tracking_number=$2, shipped_at=now() WHERE id=$1`,
		id, "FIN"+strings.ToUpper(f.tag)+id[len(id)-4:]); err != nil {
		t.Fatal(err)
	}
	if rec, _ := call(f.s, "POST", payPath, map[string]any{}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bayar tanpa referensi: %d, want 400", rec.Code)
	}
	rec, out = call(f.s, "POST", payPath, map[string]any{"reference": "BCA-VENDOR-01"}, f.admin)
	if rec.Code != http.StatusCreated || num(out["payout"].(map[string]any)["amount"]) != 2*cost+shipping {
		t.Fatalf("bayar produsen: %d %v, want nominal %d", rec.Code, out, 2*cost+shipping)
	}
	var open int
	f.pool.QueryRow(ctx, `SELECT count(*) FROM producer_charges WHERE producer_id=$1 AND payout_id IS NULL`, producer).Scan(&open)
	if open != 0 {
		t.Errorf("tagihan belum lunas: %d baris", open)
	}
	if rec, _ := call(f.s, "POST", payPath, map[string]any{"reference": "BCA-VENDOR-02"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bayar dua kali: %d, want 400", rec.Code)
	}
}

func TestProductionCostValidation(t *testing.T) {
	f := setupCheckout(t)
	var base, cost int
	f.pool.QueryRow(context.Background(), `SELECT base_price, production_cost FROM product_types WHERE id='kaos'`).Scan(&base, &cost)
	if rec, _ := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"productionCost": base + 1}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("biaya di atas harga dasar: %d, want 400", rec.Code)
	}
	if rec, _ := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"productionCost": cost}, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator mengubah tarif: %d, want 403", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"productionCost": cost}, f.admin); rec.Code != http.StatusOK {
		t.Errorf("simpan tarif sama: %d %v", rec.Code, out)
	}
}
