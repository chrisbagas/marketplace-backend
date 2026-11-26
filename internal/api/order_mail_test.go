package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"karyakita/api/internal/mail"
)

// Tes email transaksi pesanan / kreator / admin (butuh TEST_DATABASE_URL,
// lihat google_test.go). Email ditangkap fakeMailer.

// withCapture memasang fakeMailer + ADMIN_NOTIFY_EMAIL pada server fixture.
func withCapture(f *checkoutFixture) (*fakeMailer, string) {
	fm := &fakeMailer{sent: make(chan mail.Message, 50)}
	f.s.auth.Mailer = fm
	admin := "ops-" + f.tag + "@mail.test"
	f.s.auth.AdminNotifyEmail = admin
	return fm, admin
}

// drain mengambil semua email yang masuk dalam jendela waktu singkat.
func (fm *fakeMailer) drain(wait time.Duration) []mail.Message {
	var out []mail.Message
	deadline := time.After(wait)
	for {
		select {
		case m := <-fm.sent:
			out = append(out, m)
		case <-deadline:
			return out
		}
	}
}

func (f *checkoutFixture) buyerEmail() string { return f.buyerID + "@mail.test" }

func (f *checkoutFixture) act(t *testing.T, cookie *http.Cookie, id string, body map[string]string) {
	t.Helper()
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, body, cookie); rec.Code != http.StatusOK {
		t.Fatalf("%v: %d %v", body, rec.Code, out)
	}
}

func mustContain(t *testing.T, m mail.Message, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(m.Text, p) {
			t.Errorf("email %q tidak memuat %q:\n%s", m.Subject, p, m.Text)
		}
	}
}

