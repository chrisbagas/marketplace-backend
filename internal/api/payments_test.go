package api

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"karyakita/api/internal/midtrans"
)

// Tes pembayaran Midtrans dengan server Midtrans palsu (butuh TEST_DATABASE_URL).

const testServerKey = "SB-Mid-server-test-key"

// fakeMidtrans meniru Snap (buat transaksi) dan Status API.
type fakeMidtrans struct {
	mu       sync.Mutex
	srv      *httptest.Server
	created  []map[string]any             // body setiap permintaan Snap
	status   map[string]map[string]string // order_id gateway → field status
	badAuth  int
	snapCall int
}

func newFakeMidtrans(t *testing.T) *fakeMidtrans {
	f := &fakeMidtrans{status: map[string]map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if u, _, ok := r.BasicAuth(); !ok || u != testServerKey {
			f.badAuth++
			http.Error(w, `{"status_message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/snap/v1/transactions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			td := body["transaction_details"].(map[string]any)
			id := td["order_id"].(string)
			for _, c := range f.created {
				if c["transaction_details"].(map[string]any)["order_id"] == id {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error_messages":["transaction_details.order_id has already been utilized."]}`)
					return
				}
			}
			f.created = append(f.created, body)
			f.snapCall++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"tok-%s","redirect_url":"https://pay.test/%s"}`, id, id)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/") && strings.HasSuffix(r.URL.Path, "/status"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/status")
			st, ok := f.status[id]
			if !ok {
				fmt.Fprint(w, `{"status_code":"404","status_message":"Transaction doesn't exist."}`)
				return
			}
			json.NewEncoder(w).Encode(st)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMidtrans) client() *midtrans.Client {
	c := midtrans.New(testServerKey, "SB-Mid-client-test", false)
	c.SnapURL, c.APIURL = f.srv.URL, f.srv.URL
	return c
}

// set mengatur status transaksi yang akan dilaporkan Status API.
func (f *fakeMidtrans) set(gwOrderID, status string, gross int, paymentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[gwOrderID] = map[string]string{
		"order_id": gwOrderID, "transaction_status": status, "status_code": "200",
		"gross_amount": fmt.Sprintf("%d.00", gross), "payment_type": paymentType, "fraud_status": "accept",
	}
}

func (f *fakeMidtrans) snapRequests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.created...)
}

func signed(n map[string]string) map[string]string {
	sum := sha512.Sum512([]byte(n["order_id"] + n["status_code"] + n["gross_amount"] + testServerKey))
	out := map[string]string{}
	for k, v := range n {
		out[k] = v
	}
	out["signature_key"] = hex.EncodeToString(sum[:])
	return out
}

func (f *fakeMidtrans) webhook(t *testing.T, s *Server, n map[string]string) int {
	t.Helper()
	b, _ := json.Marshal(n)
	req := httptest.NewRequest("POST", "/api/payments/midtrans/notification", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

type payFixture struct {
	*checkoutFixture
	fm *fakeMidtrans
}

func setupPayments(t *testing.T) *payFixture {
	f := setupCheckout(t)
	fm := newFakeMidtrans(t)
	f.s.SetPayments(PaymentsConfig{Provider: "midtrans", Midtrans: fm.client()})
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `DELETE FROM payment_notifications WHERE order_id IN (SELECT id FROM orders WHERE user_id LIKE 'co\_%')`)
	})
	return &payFixture{f, fm}
}

func (p *payFixture) newOrder(t *testing.T, voucher string) (id string, total int) {
	t.Helper()
	rec, out := p.order(t, p.buyer, voucher, p.line("M", 2), p.line("L", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	o := out["order"].(map[string]any)
	return o["id"].(string), num(o["total"])
}

func (p *payFixture) session(t *testing.T, id string, cookie *http.Cookie) (int, map[string]any) {
	t.Helper()
	rec, out := call(p.s, "POST", "/api/orders/"+id+"/payment", nil, cookie)
	return rec.Code, out
}

func (p *payFixture) orderStatus(t *testing.T, id string) (order, payment string) {
	t.Helper()
	p.pool.QueryRow(context.Background(), `SELECT o.status::text, p.status::text FROM orders o JOIN payments p ON p.order_id=o.id WHERE o.id=$1`, id).Scan(&order, &payment)
	return
}

func (p *payFixture) royaltyRows(t *testing.T, id string) (n int) {
	p.pool.QueryRow(context.Background(), `SELECT count(*) FROM royalties r JOIN order_items oi ON oi.id=r.order_item_id WHERE oi.order_id=$1`, id).Scan(&n)
	return
}

func TestPaymentConfigAndPayRestriction(t *testing.T) {
	p := setupPayments(t)
	rec, out := call(p.s, "GET", "/api/payments/config", nil)
	if rec.Code != http.StatusOK || out["provider"] != "midtrans" || out["clientKey"] != "SB-Mid-client-test" || !strings.HasSuffix(out["snapUrl"].(string), "/snap/snap.js") {
		t.Fatalf("config: %d %v", rec.Code, out)
	}
	if _, has := out["serverKey"]; has {
		t.Fatal("server key bocor di config")
	}
	id, _ := p.newOrder(t, "")
	// pembeli tidak bisa melunasi sendiri
	if rec, _ := call(p.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, p.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli melunasi sendiri: %d, want 403", rec.Code)
	}
	if st, _ := p.orderStatus(t, id); st != "menunggu-pembayaran" {
		t.Errorf("status berubah: %s", st)
	}
	// admin boleh menandai lunas manual
	if rec, _ := call(p.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay"}, p.admin); rec.Code != http.StatusOK {
		t.Errorf("admin lunas manual: %d", rec.Code)
	}
	if st, ps := p.orderStatus(t, id); st != "dibayar" || ps != "paid" {
		t.Errorf("setelah lunas manual: %s/%s", st, ps)
	}
	var method string
	p.pool.QueryRow(context.Background(), `SELECT method FROM payments WHERE order_id=$1`, id).Scan(&method)
	if method != "Manual (admin)" {
		t.Errorf("metode %q, want Manual (admin)", method)
	}
}

func TestPaymentSessionCreationAndReuse(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "")

	if code, _ := p.session(t, id, nil); code != http.StatusUnauthorized {
		t.Errorf("tanpa login: %d, want 401", code)
	}
	if code, _ := p.session(t, id, p.other); code != http.StatusNotFound {
		t.Errorf("akun lain: %d, want 404", code)
	}
	code, out := p.session(t, id, p.buyer)
	if code != http.StatusOK || out["token"] != "tok-"+id+"-1" {
		t.Fatalf("buat sesi: %d %v", code, out)
	}
	reqs := p.fm.snapRequests()
	if len(reqs) != 1 {
		t.Fatalf("permintaan Snap: %d, want 1", len(reqs))
	}
	body := reqs[0]
	td := body["transaction_details"].(map[string]any)
	if td["order_id"] != id+"-1" || num(td["gross_amount"]) != total {
		t.Errorf("transaction_details: %v (total pesanan %d)", td, total)
	}
	// item_details harus berjumlah sama dengan gross_amount (syarat Midtrans), termasuk ongkir
	sum := 0
	for _, it := range body["item_details"].([]any) {
		m := it.(map[string]any)
		sum += num(m["price"]) * num(m["quantity"])
	}
	if sum != total {
		t.Errorf("jumlah item_details %d != total %d", sum, total)
	}
	if exp := body["expiry"].(map[string]any); num(exp["duration"]) < 1400 || exp["unit"] != "minutes" {
		t.Errorf("masa berlaku sesi: %v", exp)
	}
	// buka lagi → sesi yang sama dipakai ulang, tidak membuat transaksi baru
	if code, out := p.session(t, id, p.buyer); code != http.StatusOK || out["token"] != "tok-"+id+"-1" || len(p.fm.snapRequests()) != 1 {
		t.Errorf("pakai ulang sesi: %d %v, permintaan Snap=%d", code, out, len(p.fm.snapRequests()))
	}
	if p.fm.badAuth != 0 {
		t.Errorf("%d permintaan ke Midtrans tanpa Server Key yang benar", p.fm.badAuth)
	}
}

func TestPaymentSessionWithVoucherSumsMatch(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "HEMAT25")
	if code, out := p.session(t, id, p.buyer); code != http.StatusOK {
		t.Fatalf("sesi: %d %v", code, out)
	}
	sum := 0
	hasDiscount := false
	for _, it := range p.fm.snapRequests()[0]["item_details"].([]any) {
		m := it.(map[string]any)
		sum += num(m["price"]) * num(m["quantity"])
		hasDiscount = hasDiscount || num(m["price"]) < 0
	}
	if sum != total || !hasDiscount {
		t.Errorf("dengan voucher: jumlah item %d vs total %d, baris diskon=%v", sum, total, hasDiscount)
	}
}

func TestWebhookSettlementIsVerifiedAndIdempotent(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "")
	p.session(t, id, p.buyer)
	gw := id + "-1"

	// 1) webhook palsu (tanda tangan salah)
	forged := map[string]string{"order_id": gw, "transaction_status": "settlement", "status_code": "200", "gross_amount": fmt.Sprintf("%d.00", total), "signature_key": "salah"}
	if code := p.fm.webhook(t, p.s, forged); code != http.StatusForbidden {
		t.Errorf("tanda tangan salah: %d, want 403", code)
	}
	// 2) tanda tangan benar tapi Midtrans sendiri belum menyatakan lunas → tidak boleh lunas
	claims := signed(map[string]string{"order_id": gw, "transaction_status": "settlement", "status_code": "200", "gross_amount": fmt.Sprintf("%d.00", total)})
	p.fm.set(gw, "pending", total, "qris")
	if code := p.fm.webhook(t, p.s, claims); code != http.StatusOK {
		t.Errorf("webhook: %d", code)
	}
	if st, ps := p.orderStatus(t, id); st != "menunggu-pembayaran" || ps != "pending" {
		t.Fatalf("webhook yang tidak sesuai Status API melunasi pesanan: %s/%s", st, ps)
	}
	// 3) Midtrans menyatakan settlement → lunas, royalti tercatat, metode terisi
	p.fm.set(gw, "settlement", total, "qris")
	if code := p.fm.webhook(t, p.s, claims); code != http.StatusOK {
		t.Fatalf("webhook settlement: %d", code)
	}
	if st, ps := p.orderStatus(t, id); st != "dibayar" || ps != "paid" {
		t.Fatalf("setelah settlement: %s/%s", st, ps)
	}
	var method, gateway, ptype string
	p.pool.QueryRow(context.Background(), `SELECT method, gateway, payment_type FROM payments WHERE order_id=$1`, id).Scan(&method, &gateway, &ptype)
	if method != "QRIS" || gateway != "midtrans" || ptype != "qris" {
		t.Errorf("metode/gateway: %s/%s/%s", method, gateway, ptype)
	}
	royalties := p.royaltyRows(t, id)
	if royalties != 2 { // satu baris royalti per baris pesanan: 2×M dan 1×L
		t.Fatalf("baris royalti %d, want 2", royalties)
	}
	// 4) webhook yang sama dikirim ulang (Midtrans mengulang) → tidak menggandakan apa pun
	for i := 0; i < 3; i++ {
		p.fm.webhook(t, p.s, claims)
	}
	if n := p.royaltyRows(t, id); n != royalties {
		t.Errorf("royalti berganda setelah webhook ulang: %d", n)
	}
	var sold int
	p.pool.QueryRow(context.Background(), `SELECT sold FROM listings WHERE id=$1`, p.listing).Scan(&sold)
	if sold != 3 {
		t.Errorf("counter terjual %d, want 3 (tidak bertambah saat webhook ulang)", sold)
	}
	// 5) semua notifikasi tercatat untuk audit
	var logged, rejected int
	p.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE NOT signature_ok) FROM payment_notifications WHERE gateway_order_id=$1`, gw).Scan(&logged, &rejected)
	if logged < 4 || rejected != 1 {
		t.Errorf("log notifikasi: total %d, ditolak %d", logged, rejected)
	}
}

