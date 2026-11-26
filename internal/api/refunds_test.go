package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Tes pembatalan, refund, dan komplain (butuh TEST_DATABASE_URL, lihat
// google_test.go). Midtrans dipalsukan oleh fakeMidtrans (payments_test.go).

// paidViaGateway melunasi pesanan lewat sesi Snap + status settlement dari
// Midtrans palsu, seperti alur sungguhan (sync memanggil Status API).
func (p *payFixture) paidViaGateway(t *testing.T, id string, total int, paymentType string) string {
	t.Helper()
	if code, out := p.session(t, id, p.buyer); code != http.StatusOK {
		t.Fatalf("sesi bayar: %d %v", code, out)
	}
	gw := id + "-1"
	p.fm.set(gw, "settlement", total, paymentType)
	if rec, out := call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, p.buyer); rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %v", rec.Code, out)
	}
	if st, ps := p.orderStatus(t, id); st != "dibayar" || ps != "paid" {
		t.Fatalf("setelah settlement: %s/%s", st, ps)
	}
	return gw
}

// arrive memindahkan pesanan ke status tiba (paket sampai) lewat SQL.
func (f *checkoutFixture) arrive(t *testing.T, id string, deliveredAgo time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE orders SET status='tiba', shipped_courier='JNE REG', tracking_number=$2,
		shipped_at=now() - interval '5 days', delivered_at=now() - make_interval(secs => $3) WHERE id=$1`,
		id, "RF"+strings.ToUpper(f.tag)+id[len(id)-4:], deliveredAgo.Seconds()); err != nil {
		t.Fatal(err)
	}
}

type royaltySums struct{ net, reversed int }

func (f *checkoutFixture) royalties(t *testing.T, id string) (r royaltySums) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COALESCE(sum(r.amount),0)::int, COALESCE(sum(r.reversed_amount),0)::int
		FROM royalties r JOIN order_items oi ON oi.id = r.order_item_id WHERE oi.order_id = $1`, id).Scan(&r.net, &r.reversed); err != nil {
		t.Fatal(err)
	}
	return
}

type chargeSums struct{ production, shipping, rows int }