func TestOrderLifecycleEmails(t *testing.T) {
	f := setupCheckout(t)
	fm, adminTo := withCapture(f)

	// dibuat → 1 email ke pembeli (email akun, karena form checkout tidak mengisi email)
	rec, out := f.order(t, f.buyer, "", f.line("M", 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	total := num(out["order"].(map[string]any)["total"])
	m := fm.next(t)
	if m.To != f.buyerEmail() || !strings.Contains(m.Subject, id) || !strings.Contains(m.Subject, "menunggu pembayaran") {
		t.Fatalf("email dibuat: to=%s subject=%q", m.To, m.Subject)
	}
	mustContain(t, m, "Bayar sekarang", "http://app.test/order/"+id, rupiah(total), "2×", "sebelum")
	fm.none(t)

	// bayar → email lunas ke pembeli + 1 email ke admin (urutan goroutine tidak dijamin)
	f.act(t, f.buyer, id, map[string]string{"action": "pay", "method": "QRIS"})
	got := map[string]mail.Message{}
	for range 2 {
		m := fm.next(t)
		got[m.To] = m
	}
	paid, ok := got[f.buyerEmail()]
	if !ok || !strings.Contains(paid.Subject, "Pembayaran pesanan "+id+" diterima") {
		t.Fatalf("email lunas ke pembeli tidak ada: %v", got)
	}
	mustContain(t, paid, "QRIS", rupiah(total))
	adm, ok := got[adminTo]
	if !ok || !strings.Contains(adm.Subject, "Pesanan baru "+id) {
		t.Fatalf("email admin tidak ada: %v", got)
	}
	mustContain(t, adm, "http://app.test/admin/pesanan/"+id)

	// bayar dua kali (klik ganda / webhook ulang) → tidak ada email baru
	f.act(t, f.buyer, id, map[string]string{"action": "pay", "method": "QRIS"})
	f.act(t, f.admin, id, map[string]string{"action": "pay"})
	fm.none(t)

	// produksi → tidak ada email
	f.act(t, f.admin, id, map[string]string{"action": "advance"})
	fm.none(t)

	// dikirim → kurir + resi
	resi := "OB" + strings.ToUpper(f.tag) + "123"
	f.act(t, f.admin, id, map[string]string{"action": "ship", "courier": "JNE REG", "trackingNumber": resi})
	m = fm.next(t)
	if m.To != f.buyerEmail() || !strings.Contains(m.Subject, "sudah dikirim") {
		t.Fatalf("email dikirim: to=%s subject=%q", m.To, m.Subject)
	}
	mustContain(t, m, "JNE REG", resi)
	// koreksi resi bukan transisi → tidak ada email
	f.act(t, f.admin, id, map[string]string{"action": "update-shipment", "courier": "JNE REG", "trackingNumber": resi + "9"})
	fm.none(t)

	// tiba → minta konfirmasi + info selesai otomatis 48 jam
	f.act(t, f.admin, id, map[string]string{"action": "delivered"})
	m = fm.next(t)
	if m.To != f.buyerEmail() || !strings.Contains(m.Subject, "sudah tiba") {
		t.Fatalf("email tiba: to=%s subject=%q", m.To, m.Subject)
	}
	mustContain(t, m, "Konfirmasi pesanan diterima", "http://app.test/order/"+id, "48 jam")

	// konfirmasi pembeli: tidak ada email (pembeli sendiri yang menekan)
	f.act(t, f.buyer, id, map[string]string{"action": "confirm"})
	fm.none(t)
}

func TestOrderEmailPrefersCustomerEmailField(t *testing.T) {
	f := setupCheckout(t)
	fm, _ := withCapture(f)
	cust := map[string]any{}
	for k, v := range testCustomer {
		cust[k] = v
	}
	cust["email"] = "Penerima-" + f.tag + "@mail.test"
	rec, out := call(f.s, "POST", "/api/orders", map[string]any{
		"customer": cust, "items": []any{f.line("M", 1)}, "courier": "JNE REG",
	}, f.buyer)
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	if m := fm.next(t); m.To != "penerima-"+f.tag+"@mail.test" {
		t.Errorf("email dikirim ke %s, want email di pesanan", m.To)
	}
	fm.none(t)
}

func TestWebhookReplaySendsNoMoreEmails(t *testing.T) {
	p := setupPayments(t)
	fm, adminTo := withCapture(p.checkoutFixture)
	id, total := p.newOrder(t, "")
	if m := fm.next(t); !strings.Contains(m.Subject, "menunggu pembayaran") {
		t.Fatalf("email dibuat: %q", m.Subject)
	}
	p.session(t, id, p.buyer)
	gw := id + "-1"
	claims := signed(map[string]string{"order_id": gw, "transaction_status": "settlement", "status_code": "200", "gross_amount": fmt.Sprintf("%d.00", total)})

	// Midtrans belum menyatakan lunas → tidak ada email
	p.fm.set(gw, "pending", total, "qris")
	p.fm.webhook(t, p.s, claims)
	fm.none(t)

	// settlement → lunas: email ke pembeli + admin
	p.fm.set(gw, "settlement", total, "qris")
	if code := p.fm.webhook(t, p.s, claims); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	to := map[string]bool{}
	for range 2 {
		to[fm.next(t).To] = true
	}
	if !to[p.buyerEmail()] || !to[adminTo] {
		t.Fatalf("penerima email lunas: %v", to)
	}

	// webhook yang sama dikirim ulang + sync → tidak ada email baru
	for range 3 {
		p.fm.webhook(t, p.s, claims)
	}
	call(p.s, "POST", "/api/orders/"+id+"/payment/sync", nil, p.buyer)
	fm.none(t)
}

func TestAutoCompleteAndCancelEmails(t *testing.T) {
	f := setupCheckout(t)
	fm, _ := withCapture(f)
	ctx := context.Background()

	// tiba lewat batas → selesai otomatis, 1 email (job dijalankan dua kali)
	_, out := f.order(t, f.buyer, "", f.line("M", 1))
	done := out["order"].(map[string]any)["id"].(string)
	fm.next(t) // email dibuat
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status='tiba', shipped_courier='JNE REG', tracking_number=$2,
		shipped_at=now() - interval '5 days', delivered_at=now() - interval '3 days' WHERE id=$1`,
		done, "OBA"+strings.ToUpper(f.tag)+done[len(done)-4:]); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.s.AutoCompleteOrders(ctx, AutoCompleteAfter); err != nil {
			t.Fatal(err)
		}
	}
	var mine []mail.Message
	for _, m := range fm.drain(700 * time.Millisecond) {
		if m.To == f.buyerEmail() {
			mine = append(mine, m)
		}
	}
	if len(mine) != 1 || !strings.Contains(mine[0].Subject, "Pesanan "+done+" selesai") {
		t.Fatalf("email selesai otomatis: %d email %v", len(mine), mine)
	}
	mustContain(t, mine[0], "48 jam")

	// tak dibayar → dibatalkan, 1 email walau dipanggil ulang
	_, out = f.order(t, f.buyer, "", f.line("M", 1))
	stale := out["order"].(map[string]any)["id"].(string)
	fm.next(t) // email dibuat
	for range 2 {
		if err := f.s.cancelUnpaidOrder(ctx, stale, "batas waktu pembayaran habis"); err != nil {
			t.Fatal(err)
		}
	}
	m := fm.next(t)
	if m.To != f.buyerEmail() || !strings.Contains(m.Subject, "Pesanan "+stale+" dibatalkan") {
		t.Fatalf("email batal: to=%s subject=%q", m.To, m.Subject)
	}
	fm.none(t)

	// pesanan yang sudah lunas tidak bisa dibatalkan → tidak ada email
	paid := f.paidOrder(t, 1)
	fm.drain(500 * time.Millisecond) // dibuat + lunas
	if err := f.s.cancelUnpaidOrder(ctx, paid, "x"); err != nil {
		t.Fatal(err)
	}
	fm.none(t)
}

func TestDesignReviewEmails(t *testing.T) {
	f := setupCheckout(t)
	fm, _ := withCapture(f)
	ctx := context.Background()
	designer := "d-co-" + f.tag
	subs := []string{"ds-ob1-" + f.tag, "ds-ob2-" + f.tag}
	t.Cleanup(func() {
		f.pool.Exec(ctx, `UPDATE design_submissions SET published_listing_id = NULL WHERE id = ANY($1)`, subs)
		f.pool.Exec(ctx, `DELETE FROM listing_search WHERE listing_id IN (SELECT id FROM listings WHERE design_id IN (SELECT id FROM designs WHERE designer_id = $1 AND id <> $2))`, designer, "dz-co-"+f.tag)
		f.pool.Exec(ctx, `DELETE FROM listings WHERE design_id IN (SELECT id FROM designs WHERE designer_id = $1 AND id <> $2)`, designer, "dz-co-"+f.tag)
		f.pool.Exec(ctx, `DELETE FROM designs WHERE designer_id = $1 AND id <> $2`, designer, "dz-co-"+f.tag)
		f.pool.Exec(ctx, `DELETE FROM design_submissions WHERE id = ANY($1)`, subs)
	})
	for i, title := range []string{"Matahari Pagi", "Bulan Malam"} {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO design_submissions (id, designer_id, designer_name, title, product_type_id, color_id, price, uri, status)
			VALUES ($1, $2, 'Kreator', $3, 'kaos', 'putih', 150000, 'data:image/svg+xml,x', 'review')`,
			subs[i], designer, title); err != nil {
			t.Fatal(err)
		}
	}
	creatorTo := "co_creator_" + f.tag + "@mail.test"
	review := func(id, status, note string) {
		t.Helper()
		if rec, out := call(f.s, "PATCH", "/api/designs", map[string]any{"id": id, "status": status, "note": note}, f.admin); rec.Code != http.StatusOK {
			t.Fatalf("review %s: %d %v", status, rec.Code, out)
		}
	}

	review(subs[0], "disetujui", "")
	m := fm.next(t)
	if m.To != creatorTo || !strings.Contains(m.Subject, "Matahari Pagi") || !strings.Contains(m.Subject, "disetujui") {
		t.Fatalf("email disetujui: to=%s subject=%q", m.To, m.Subject)
	}
	var listing string
	f.pool.QueryRow(ctx, `SELECT published_listing_id FROM design_submissions WHERE id=$1`, subs[0]).Scan(&listing)
	mustContain(t, m, "http://app.test/product/"+listing)
	review(subs[0], "disetujui", "") // diulang → tidak ada email baru
	fm.none(t)

	review(subs[1], "ditolak", "Resolusi gambar terlalu rendah")
	m = fm.next(t)
	if m.To != creatorTo || !strings.Contains(m.Subject, "Bulan Malam") || !strings.Contains(m.Subject, "belum bisa disetujui") {
		t.Fatalf("email ditolak: to=%s subject=%q", m.To, m.Subject)
	}
	mustContain(t, m, "Resolusi gambar terlalu rendah")
	review(subs[1], "ditolak", "Resolusi gambar terlalu rendah")
	fm.none(t)
}