func TestWebhookAmountMismatchIgnored(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "")
	p.session(t, id, p.buyer)
	gw := id + "-1"
	p.fm.set(gw, "settlement", total-1000, "qris") // Midtrans melaporkan nominal lebih kecil
	n := signed(map[string]string{"order_id": gw, "transaction_status": "settlement", "status_code": "200", "gross_amount": fmt.Sprintf("%d.00", total-1000)})
	p.fm.webhook(t, p.s, n)
	if st, _ := p.orderStatus(t, id); st != "menunggu-pembayaran" {
		t.Errorf("nominal tidak cocok tetapi pesanan %s", st)
	}
}

func TestSyncAppliesStatusFromMidtrans(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "")
	sync := func(cookie *http.Cookie) (int, map[string]any) {
		rec, out := call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, cookie)
		return rec.Code, out
	}
	// belum ada sesi / belum ada transaksi di Midtrans → tidak ada perubahan
	if code, out := sync(p.buyer); code != http.StatusOK || out["order"].(map[string]any)["status"] != "menunggu-pembayaran" {
		t.Fatalf("sync tanpa sesi: %d %v", code, out)
	}
	p.session(t, id, p.buyer)
	if code, out := sync(p.buyer); code != http.StatusOK || out["order"].(map[string]any)["status"] != "menunggu-pembayaran" {
		t.Fatalf("sync saat belum ada transaksi: %d %v", code, out)
	}
	if code, _ := sync(p.other); code != http.StatusNotFound {
		t.Errorf("sync akun lain: %d, want 404", code)
	}
	// VA diterbitkan → pending dengan metode terisi
	p.fm.mu.Lock()
	p.fm.status[id+"-1"] = map[string]string{"order_id": id + "-1", "transaction_status": "pending", "status_code": "201",
		"gross_amount": fmt.Sprintf("%d.00", total), "payment_type": "echannel"}
	p.fm.mu.Unlock()
	_, out := sync(p.buyer)
	if pay := out["order"].(map[string]any)["payment"].(map[string]any); pay["status"] != "pending" || pay["method"] != "VA Mandiri" {
		t.Errorf("setelah VA terbit: %v", pay)
	}
	// dibayar → sync melunasi (tanpa webhook, seperti dev tanpa tunnel)
	p.fm.set(id+"-1", "settlement", total, "gopay")
	code, out := sync(p.buyer)
	o := out["order"].(map[string]any)
	if code != http.StatusOK || o["status"] != "dibayar" || o["payment"].(map[string]any)["method"] != "GoPay" {
		t.Errorf("sync melunasi: %d %v", code, o["payment"])
	}
}

