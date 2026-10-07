package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tes integrasi checkout, voucher, akses pesanan, dan pesanan masuk kreator
// (butuh TEST_DATABASE_URL, lihat google_test.go).

type checkoutFixture struct {
	s                     *Server
	pool                  *pgxpool.Pool
	buyer, other, admin   *http.Cookie
	creator, otherCreator *http.Cookie
	buyerID               string
	listing, listing2     string // milik creator; listing2 nonaktif
	price                 int
	tag                   string
}

// newSessionUser membuat akun + sesi langsung di database (tanpa password/email).
func newSessionUser(t *testing.T, pool *pgxpool.Pool, id, role, name string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, email, name, role, email_verified) VALUES ($1,$1,$1 || '@mail.test',$2,$3,true)`,
		id, name, role); err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id, expires_at) VALUES ($1,$2, now() + interval '1 hour')`,
		hashToken(token), id); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: token}
}

func setupCheckout(t *testing.T) *checkoutFixture {
	s, _, pool := emailServer(t)
	ctx := context.Background()
	tag := randomHex(3)
	f := &checkoutFixture{s: s, pool: pool, tag: tag, price: 123_000}
	f.buyerID = "co_buyer_" + tag
	f.buyer = newSessionUser(t, pool, f.buyerID, "customer", "Budi Santoso Wijaya")
	f.other = newSessionUser(t, pool, "co_other_"+tag, "customer", "Orang Lain")
	f.admin = newSessionUser(t, pool, "co_admin_"+tag, "admin", "Admin Tes")
	f.creator = newSessionUser(t, pool, "co_creator_"+tag, "designer", "Kreator Tes")
	f.otherCreator = newSessionUser(t, pool, "co_creator2_"+tag, "designer", "Kreator Lain")
	for _, d := range [][2]string{{"d-co-" + tag, "co_creator_" + tag}, {"d-co2-" + tag, "co_creator2_" + tag}} {
		if _, err := pool.Exec(ctx, `INSERT INTO designers (id, user_id, name, city) VALUES ($1,$2,'Kreator','Bandung')`, d[0], d[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO designs (id, designer_id, title, uri, tags) VALUES ($1,$2,'Desain Tes','data:image/svg+xml,x','{}')`,
		"dz-co-"+tag, "d-co-"+tag); err != nil {
		t.Fatal(err)
	}
	f.listing, f.listing2 = "kaos-co-"+tag, "kaos-co-off-"+tag
	if _, err := pool.Exec(ctx, `
		INSERT INTO listings (id, design_id, product_type_id, price, active) VALUES
		  ($1, $3, 'kaos', $4, true), ($2, $3, 'kaos', $4, false)`, f.listing, f.listing2, "dz-co-"+tag, f.price); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		users := []string{f.buyerID, "co_other_" + tag, "co_admin_" + tag, "co_creator_" + tag, "co_creator2_" + tag}
		pool.Exec(ctx, `DELETE FROM royalties WHERE order_item_id IN (SELECT oi.id FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.user_id = ANY($1))`, users)
		pool.Exec(ctx, `DELETE FROM reviews WHERE order_id IN (SELECT id FROM orders WHERE user_id = ANY($1))`, users)
		pool.Exec(ctx, `DELETE FROM order_events WHERE order_id IN (SELECT id FROM orders WHERE user_id = ANY($1))`, users)
		pool.Exec(ctx, `DELETE FROM orders WHERE user_id = ANY($1)`, users)
		pool.Exec(ctx, `DELETE FROM listings WHERE design_id = $1`, "dz-co-"+tag)
		pool.Exec(ctx, `DELETE FROM designs WHERE id = $1`, "dz-co-"+tag)
		pool.Exec(ctx, `DELETE FROM designers WHERE id = ANY($1)`, []string{"d-co-" + tag, "d-co2-" + tag})
		pool.Exec(ctx, `DELETE FROM vouchers WHERE code LIKE $1`, "T"+strings.ToUpper(tag)+"%")
		pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, users)
	})
	return f
}

func (f *checkoutFixture) line(size string, qty int) map[string]any {
	return map[string]any{"productId": f.listing, "title": "x", "type": "kaos", "color": "putih", "size": size, "qty": qty, "price": 1}
}

var testCustomer = map[string]any{"name": "Budi Santoso", "phone": "0812-3456-7890", "address": "Jl. Merdeka No. 10, RT 01/RW 02", "city": "Bandung", "postal": "40111"}

func (f *checkoutFixture) order(t *testing.T, cookie *http.Cookie, voucher string, items ...map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, out := call(f.s, "POST", "/api/orders", map[string]any{
		"customer": testCustomer, "items": items, "courier": "JNE REG", "voucher": voucher, "notes": "Titip satpam",
	}, cookie)
	return rec, out
}

func quoteOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	q, ok := out["quote"].(map[string]any)
	if !ok {
		t.Fatalf("tidak ada quote: %v", out)
	}
	return q
}

func num(v any) int { f, _ := v.(float64); return int(f) }

func TestQuoteUsesServerPricesAndKeepsSizesSeparate(t *testing.T) {
	f := setupCheckout(t)
	rec, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{
		"items":   []any{f.line("M", 2), f.line("L", 1), f.line("M", 1)}, // M dua kali → digabung
		"courier": "JNE REG",
	}, f.buyer)
	if rec.Code != http.StatusOK {
		t.Fatalf("quote: %d %v", rec.Code, out)
	}
	q := quoteOf(t, out)
	items := q["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("baris: %d, want 2 (M digabung, L terpisah)", len(items))
	}
	m, l := items[0].(map[string]any), items[1].(map[string]any)
	if m["size"] != "M" || num(m["qty"]) != 3 || l["size"] != "L" || num(l["qty"]) != 1 {
		t.Fatalf("baris tidak sesuai: %v / %v", m, l)
	}
	if num(m["price"]) != f.price {
		t.Fatalf("harga satuan %d, want %d dari database (bukan dari browser)", num(m["price"]), f.price)
	}
	if num(q["subtotal"]) != 4*f.price || num(q["shipping"]) != 20_000 || num(q["total"]) != 4*f.price+20_000 {
		t.Fatalf("ringkasan salah: %v", q)
	}
}

func TestQuoteRejectsInvalidLines(t *testing.T) {
	f := setupCheckout(t)
	bad := map[string]map[string]any{
		"ukuran":   f.line("XXXL", 1),
		"warna":    {"productId": f.listing, "type": "kaos", "color": "ungu", "size": "M", "qty": 1},
		"qty 0":    f.line("M", 0),
		"qty 100":  f.line("M", 100),
		"nonaktif": {"productId": f.listing2, "type": "kaos", "color": "putih", "size": "M", "qty": 1},
		"custom":   {"productId": "custom-1", "type": "kaos", "color": "putih", "size": "M", "qty": 1, "designUri": "https://evil.example/x.png"},
	}
	for name, line := range bad {
		if rec, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{line}}, f.buyer); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, rec.Code, out)
		}
	}
	// custom yang sah: harga = harga dasar kaos + biaya custom
	_, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{
		map[string]any{"productId": "custom-9", "title": "Custom Kaos — Logo", "type": "kaos", "color": "putih", "size": "M", "qty": 1, "designUri": "/uploads/a.png", "price": 1},
	}}, f.buyer)
	if it := quoteOf(t, out)["items"].([]any)[0].(map[string]any); num(it["price"]) != 95_000+customFee {
		t.Errorf("harga custom %v, want %d", it["price"], 95_000+customFee)
	}
	if rec, _ := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{f.line("M", 1)}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("quote tanpa login: %d, want 401", rec.Code)
	}
}

func TestVoucherRules(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	code := func(s string) string { return "T" + strings.ToUpper(f.tag) + s }
	if _, err := f.pool.Exec(ctx, `INSERT INTO vouchers (code, kind, value, min_subtotal, max_discount, usage_limit, per_user_limit, active, ends_at) VALUES
		($1,'percent',50,0,30000,NULL,5,true,NULL),
		($2,'fixed',40000,500000,NULL,NULL,5,true,NULL),
		($3,'shipping',50000,0,NULL,NULL,5,true,NULL),
		($4,'fixed',10000,0,NULL,NULL,1,true,NULL),
		($5,'fixed',10000,0,NULL,1,5,true,NULL),
		($6,'fixed',10000,0,NULL,NULL,5,false,NULL),
		($7,'fixed',10000,0,NULL,NULL,5,true,now() - interval '1 day')`,
		code("PCT"), code("MIN"), code("SHIP"), code("ONCE"), code("QUOTA"), code("OFF"), code("EXP")); err != nil {
		t.Fatal(err)
	}

	quote := func(cookie *http.Cookie, v string) map[string]any {
		_, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []any{f.line("M", 2)}, "courier": "JNE REG", "voucher": v}, cookie)
		return quoteOf(t, out)
	}
	// 50% dari 246.000 = 123.000 → dibatasi maks 30.000; kode tidak peka huruf besar/kecil
	if q := quote(f.buyer, " "+code("pct")+" "); num(q["discount"]) != 30_000 || num(q["total"]) != 2*f.price+20_000-30_000 {
		t.Errorf("persen dengan batas: %v", q)
	}
	if q := quote(f.buyer, code("MIN")); q["voucherError"] == nil || num(q["discount"]) != 0 {
		t.Errorf("minimal belanja tidak terpenuhi seharusnya ditolak: %v", q)
	}
	if q := quote(f.buyer, code("SHIP")); num(q["discount"]) != 20_000 { // subsidi maks = ongkir JNE
		t.Errorf("gratis ongkir: %v", q)
	}
	for _, c := range []string{code("OFF"), code("EXP"), "TIDAKADA"} {
		if q := quote(f.buyer, c); q["voucherError"] == nil {
			t.Errorf("voucher %s seharusnya ditolak: %v", c, q)
		}
	}
	// voucher tak berlaku saat membuat pesanan → 400, bukan ditagih harga penuh diam-diam
	if rec, _ := f.order(t, f.buyer, code("MIN"), f.line("M", 1)); rec.Code != http.StatusBadRequest {
		t.Errorf("pesanan dengan voucher tak berlaku: %d, want 400", rec.Code)
	}
	// batas per akun: sekali pakai
	if rec, out := f.order(t, f.buyer, code("ONCE"), f.line("M", 1)); rec.Code != http.StatusOK || num(out["order"].(map[string]any)["discount"]) != 10_000 {
		t.Fatalf("pakai voucher sekali: %d %v", rec.Code, out)
	}
	if q := quote(f.buyer, code("ONCE")); q["voucherError"] == nil {
		t.Error("voucher sekali pakai bisa dipakai dua kali oleh akun yang sama")
	}
	if q := quote(f.other, code("ONCE")); q["voucherError"] != nil {
		t.Errorf("akun lain seharusnya masih boleh: %v", q)
	}
	// kuota total
	f.order(t, f.other, code("QUOTA"), f.line("M", 1))
	if q := quote(f.buyer, code("QUOTA")); q["voucherError"] == nil {
		t.Error("kuota voucher habis tapi masih diterima")
	}
}

func TestOrderOwnershipAndAdminDesignerViews(t *testing.T) {
	f := setupCheckout(t)

	if rec, _ := call(f.s, "POST", "/api/orders", map[string]any{"customer": testCustomer, "items": []any{f.line("M", 1)}, "courier": "JNE REG"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("checkout tanpa login: %d, want 401", rec.Code)
	}
	badPhone := map[string]any{"name": "B", "phone": "12", "address": "Jl. Merdeka No. 10", "city": "Bandung"}
	if rec, _ := call(f.s, "POST", "/api/orders", map[string]any{"customer": badPhone, "items": []any{f.line("M", 1)}, "courier": "JNE REG"}, f.buyer); rec.Code != http.StatusBadRequest {
		t.Fatalf("nomor HP tidak valid: %d, want 400", rec.Code)
	}

	rec, out := call(f.s, "POST", "/api/orders", map[string]any{
		"customer": testCustomer, "items": []any{f.line("M", 2), f.line("XL", 1)}, "courier": "JNE REG",
		"notes": "Titip satpam", "saveProfile": true,
	}, f.buyer)
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	order := out["order"].(map[string]any)
	id := order["id"].(string)
	if order["userId"] != f.buyerID || num(order["total"]) != 3*f.price+20_000 || len(order["items"].([]any)) != 2 {
		t.Fatalf("pesanan tidak sesuai: %v", order)
	}
	cust := order["customer"].(map[string]any)
	if cust["phone"] != "081234567890" || cust["postal"] != "40111" || order["notes"] != "Titip satpam" {
		t.Fatalf("detail pengiriman tidak tersimpan rapi: %v / %v", cust, order["notes"])
	}
	var phone string
	f.pool.QueryRow(context.Background(), `SELECT phone FROM users WHERE id=$1`, f.buyerID).Scan(&phone)
	if phone != "081234567890" {
		t.Errorf("saveProfile tidak memperbarui profil: %q", phone)
	}

	// hanya pemilik & admin yang bisa melihat / membayar
	if rec, _ := call(f.s, "GET", "/api/orders/"+id, nil, f.other); rec.Code != http.StatusNotFound {
		t.Errorf("akun lain melihat pesanan: %d, want 404", rec.Code)
	}
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay"}, f.other); rec.Code != http.StatusNotFound {
		t.Errorf("akun lain membayar pesanan: %d, want 404", rec.Code)
	}
	if rec, _ := call(f.s, "GET", "/api/orders/"+id, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tanpa login melihat pesanan: %d, want 401", rec.Code)
	}
	if _, out := call(f.s, "GET", "/api/orders/mine", nil, f.buyer); len(out["orders"].([]any)) != 1 {
		t.Errorf("pesanan saya: %v", out)
	}
	if _, out := call(f.s, "GET", "/api/orders/mine", nil, f.other); len(out["orders"].([]any)) != 0 {
		t.Errorf("pesanan akun lain bocor: %v", out)
	}

	// kreator melihat item miliknya (royalti belum tercatat), kreator lain tidak
	_, out = call(f.s, "GET", "/api/designer/orders", nil, f.creator)
	items := out["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("pesanan masuk kreator: %d item, want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["buyer"] != "Budi S." || first["royaltyBooked"] != false || first["city"] != "Bandung" {
		t.Errorf("item kreator: %v", first)
	}
	if _, ok := first["address"]; ok {
		t.Error("alamat pembeli bocor ke kreator")
	}
	if _, out := call(f.s, "GET", "/api/designer/orders", nil, f.otherCreator); len(out["items"].([]any)) != 0 {
		t.Errorf("kreator lain melihat pesanan: %v", out)
	}
	if rec, _ := call(f.s, "GET", "/api/designer/orders", nil, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pelanggan membuka pesanan kreator: %d, want 403", rec.Code)
	}

	// pemilik membayar → royalti tercatat; memajukan status hanya admin
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, f.buyer); rec.Code != http.StatusOK {
		t.Fatalf("bayar: %d", rec.Code)
	}
	_, out = call(f.s, "GET", "/api/designer/orders", nil, f.creator)
	summary := out["summary"].(map[string]any)
	want := int(float64(3*f.price) * 0.12)
	if num(summary["royaltyBooked"]) < want-2 || num(summary["royaltyBooked"]) > want+2 || num(summary["paidUnits"]) != 3 {
		t.Errorf("royalti setelah bayar: %v, want ±%d", summary, want)
	}
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pembeli memajukan status: %d, want 403", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.admin); rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "produksi" {
		t.Errorf("admin memajukan status: %d %v", rec.Code, out)
	}

	// admin: daftar + pencarian (nomor pesanan / username) + filter status
	_, out = call(f.s, "GET", "/api/orders?q="+f.buyerID, nil, f.admin)
	if num(out["total"]) != 1 || out["orders"].([]any)[0].(map[string]any)["username"] != f.buyerID {
		t.Errorf("admin cari pesanan: %v", out)
	}
	if _, out := call(f.s, "GET", "/api/orders?status=produksi&q="+id, nil, f.admin); num(out["total"]) != 1 {
		t.Errorf("admin filter status: %v", out)
	}
	if rec, _ := call(f.s, "GET", "/api/orders", nil, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pelanggan membuka daftar semua pesanan: %d, want 403", rec.Code)
	}

	// ulasan hanya oleh pemilik pesanan
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE orders SET status='selesai', shipped_courier='JNE REG', tracking_number=$2, shipped_at=now() WHERE id=$1`,
		id, "TES"+strings.ToUpper(f.tag)+"001"); err != nil {
		t.Fatal(err)
	}
	review := map[string]any{"orderId": id, "listingId": f.listing, "rating": 5}
	if rec, _ := call(f.s, "POST", "/api/reviews", review, f.other); rec.Code != http.StatusNotFound {
		t.Errorf("akun lain mengulas pesanan orang: %d, want 404", rec.Code)
	}
	if rec, out := call(f.s, "POST", "/api/reviews", review, f.buyer); rec.Code != http.StatusOK {
		t.Errorf("pemilik mengulas: %d %v", rec.Code, out)
	}
}

func TestAdminVoucherCRUD(t *testing.T) {
	f := setupCheckout(t)
	code := "T" + strings.ToUpper(f.tag) + "NEW"
	body := map[string]any{"code": code, "description": "Tes", "kind": "percent", "value": 15, "minSubtotal": 50000, "maxDiscount": 20000}
	if rec, _ := call(f.s, "POST", "/api/vouchers", body, f.buyer); rec.Code != http.StatusForbidden {
		t.Errorf("pelanggan membuat voucher: %d, want 403", rec.Code)
	}
	if rec, out := call(f.s, "POST", "/api/vouchers", body, f.admin); rec.Code != http.StatusCreated {
		t.Fatalf("buat voucher: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "POST", "/api/vouchers", body, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("kode kembar: %d, want 409", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/vouchers", map[string]any{"code": code + "X", "kind": "percent", "value": 150}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("persen > 100: %d, want 400", rec.Code)
	}
	if rec, out := call(f.s, "PATCH", "/api/vouchers/"+code, map[string]any{"active": false}, f.admin); rec.Code != http.StatusOK || out["voucher"].(map[string]any)["active"] != false {
		t.Errorf("nonaktifkan voucher: %d %v", rec.Code, out)
	}
}

func TestShippingRequiresCourierTracking(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	newPaidOrder := func() string {
		_, out := f.order(t, f.buyer, "", f.line("M", 1))
		id := out["order"].(map[string]any)["id"].(string)
		call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, f.buyer)
		if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.admin); rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "produksi" {
			t.Fatalf("ke produksi: %d %v", rec.Code, out)
		}
		return id
	}
	id := newPaidOrder()
	act := func(cookie *http.Cookie, body map[string]string) (int, map[string]any) {
		rec, out := call(f.s, "PATCH", "/api/orders/"+id, body, cookie)
		return rec.Code, out
	}

	// tidak bisa lompat ke "dikirim" tanpa data kurir
	if code, out := act(f.admin, map[string]string{"action": "advance"}); code != http.StatusBadRequest {
		t.Fatalf("advance dari produksi tanpa resi: %d %v, want 400", code, out)
	}
	for name, body := range map[string]map[string]string{
		"tanpa resi":  {"action": "ship", "courier": "JNE REG"},
		"tanpa kurir": {"action": "ship", "trackingNumber": "JNE0123456789"},
		"resi pendek": {"action": "ship", "courier": "JNE REG", "trackingNumber": "AB1"},
		"resi aneh":   {"action": "ship", "courier": "JNE REG", "trackingNumber": "<script>123"},
	} {
		if code, _ := act(f.admin, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	ship := map[string]string{"action": "ship", "courier": "JNE REG", "trackingNumber": " jne 0123 4567 89" + strings.ToUpper(f.tag)}
	if code, _ := act(f.buyer, ship); code != http.StatusForbidden {
		t.Errorf("pembeli mengisi resi: %d, want 403", code)
	}
	code, out := act(f.admin, ship)
	if code != http.StatusOK {
		t.Fatalf("kirim dengan resi: %d %v", code, out)
	}
	o := out["order"].(map[string]any)
	sh, _ := o["shipment"].(map[string]any)
	wantResi := "JNE0123456789" + strings.ToUpper(f.tag)
	if o["status"] != "dikirim" || sh == nil || sh["trackingNumber"] != wantResi || sh["courier"] != "JNE REG" || num(sh["shippedAt"]) == 0 {
		t.Fatalf("data pengiriman: status=%v shipment=%v", o["status"], sh)
	}
	// pembeli melihat resi di pesanannya
	if _, out := call(f.s, "GET", "/api/orders/"+id, nil, f.buyer); out["order"].(map[string]any)["shipment"] == nil {
		t.Error("pembeli tidak melihat resi")
	}
	// resi yang sama untuk pesanan lain (kurir sama) ditolak
	id2 := newPaidOrder()
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id2, ship, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("resi kembar: %d, want 409", rec.Code)
	}
	// koreksi resi hanya saat dikirim
	fix := map[string]string{"action": "update-shipment", "courier": "SiCepat REG", "trackingNumber": "SCP" + strings.ToUpper(f.tag) + "777"}
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id2, fix, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("koreksi resi saat masih produksi: %d, want 400", rec.Code)
	}
	if code, out := act(f.admin, fix); code != http.StatusOK || out["order"].(map[string]any)["shipment"].(map[string]any)["courier"] != "SiCepat REG" {
		t.Errorf("koreksi resi: %d %v", code, out)
	}
	if code, out := act(f.admin, map[string]string{"action": "advance"}); code != http.StatusOK || out["order"].(map[string]any)["status"] != "selesai" {
		t.Errorf("dikirim → selesai: %d %v", code, out)
	}
	if code, _ := act(f.admin, map[string]string{"action": "advance"}); code != http.StatusBadRequest {
		t.Errorf("advance dari selesai: %d, want 400", code)
	}
	// database ikut menjaga: status dikirim tanpa resi ditolak CHECK constraint
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status='dikirim' WHERE id=$1`, id2); err == nil {
		t.Error("database menerima status dikirim tanpa resi")
	}
}