func (f *checkoutFixture) charges(t *testing.T, id string) (c chargeSums) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COALESCE(sum(amount) FILTER (WHERE kind='produksi'),0)::int, COALESCE(sum(amount) FILTER (WHERE kind='ongkir'),0)::int, count(*)::int
		FROM producer_charges WHERE order_id = $1`, id).Scan(&c.production, &c.shipping, &c.rows); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *checkoutFixture) flow(t *testing.T) map[string]any {
	t.Helper()
	rec, out := call(f.s, "GET", "/api/finance", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("finance: %d %v", rec.Code, out)
	}
	return out["flow"].(map[string]any)
}

func (f *checkoutFixture) owed(t *testing.T) map[string]any {
	t.Helper()
	_, out := call(f.s, "GET", "/api/finance", nil, f.admin)
	return out["owed"].(map[string]any)
}

func TestAdminCancelPaidOrderWithGatewayRefund(t *testing.T) {
	p := setupPayments(t)
	ctx := context.Background()
	base := kaosBase(t, p.pool)
	var cost int
	p.pool.QueryRow(ctx, `SELECT production_cost FROM product_types WHERE id='kaos'`).Scan(&cost)

	// pesanan lain yang sudah selesai: saldo tersedia kreator harus utuh setelah pembatalan
	done := p.paidOrder(t, 1)
	p.complete(t, done)
	available := p.balance(t)["available"]

	flow0, owed0 := p.flow(t), p.owed(t)

	id, total := p.newOrder(t, "") // 2×M + 1×L
	royalty := 3 * (p.price - base)
	gw := p.paidViaGateway(t, id, total, "qris")
	if r := p.royalties(t, id); r.net != royalty || r.reversed != 0 {
		t.Fatalf("royalti sebelum batal: %+v, want net %d", r, royalty)
	}
	if c := p.charges(t, id); c.production != 3*cost || c.rows != 3 {
		t.Fatalf("tagihan produsen sebelum batal: %+v", c)
	}
	if b := p.balance(t); num(b["pending"]) < royalty {
		t.Fatalf("royalti belum tertahan di saldo: %v", b)
	}

	// pembeli tidak bisa membatalkan pesanan yang sudah dibayar, dan tidak bisa merefund
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil, p.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli batal pesanan lunas: %d, want 403", rec.Code)
	}
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"reason": "ingin batal"}, p.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli refund: %d, want 403", rec.Code)
	}
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil, p.other); rec.Code != http.StatusNotFound {
		t.Errorf("akun lain batal: %d, want 404", rec.Code)
	}
	if st, _ := p.orderStatus(t, id); st != "dibayar" {
		t.Fatalf("status berubah oleh permintaan terlarang: %s", st)
	}
	// admin harus menulis alasan
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{}, p.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("tanpa alasan: %d, want 400", rec.Code)
	}

	// qris → refund lewat Midtrans
	_, out := call(p.s, "GET", "/api/orders/"+id, nil, p.admin)
	if mode := out["order"].(map[string]any)["payment"].(map[string]any)["refundMode"]; mode != "partial" {
		t.Errorf("refundMode qris: %v, want partial", mode)
	}
	rec, out := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"reason": "Pembeli salah pesan ukuran"}, p.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("refund: %d %v", rec.Code, out)
	}
	o := out["order"].(map[string]any)
	if o["status"] != "dibatalkan" || num(o["refunded"]) != total {
		t.Errorf("pesanan setelah refund: status=%v refunded=%v", o["status"], o["refunded"])
	}
	refunds := o["refunds"].([]any)
	if len(refunds) != 1 {
		t.Fatalf("baris refunds: %v", refunds)
	}
	rf := refunds[0].(map[string]any)
	if rf["method"] != "gateway" || num(rf["amount"]) != total || !strings.HasPrefix(rf["reference"].(string), "kk-"+id) {
		t.Errorf("refund: %v", rf)
	}
	if len(p.fm.refunds) != 1 || num(p.fm.refunds[0]["amount"]) != total || p.fm.refunds[0]["refund_key"] != rf["reference"] {
		t.Errorf("permintaan ke Midtrans: %v", p.fm.refunds)
	}
	_ = gw

	// buku: royalti dibalik penuh, tagihan produsen dihapus, counter terjual kembali
	if r := p.royalties(t, id); r.net != 0 || r.reversed != royalty {
		t.Errorf("royalti setelah batal: %+v, want net 0 reversed %d", r, royalty)
	}
	if c := p.charges(t, id); c.rows != 0 {
		t.Errorf("tagihan produsen tersisa: %+v", c)
	}
	var sold int
	p.pool.QueryRow(ctx, `SELECT sold FROM listings WHERE id=$1`, p.listing).Scan(&sold)
	if sold != 1 { // hanya pesanan `done` (1 pcs)
		t.Errorf("counter terjual %d, want 1", sold)
	}
	// saldo kreator: royalti yang dibalik tidak tertahan maupun bisa ditarik; saldo lain utuh
	b := p.balance(t)
	if b["available"] != available || num(b["reversed"]) != royalty {
		t.Errorf("saldo setelah batal: %v (available awal %v, reversed want %d)", b, available, royalty)
	}
	if num(b["pending"]) != 0 {
		t.Errorf("royalti yang dibalik masih tertahan: %v", b)
	}
	// pesanan batal tetap tampil di dashboard kreator dengan royalti yang dibalik
	_, dash := call(p.s, "GET", "/api/designer/orders", nil, p.creator)
	sum := dash["summary"].(map[string]any)
	if num(sum["royaltyReversed"]) != royalty {
		t.Errorf("ringkasan kreator: %v", sum)
	}
	// keuangan: refund tampil di total, platform tidak menanggung royalti/produsen pesanan ini
	flow1, owed1 := p.flow(t), p.owed(t)
	d := func(k string) int { return num(flow1[k]) - num(flow0[k]) }
	if d("received") != total || d("refunded") != total || d("refunds") != 1 || d("production") != 0 || d("shippingCost") != 0 || d("creators") != 0 {
		t.Errorf("selisih keuangan: received %d refunded %d refunds %d production %d ship %d creators %d",
			d("received"), d("refunded"), d("refunds"), d("production"), d("shippingCost"), d("creators"))
	}
	if d("creatorsReversed") != royalty {
		t.Errorf("creatorsReversed %d, want %d", d("creatorsReversed"), royalty)
	}
	if d("platformIncome") != -d("gatewayFees") { // yang hilang hanya biaya gateway (tidak dikembalikan Midtrans)
		t.Errorf("platformIncome %d vs biaya gateway %d", d("platformIncome"), d("gatewayFees"))
	}
	if num(flow1["platformIncome"]) != num(flow1["received"])-num(flow1["refunded"])-num(flow1["production"])-num(flow1["shippingCost"])-num(flow1["creators"])-num(flow1["gatewayFees"]) {
		t.Errorf("rumus laporan tidak konsisten: %v", flow1)
	}
	if num(owed1["cashEstimate"])-num(owed0["cashEstimate"]) != 0-d("gatewayFees") {
		t.Errorf("kas: selisih %d, want %d", num(owed1["cashEstimate"])-num(owed0["cashEstimate"]), -d("gatewayFees"))
	}
	if num(owed1["producersInProduction"]) != num(owed0["producersInProduction"]) || num(owed1["creatorsHeld"]) != num(owed0["creatorsHeld"]) {
		t.Errorf("kewajiban berubah oleh pesanan batal: %v → %v", owed0, owed1)
	}
	// tidak bisa direfund dua kali
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"reason": "sekali lagi"}, p.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("refund ganda: %d, want 400", rec.Code)
	}
	if len(p.fm.refunds) != 1 {
		t.Errorf("refund ganda sampai ke Midtrans: %d", len(p.fm.refunds))
	}
	// timeline
	var label string
	p.pool.QueryRow(ctx, `SELECT label FROM order_events WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, id).Scan(&label)
	if !strings.Contains(label, "dibatalkan") || !strings.Contains(label, "Midtrans") {
		t.Errorf("event terakhir: %q", label)
	}
}