func TestFailedAttemptThenRetryUsesNewAttempt(t *testing.T) {
	p := setupPayments(t)
	id, total := p.newOrder(t, "")
	p.session(t, id, p.buyer)
	p.fm.set(id+"-1", "deny", total, "credit_card")
	call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, p.buyer)
	if st, ps := p.orderStatus(t, id); st != "menunggu-pembayaran" || ps != "failed" {
		t.Fatalf("setelah ditolak: %s/%s, want menunggu/failed", st, ps)
	}
	code, out := p.session(t, id, p.buyer)
	if code != http.StatusOK || out["token"] != "tok-"+id+"-2" {
		t.Fatalf("percobaan ke-2: %d %v", code, out)
	}
	if _, ps := p.orderStatus(t, id); ps != "pending" {
		t.Errorf("status pembayaran setelah sesi baru: %s, want pending", ps)
	}
	// notifikasi untuk percobaan lama (-1) tidak boleh memengaruhi percobaan baru
	old := signed(map[string]string{"order_id": id + "-1", "transaction_status": "settlement", "status_code": "200", "gross_amount": fmt.Sprintf("%d.00", total)})
	p.fm.set(id+"-1", "settlement", total, "qris")
	p.fm.webhook(t, p.s, old)
	if st, _ := p.orderStatus(t, id); st != "menunggu-pembayaran" {
		t.Errorf("notifikasi percobaan lama melunasi pesanan: %s", st)
	}
}