func TestPayoutPaidEmail(t *testing.T) {
	f := setupCheckout(t)
	fm, _ := withCapture(f)
	ctx := context.Background()
	var id int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO payouts (designer_id, amount, status) VALUES ($1, 75000, 'diminta') RETURNING id`,
		"d-co-"+f.tag).Scan(&id); err != nil {
		t.Fatal(err)
	}
	patch := func(body map[string]any) int {
		rec, _ := call(f.s, "PATCH", fmt.Sprintf("/api/payouts/%d", id), body, f.admin)
		return rec.Code
	}
	if code := patch(map[string]any{"action": "process"}); code != http.StatusOK {
		t.Fatalf("process: %d", code)
	}
	fm.none(t)
	if code := patch(map[string]any{"action": "paid", "reference": "TRF-" + f.tag}); code != http.StatusOK {
		t.Fatalf("paid: %d", code)
	}
	m := fm.next(t)
	if m.To != "co_creator_"+f.tag+"@mail.test" || !strings.Contains(m.Subject, "Rp75.000") {
		t.Fatalf("email penarikan: to=%s subject=%q", m.To, m.Subject)
	}
	mustContain(t, m, "TRF-"+f.tag, "Rp75.000")
	if code := patch(map[string]any{"action": "paid", "reference": "TRF-LAIN"}); code != http.StatusConflict {
		t.Errorf("paid dua kali: %d, want 409", code)
	}
	fm.none(t)
}

func TestAdminNotifySkippedWhenUnset(t *testing.T) {
	f := setupCheckout(t)
	fm, _ := withCapture(f)
	f.s.auth.AdminNotifyEmail = ""
	id := f.paidOrder(t, 1)
	for _, m := range fm.drain(500 * time.Millisecond) {
		if m.To != f.buyerEmail() {
			t.Errorf("email tak terduga ke %s: %q (pesanan %s)", m.To, m.Subject, id)
		}
	}
}

// TestRenderOrderTemplates mencetak setiap template sekali (go test -v -run Render)
// supaya teksnya bisa dibaca mata; juga memastikan HTML tidak berisi sisa placeholder.
func TestRenderOrderTemplates(t *testing.T) {
	o := mail.OrderInfo{
		ID: "KK-261126-0042", URL: "http://app.test/order/KK-261126-0042", Buyer: "Budi Santoso",
		Items: []mail.OrderLine{
			{Title: "Kaos Matahari Pagi", Variant: "Putih, M", Qty: 2, Price: 129000},
			{Title: "Mug Bulan Malam", Variant: "Hitam", Qty: 1, Price: 89000},
		},
		Subtotal: 347000, Shipping: 20000, Discount: 10000, Total: 357000,
		Courier: "JNE REG", Tracking: "JNE0123456789", Method: "QRIS",
		PayBefore:     time.Date(2026, 11, 27, 7, 30, 0, 0, time.UTC),
		ConfirmWithin: "48 jam",
	}
	msgs := []mail.Message{
		mail.OrderCreated("b@mail.test", "Budi", o),
		mail.OrderPaid("b@mail.test", "Budi", o),
		mail.OrderShipped("b@mail.test", "Budi", o),
		mail.OrderArrived("b@mail.test", "Budi", o),
		mail.OrderCompleted("b@mail.test", "Budi", o),
		mail.OrderCancelled("b@mail.test", "Budi", o),
		mail.DesignApproved("k@mail.test", "Raka", "Matahari Pagi", "http://app.test/product/kaos-matahari-0001"),
		mail.DesignRejected("k@mail.test", "Raka", "Bulan Malam", "Resolusi terlalu rendah"),
		mail.PayoutPaid("k@mail.test", "Raka", 250000, "TRF-998877"),
		mail.AdminNewPaidOrder("ops@mail.test", o),
	}
	for _, m := range msgs {
		t.Logf("\n=== %s\n--- to: %s\n%s", m.Subject, m.To, m.Text)
		if strings.Contains(m.HTML, "{{") || strings.Contains(m.HTML, "ZgotmplZ") || m.HTML == "" {
			t.Errorf("HTML %q bermasalah", m.Subject)
		}
	}
}