func TestRefundManualAndGatewayFailure(t *testing.T) {
	p := setupPayments(t)

	// transfer bank (VA): tidak bisa direfund lewat API → manual
	id, total := p.newOrder(t, "")
	p.paidViaGateway(t, id, total, "bank_transfer")
	_, out := call(p.s, "GET", "/api/orders/"+id, nil, p.admin)
	if mode, has := out["order"].(map[string]any)["payment"].(map[string]any)["refundMode"]; has && mode != "" {
		t.Errorf("refundMode VA: %v, want kosong", mode)
	}
	if rec, out := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"method": "gateway", "reason": "batal"}, p.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("refund gateway untuk VA: %d %v, want 400", rec.Code, out)
	}
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"method": "manual", "reason": "batal"}, p.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("manual tanpa referensi: %d, want 400", rec.Code)
	}
	if st, _ := p.orderStatus(t, id); st != "dibayar" {
		t.Fatalf("pesanan berubah oleh refund yang ditolak: %s", st)
	}
	if len(p.fm.refunds) != 0 {
		t.Fatalf("Midtrans dipanggil untuk metode yang tidak didukung")
	}
	rec, out := call(p.s, "POST", "/api/orders/"+id+"/refund", map[string]any{"reason": "Stok habis", "reference": "BCA-RF-0001"}, p.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("refund manual: %d %v", rec.Code, out)
	}
	rf := out["order"].(map[string]any)["refunds"].([]any)[0].(map[string]any)
	if rf["method"] != "manual" || rf["reference"] != "BCA-RF-0001" || num(rf["amount"]) != total {
		t.Errorf("refund manual: %v", rf)
	}
	if r := p.royalties(t, id); r.net != 0 {
		t.Errorf("royalti tersisa: %+v", r)
	}

	// Midtrans menolak (412) → semuanya dibatalkan, pesanan tetap dibayar, bisa dicoba lagi
	id2, total2 := p.newOrder(t, "")
	p.paidViaGateway(t, id2, total2, "gopay")
	p.fm.refundFails = true
	rec, out = call(p.s, "POST", "/api/orders/"+id2+"/refund", map[string]any{"reason": "batal"}, p.admin)
	if rec.Code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(out["error"]), "Midtrans") {
		t.Fatalf("refund ditolak gateway: %d %v", rec.Code, out)
	}
	if st, _ := p.orderStatus(t, id2); st != "dibayar" {
		t.Errorf("status setelah refund gagal: %s", st)
	}
	if r := p.royalties(t, id2); r.reversed != 0 || r.net == 0 {
		t.Errorf("royalti berubah walau refund gagal: %+v", r)
	}
	if c := p.charges(t, id2); c.rows != 3 {
		t.Errorf("tagihan produsen berubah walau refund gagal: %+v", c)
	}
	var n int
	p.pool.QueryRow(context.Background(), `SELECT count(*) FROM refunds WHERE order_id=$1`, id2).Scan(&n)
	if n != 0 {
		t.Errorf("baris refunds tercatat walau gagal: %d", n)
	}
	// pesanan yang sudah produksi tidak bisa dibatalkan dengan refund
	p.fm.refundFails = false
	p.pool.Exec(context.Background(), `UPDATE orders SET status='produksi' WHERE id=$1`, id2)
	if rec, _ := call(p.s, "POST", "/api/orders/"+id2+"/refund", map[string]any{"reason": "batal"}, p.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("refund pesanan produksi: %d, want 400", rec.Code)
	}
	if len(p.fm.refunds) != 0 {
		t.Errorf("Midtrans dipanggil untuk pesanan produksi")
	}
}