func TestExpiredPaymentCancelsOrderAndFreesVoucher(t *testing.T) {
	p := setupPayments(t)
	ctx := context.Background()
	code := "T" + strings.ToUpper(p.tag) + "ONE"
	p.pool.Exec(ctx, `INSERT INTO vouchers (code, kind, value, usage_limit, per_user_limit) VALUES ($1,'fixed',10000,1,1)`, code)

	id, total := p.newOrder(t, code)
	// kuota 1 habis oleh pesanan ini
	if _, out := call(p.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{p.line("M", 1)}, "courier": "JNE REG", "voucher": code}, p.other); quoteOf(t, out)["voucherError"] == nil {
		t.Fatal("voucher kuota 1 masih bisa dipakai akun lain padahal sudah terpakai")
	}
	p.session(t, id, p.buyer)
	p.fm.set(id+"-1", "expire", total, "bank_transfer")
	rec, out := call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, p.buyer)
	o := out["order"].(map[string]any)
	if rec.Code != http.StatusOK || o["status"] != "dibatalkan" || o["payment"].(map[string]any)["status"] != "expired" {
		t.Fatalf("setelah expire: %d status=%v payment=%v", rec.Code, o["status"], o["payment"])
	}
	// voucher bebas lagi
	if _, out := call(p.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{p.line("M", 1)}, "courier": "JNE REG", "voucher": code}, p.other); quoteOf(t, out)["voucherError"] != nil {
		t.Errorf("voucher tidak dibebaskan setelah pesanan dibatalkan: %v", quoteOf(t, out)["voucherError"])
	}
	// pesanan dibatalkan tidak muncul sebagai pesanan masuk kreator, dan tidak bisa dibayar lagi
	if _, out := call(p.s, "GET", "/api/designer/orders", nil, p.creator); len(out["items"].([]any)) != 0 {
		t.Errorf("pesanan dibatalkan muncul di kreator: %v", out["items"])
	}
	if code, _ := p.session(t, id, p.buyer); code != http.StatusBadRequest {
		t.Errorf("sesi bayar untuk pesanan dibatalkan: %d, want 400", code)
	}
	// pembayaran yang terlambat (uang sudah masuk setelah dibatalkan) tetap diterima
	p.fm.set(id+"-1", "settlement", total, "bank_transfer")
	call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, p.buyer)
	if st, ps := p.orderStatus(t, id); st != "dibayar" || ps != "paid" {
		t.Errorf("pembayaran terlambat: %s/%s, want dibayar/paid", st, ps)
	}
}