func TestBuyerCancelsUnpaidOrder(t *testing.T) {
	p := setupPayments(t)
	ctx := context.Background()
	code := "T" + strings.ToUpper(p.tag) + "CAN"
	p.pool.Exec(ctx, `INSERT INTO vouchers (code, kind, value, usage_limit, per_user_limit) VALUES ($1,'fixed',10000,1,1)`, code)

	id, _ := p.newOrder(t, code)
	p.session(t, id, p.buyer)
	gw := id + "-1"
	p.fm.set(gw, "pending", 1, "qris")
	p.fm.mu.Lock()
	p.fm.status[gw]["gross_amount"] = fmt.Sprintf("%d.00", p.orderTotal(t, id))
	p.fm.mu.Unlock()

	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tanpa login: %d, want 401", rec.Code)
	}
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil, p.other); rec.Code != http.StatusNotFound {
		t.Errorf("akun lain: %d, want 404", rec.Code)
	}
	rec, out := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil, p.buyer)
	if rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "dibatalkan" {
		t.Fatalf("batal: %d %v", rec.Code, out)
	}
	if len(p.fm.cancels) != 1 || p.fm.cancels[0] != gw {
		t.Errorf("transaksi Midtrans tidak dibatalkan: %v", p.fm.cancels)
	}
	if _, ps := p.orderStatus(t, id); ps != "expired" {
		t.Errorf("status pembayaran: %s, want expired", ps)
	}
	// voucher bebas, pesanan tidak bisa dibayar lagi, batal dua kali ditolak
	if _, out := call(p.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{p.line("M", 1)}, "courier": "JNE REG", "voucher": code}, p.other); quoteOf(t, out)["voucherError"] != nil {
		t.Errorf("voucher belum bebas: %v", quoteOf(t, out)["voucherError"])
	}
	if c, _ := p.session(t, id, p.buyer); c != http.StatusBadRequest {
		t.Errorf("sesi bayar setelah batal: %d, want 400", c)
	}
	if rec, _ := call(p.s, "POST", "/api/orders/"+id+"/cancel", nil, p.buyer); rec.Code != http.StatusConflict {
		t.Errorf("batal ganda: %d, want 409", rec.Code)
	}

	// pesanan tanpa percobaan bayar (belum ada transaksi di Midtrans) juga bisa dibatalkan
	id2, _ := p.newOrder(t, "")
	if rec, _ := call(p.s, "POST", "/api/orders/"+id2+"/cancel", nil, p.buyer); rec.Code != http.StatusOK {
		t.Errorf("batal tanpa sesi: %d", rec.Code)
	}

	// pembayaran ternyata sudah masuk di Midtrans → tidak dibatalkan, malah dilunasi
	id3, total3 := p.newOrder(t, "")
	p.session(t, id3, p.buyer)
	p.fm.set(id3+"-1", "settlement", total3, "qris")
	if rec, out := call(p.s, "POST", "/api/orders/"+id3+"/cancel", nil, p.buyer); rec.Code != http.StatusConflict {
		t.Errorf("batal saat sudah dibayar di gateway: %d %v, want 409", rec.Code, out)
	}
	if st, ps := p.orderStatus(t, id3); st != "dibayar" || ps != "paid" {
		t.Errorf("pesanan seharusnya lunas: %s/%s", st, ps)
	}
	if len(p.fm.cancels) != 1 {
		t.Errorf("Cancel API dipanggil untuk transaksi settlement: %v", p.fm.cancels)
	}
}

func (p *payFixture) orderTotal(t *testing.T, id string) (total int) {
	t.Helper()
	p.pool.QueryRow(context.Background(), `SELECT total FROM orders WHERE id=$1`, id).Scan(&total)
	return
}

func photoBody(reason, desc string, photos ...string) map[string]any {
	return map[string]any{"reason": reason, "description": desc, "photos": photos}
}

func TestComplaintPausesAutoCompleteAndRejection(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	id := f.paidOrder(t, 2)
	path := "/api/orders/" + id + "/complaint"
	good := photoBody("cacat-cetak", "Hasil cetak luntur setelah dicuci sekali", "/uploads/d-000001-0001.jpg", "/uploads/d-000002-0002.png")

	// belum tiba → belum bisa komplain
	if rec, _ := call(f.s, "POST", path, good, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("komplain sebelum tiba: %d, want 400", rec.Code)
	}
	f.arrive(t, id, 3*24*time.Hour) // sudah lewat batas 48 jam

	// bukan pembeli
	if rec, _ := call(f.s, "POST", path, good, f.other); rec.Code != http.StatusNotFound {
		t.Errorf("komplain akun lain: %d, want 404", rec.Code)
	}
	if rec, _ := call(f.s, "POST", path, good, f.admin); rec.Code != http.StatusForbidden {
		t.Errorf("komplain oleh admin: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "POST", path, good); rec.Code != http.StatusUnauthorized {
		t.Errorf("komplain tanpa login: %d, want 401", rec.Code)
	}
	// validasi
	for name, body := range map[string]map[string]any{
		"alasan salah":     photoBody("bebas", "Hasil cetak luntur setelah dicuci"),
		"deskripsi pendek": photoBody("rusak", "rusak"),
		"4 foto":           photoBody("rusak", "Kardus penyok dan produk sobek", "/uploads/a.jpg", "/uploads/b.jpg", "/uploads/c.jpg", "/uploads/d.jpg"),
		"foto javascript":  photoBody("rusak", "Kardus penyok dan produk sobek", "javascript:alert(1)"),
	} {
		if rec, _ := call(f.s, "POST", path, body, f.buyer); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	rec, out := call(f.s, "POST", path, good, f.buyer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("komplain: %d %v", rec.Code, out)
	}
	cp := out["order"].(map[string]any)["complaint"].(map[string]any)
	if cp["status"] != "terbuka" || len(cp["photos"].([]any)) != 2 || out["order"].(map[string]any)["autoCompleteAt"] != nil {
		t.Errorf("komplain: %v / autoCompleteAt=%v", cp, out["order"].(map[string]any)["autoCompleteAt"])
	}
	if rec, _ := call(f.s, "POST", path, good, f.buyer); rec.Code != http.StatusConflict {
		t.Errorf("komplain ganda: %d, want 409", rec.Code)
	}

	// komplain terbuka menahan selesai-otomatis dan tombol konfirmasi
	if n, err := f.s.AutoCompleteOrders(ctx, AutoCompleteAfter); err != nil || n != 0 {
		var st string
		f.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
		if st != "tiba" {
			t.Errorf("selesai otomatis walau ada komplain terbuka: n=%d err=%v status=%s", n, err, st)
		}
	}
	var st string
	f.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
	if st != "tiba" {
		t.Fatalf("status %s, want tiba", st)
	}
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]any{"action": "confirm"}, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("konfirmasi saat komplain terbuka: %d, want 400", rec.Code)
	}

	// admin: hanya admin yang bisa memutuskan; penolakan wajib catatan
	cpID := fmt.Sprint(num(cp["id"]))
	if rec, _ := call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "reject", "note": "x"}, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli memutuskan komplain: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "reject"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("tolak tanpa catatan: %d, want 400", rec.Code)
	}
	if rec, out := call(f.s, "GET", "/api/complaints?status=terbuka", nil, f.admin); rec.Code != http.StatusOK || len(out["complaints"].([]any)) < 1 {
		t.Errorf("daftar komplain admin: %d %v", rec.Code, out)
	}
	rec, out = call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "reject", "note": "Foto menunjukkan pemakaian normal"}, f.admin)
	if rec.Code != http.StatusOK || out["complaint"].(map[string]any)["status"] != "ditolak" {
		t.Fatalf("tolak: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "reject", "note": "lagi"}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("putuskan dua kali: %d, want 409", rec.Code)
	}

	// setelah ditolak, hitungan 48 jam dimulai dari waktu keputusan (bukan waktu tiba)
	if n, _ := f.s.AutoCompleteOrders(ctx, AutoCompleteAfter); n != 0 {
		var st string
		f.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
		if st != "tiba" {
			t.Errorf("selesai otomatis terlalu cepat setelah penolakan: %s", st)
		}
	}
	_, got := call(f.s, "GET", "/api/orders/"+id, nil, f.buyer)
	o := got["order"].(map[string]any)
	if o["status"] != "tiba" || o["autoCompleteAt"] == nil || int64(num(o["autoCompleteAt"])) < time.Now().Add(47*time.Hour).UnixMilli() {
		t.Errorf("autoCompleteAt setelah ditolak: %v (status %v)", o["autoCompleteAt"], o["status"])
	}
	f.pool.Exec(ctx, `UPDATE complaints SET resolved_at = now() - interval '3 days', created_at = now() - interval '4 days' WHERE id=$1`, num(cp["id"]))
	if n, err := f.s.AutoCompleteOrders(ctx, AutoCompleteAfter); err != nil || n < 1 {
		t.Fatalf("selesai otomatis setelah 48 jam dari keputusan: n=%d err=%v", n, err)
	}
	f.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
	if st != "selesai" {
		t.Errorf("status akhir %s, want selesai", st)
	}
}