func TestCancelUnpaidOrdersJob(t *testing.T) {
	p := setupPayments(t)
	ctx := context.Background()
	old, _ := p.newOrder(t, "")
	fresh, _ := p.newOrder(t, "")
	paid, _ := p.newOrder(t, "")
	call(p.s, "PATCH", "/api/orders/"+paid, map[string]string{"action": "pay"}, p.admin)
	for _, id := range []string{old, paid} {
		p.pool.Exec(ctx, `UPDATE orders SET created_at = now() - interval '26 hours' WHERE id=$1`, id)
	}
	n, err := p.s.CancelUnpaidOrders(ctx)
	if err != nil || n < 1 {
		t.Fatalf("job: n=%d err=%v", n, err)
	}
	if st, ps := p.orderStatus(t, old); st != "dibatalkan" || ps != "expired" {
		t.Errorf("pesanan lewat batas: %s/%s", st, ps)
	}
	if st, _ := p.orderStatus(t, fresh); st != "menunggu-pembayaran" {
		t.Errorf("pesanan baru ikut dibatalkan: %s", st)
	}
	if st, _ := p.orderStatus(t, paid); st != "dibayar" {
		t.Errorf("pesanan lunas ikut dibatalkan: %s", st)
	}
	var label string
	p.pool.QueryRow(ctx, `SELECT label FROM order_events WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, old).Scan(&label)
	if !strings.Contains(label, "dibatalkan") {
		t.Errorf("event terakhir: %q", label)
	}
	// dijalankan ulang tidak membatalkan dua kali
	if n2, _ := p.s.CancelUnpaidOrders(ctx); n2 != 0 {
		t.Errorf("job kedua membatalkan %d pesanan", n2)
	}
	_ = time.Now
}

func TestProviderNoneDisablesBuyerPayment(t *testing.T) {
	f := setupCheckout(t)
	f.s.SetPayments(PaymentsConfig{Provider: "none"})
	_, out := f.order(t, f.buyer, "", f.line("M", 1))
	id := out["order"].(map[string]any)["id"].(string)
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay"}, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli bayar tanpa penyedia: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/orders/"+id+"/payment", nil, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("sesi tanpa penyedia: %d, want 400", rec.Code)
	}
	if rec, out := call(f.s, "GET", "/api/payments/config", nil); rec.Code != http.StatusOK || out["provider"] != "none" || out["clientKey"] != nil {
		t.Errorf("config none: %v", out)
	}
}