func TestComplaintReprintCreatesLinkedFreeOrder(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	var cost int
	f.pool.QueryRow(ctx, `SELECT production_cost FROM product_types WHERE id='kaos'`).Scan(&cost)

	_, out := f.order(t, f.buyer, "", f.line("M", 2), f.line("L", 1))
	o0 := out["order"].(map[string]any)
	id := o0["id"].(string)
	shipping := num(o0["shipping"].(map[string]any)["cost"])
	if err := f.s.payOrder(ctx, id, "QRIS"); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, id, time.Hour)
	orig := f.royalties(t, id)
	flow0 := f.flow(t)

	rec, out := call(f.s, "POST", "/api/orders/"+id+"/complaint", photoBody("salah-produk", "Ukuran yang datang L padahal pesan M"), f.buyer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("komplain: %d %v", rec.Code, out)
	}
	cpID := fmt.Sprint(num(out["order"].(map[string]any)["complaint"].(map[string]any)["id"]))
	rec, out = call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "reprint", "note": "Cetak ulang ukuran M"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("cetak ulang: %d %v", rec.Code, out)
	}
	cp := out["complaint"].(map[string]any)
	rid, _ := cp["reprintOrderId"].(string)
	if cp["status"] != "dicetak-ulang" || rid == "" || rid == id {
		t.Fatalf("komplain setelah cetak ulang: %v", cp)
	}

	// pesanan pengganti: tertaut, total 0, item sama, status dibayar
	_, got := call(f.s, "GET", "/api/orders/"+rid, nil, f.buyer)
	ro := got["order"].(map[string]any)
	if ro["reprintOf"] != id || num(ro["total"]) != 0 || num(ro["subtotal"]) != 0 || ro["status"] != "dibayar" || len(ro["items"].([]any)) != 2 {
		t.Fatalf("pesanan cetak ulang: %v", ro)
	}
	if ro["customer"].(map[string]any)["address"] != testCustomer["address"] {
		t.Errorf("alamat tidak tersalin: %v", ro["customer"])
	}
	// buku: tagihan produsen ada (produksi + ongkir), royalti tidak ada, biaya gateway 0
	if c := f.charges(t, rid); c.production != 3*cost || c.shipping != shipping || c.rows != 3 {
		t.Errorf("tagihan cetak ulang: %+v, want produksi %d ongkir %d", c, 3*cost, shipping)
	}
	if r := f.royalties(t, rid); r.net != 0 || r.reversed != 0 {
		t.Errorf("cetak ulang menghasilkan royalti: %+v", r)
	}
	var fee, amount int
	f.pool.QueryRow(ctx, `SELECT gateway_fee, amount FROM payments WHERE order_id=$1`, rid).Scan(&fee, &amount)
	if fee != 0 || amount != 0 {
		t.Errorf("pembayaran cetak ulang: fee %d amount %d", fee, amount)
	}
	// pesanan asal tidak berubah: royalti utuh, tetap tiba
	if r := f.royalties(t, id); r != orig {
		t.Errorf("royalti pesanan asal berubah: %+v → %+v", orig, r)
	}
	flow1 := f.flow(t)
	if num(flow1["reprints"])-num(flow0["reprints"]) != 1 || num(flow1["reprintCost"])-num(flow0["reprintCost"]) != 3*cost+shipping {
		t.Errorf("laporan cetak ulang: %v → %v", flow0, flow1)
	}
	if num(flow1["orders"]) != num(flow0["orders"]) || num(flow1["received"]) != num(flow0["received"]) {
		t.Errorf("cetak ulang dihitung sebagai penjualan: %v → %v", flow0, flow1)
	}
	if d := num(flow1["platformIncome"]) - num(flow0["platformIncome"]); d != -(3*cost + shipping) {
		t.Errorf("biaya cetak ulang pada laba platform: %d, want %d", d, -(3*cost + shipping))
	}

	// berjalan normal: dibayar → produksi → dikirim, pembeli tidak bisa menandai lunas / membatalkannya
	if rec, _ := call(f.s, "POST", "/api/orders/"+rid+"/cancel", nil, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli batal pesanan cetak ulang: %d, want 403", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", "/api/orders/"+rid, map[string]any{"action": "advance"}, f.admin); rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "produksi" {
		t.Fatalf("advance: %d %v", rec.Code, out)
	}
	tracking := "RP" + strings.ToUpper(f.tag) + rid[len(rid)-4:]
	if rec, out := call(f.s, "PATCH", "/api/orders/"+rid, map[string]any{"action": "ship", "courier": "JNE REG", "trackingNumber": tracking}, f.admin); rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "dikirim" {
		t.Fatalf("ship: %d %v", rec.Code, out)
	}
	// tagihan produsen cetak ulang ikut menjadi hak produsen yang siap dibayar
	if num(f.owed(t)["producersReady"]) < 3*cost+shipping {
		t.Errorf("tagihan cetak ulang tidak siap dibayar: %v", f.owed(t))
	}
	// pesanan asal: keputusan sudah diambil → menunggu 48 jam dari keputusan lalu selesai (bukan langsung)
	if n, _ := f.s.AutoCompleteOrders(ctx, AutoCompleteAfter); n != 0 {
		var st string
		f.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
		if st != "tiba" {
			t.Errorf("pesanan asal selesai terlalu cepat: %s", st)
		}
	}
}

func TestComplaintPartialRefundReversesRoyaltyProportionally(t *testing.T) {
	p := setupPayments(t)
	ctx := context.Background()
	base := kaosBase(t, p.pool)

	id, total := p.newOrder(t, "")
	p.paidViaGateway(t, id, total, "qris")
	royalty := 3 * (p.price - base)
	chargesBefore := p.charges(t, id)
	p.arrive(t, id, 3*24*time.Hour)
	_, out := call(p.s, "POST", "/api/orders/"+id+"/complaint", photoBody("cacat-cetak", "Gambar tercetak miring dan buram"), p.buyer)
	cpID := fmt.Sprint(num(out["order"].(map[string]any)["complaint"].(map[string]any)["id"]))

	half := total / 2
	// nominal tidak masuk akal
	for _, amt := range []int{0, -5, total + 1} {
		if rec, _ := call(p.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "refund", "amount": amt}, p.admin); rec.Code != http.StatusBadRequest {
			t.Errorf("nominal %d: %d, want 400", amt, rec.Code)
		}
	}
	if len(p.fm.refunds) != 0 {
		t.Fatalf("Midtrans dipanggil untuk nominal tidak sah")
	}
	rec, out := call(p.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "refund", "amount": half, "note": "Ganti rugi sebagian"}, p.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("refund sebagian: %d %v", rec.Code, out)
	}
	cp := out["complaint"].(map[string]any)
	if cp["status"] != "direfund" || num(cp["refundAmount"]) != half {
		t.Errorf("komplain: %v", cp)
	}
	if len(p.fm.refunds) != 1 || num(p.fm.refunds[0]["amount"]) != half {
		t.Fatalf("permintaan refund ke Midtrans: %v", p.fm.refunds)
	}
	// royalti dibalik sebanding: round(royalty × half / total) per baris
	wantRev := 0
	rows, _ := p.pool.Query(ctx, `SELECT r.amount + r.reversed_amount, r.reversed_amount FROM royalties r JOIN order_items oi ON oi.id=r.order_item_id WHERE oi.order_id=$1`, id)
	for rows.Next() {
		var orig, rev int
		rows.Scan(&orig, &rev)
		want := int(float64(orig)*float64(half)/float64(total) + 0.5)
		if rev != want {
			t.Errorf("baris royalti: dibalik %d, want %d (asli %d)", rev, want, orig)
		}
		wantRev += want
	}
	rows.Close()
	if r := p.royalties(t, id); r.reversed != wantRev || r.net+r.reversed != royalty {
		t.Errorf("royalti: %+v, want reversed %d total %d", r, wantRev, royalty)
	}
	// barang sudah dicetak & dikirim: tagihan produsen tidak berubah
	if c := p.charges(t, id); c != chargesBefore {
		t.Errorf("tagihan produsen berubah oleh refund komplain: %+v → %+v", chargesBefore, c)
	}
	// pesanan tetap tiba; setelah 48 jam dari keputusan, selesai & royalti bersih jadi saldo tersedia
	var st string
	p.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st)
	if st != "tiba" {
		t.Fatalf("status %s, want tiba", st)
	}
	before := p.balance(t)
	p.pool.Exec(ctx, `UPDATE complaints SET resolved_at = now() - interval '3 days', created_at = now() - interval '4 days' WHERE id=$1`, num(cp["id"]))
	if n, err := p.s.AutoCompleteOrders(ctx, AutoCompleteAfter); err != nil || n < 1 {
		t.Fatalf("selesai otomatis: n=%d err=%v", n, err)
	}
	after := p.balance(t)
	if num(after["available"])-num(before["available"]) != royalty-wantRev || num(after["reversed"]) != wantRev {
		t.Errorf("saldo: sebelum %v sesudah %v, want tersedia +%d", before, after, royalty-wantRev)
	}
	// kreator bisa menarik tepat sebesar sisa bersih, tidak lebih
	p.pool.Exec(ctx, `UPDATE designers SET bank_name='BCA', bank_account='1234567890', bank_holder='Kreator Tes' WHERE id=$1`, "d-co-"+p.tag)
	if code, _ := p.withdraw(num(after["available"]) + 1); code != http.StatusBadRequest {
		t.Errorf("tarik melebihi saldo bersih: %d, want 400", code)
	}

	// sisa dana tidak bisa melebihi total: refund kedua (sisanya) lewat endpoint pesanan juga ditolak karena bukan `dibayar`;
	// komplain sudah diputuskan jadi tidak bisa direfund lagi
	if rec, _ := call(p.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "refund", "amount": total - half}, p.admin); rec.Code != http.StatusConflict {
		t.Errorf("refund kedua pada komplain yang sama: %d, want 409", rec.Code)
	}
}

func TestComplaintRefundCumulativeNeverExceedsPaid(t *testing.T) {
	f := setupCheckout(t) // mock: refund manual
	ctx := context.Background()
	base := kaosBase(t, f.pool)
	id := f.paidOrder(t, 3)
	var total int
	f.pool.QueryRow(ctx, `SELECT total FROM orders WHERE id=$1`, id).Scan(&total)
	royalty := 3 * (f.price - base)
	f.arrive(t, id, time.Hour)
	_, out := call(f.s, "POST", "/api/orders/"+id+"/complaint", photoBody("rusak", "Produk sobek saat dibuka dari kemasan"), f.buyer)
	cpID := fmt.Sprint(num(out["order"].(map[string]any)["complaint"].(map[string]any)["id"]))

	// refund penuh lewat komplain tanpa referensi manual ditolak
	if rec, _ := call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "refund", "amount": total}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("manual tanpa referensi: %d, want 400", rec.Code)
	}
	rec, out := call(f.s, "PATCH", "/api/complaints/"+cpID, map[string]any{"action": "refund", "amount": total, "reference": "BRI-RF-77"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("refund penuh: %d %v", rec.Code, out)
	}
	if r := f.royalties(t, id); r.net != 0 || r.reversed != royalty {
		t.Errorf("refund penuh: royalti %+v, want net 0 reversed %d", r, royalty)
	}
	// refund tambahan lewat jalur apa pun tidak bisa melebihi dana yang dibayar
	_, got := call(f.s, "GET", "/api/orders/"+id, nil, f.admin)
	if o := got["order"].(map[string]any); num(o["refunded"]) != total || o["complaint"].(map[string]any)["status"] != "direfund" {
		t.Errorf("pesanan: %v", o)
	}
}
