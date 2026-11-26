package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"karyakita/api/internal/biteship"
)

// Tes pengiriman Biteship dengan server Biteship palsu (butuh TEST_DATABASE_URL).

const (
	testBiteshipKey = "biteship_test.key"
	testHookSecret  = "rahasia-webhook"
	testDestArea    = "IDNP33IDNC325IDND3713IDZ30161" // contoh: Sako Baru, Palembang
	testOriginArea  = "IDNP6IDNC148IDND836IDZ12410"
)

type fakeBiteship struct {
	mu        sync.Mutex
	srv       *httptest.Server
	badAuth   int
	rateReqs  []map[string]any
	orders    map[string]map[string]any // reference_id → request body
	tracking  map[string]string         // tracking id → status
	waybills  map[string]string         // reference_id → waybill
	noWaybill bool
}

func newFakeBiteship(t *testing.T) *fakeBiteship {
	f := &fakeBiteship{orders: map[string]map[string]any{}, tracking: map[string]string{}, waybills: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != testBiteshipKey {
			f.badAuth++
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"success":false,"error":"Authorization failed","code":40101001}`)
			return
		}
		var body map[string]any
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&body)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/maps/areas":
			if r.URL.Query().Get("countries") != "ID" || r.URL.Query().Get("input") == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"success":false,"error":"bad query"}`)
				return
			}
			fmt.Fprintf(w, `{"success":true,"areas":[{"id":%q,"name":"Sako Baru, Sako, Palembang. 30161","administrative_division_level_1_name":"Sumatera Selatan","administrative_division_level_2_name":"Palembang","administrative_division_level_3_name":"Sako","postal_code":30161}]}`, testDestArea)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/rates/couriers":
			f.rateReqs = append(f.rateReqs, body)
			if body["destination_area_id"] != testDestArea {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"success":false,"error":"area not found","code":40001001}`)
				return
			}
			weight := 0
			for _, it := range body["items"].([]any) {
				m := it.(map[string]any)
				weight += int(m["weight"].(float64)) * int(m["quantity"].(float64))
			}
			kg := (weight + 999) / 1000
			// the fake bills 12.000 per kg for JNE REG and 15.000 per kg for SiCepat BEST
			fmt.Fprintf(w, `{"success":true,"pricing":[
				{"courier_code":"sicepat","courier_name":"SiCepat","courier_service_code":"best","courier_service_name":"Besok Sampai Tujuan","shipment_duration_range":"1","shipment_duration_unit":"days","price":%d},
				{"courier_code":"jne","courier_name":"JNE","courier_service_code":"reg","courier_service_name":"Reguler","shipment_duration_range":"2 - 3","shipment_duration_unit":"days","price":%d}]}`,
				15000*kg, 12000*kg)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
			ref, _ := body["reference_id"].(string)
			if old, dup := f.orders[ref]; dup {
				_ = old
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"success":false,"error":"Reference id has already been used","code":40002060,"details":{"order_id":"bo-%s","waybill_id":%q,"reference_id":%q}}`, ref, f.waybills[ref], ref)
				return
			}
			f.orders[ref] = body
			f.waybills[ref] = "WYB" + strings.ReplaceAll(ref, "-", "")
			f.tracking["trk-"+ref] = "confirmed"
			wb := f.waybills[ref]
			if f.noWaybill {
				wb = ""
			}
			fmt.Fprintf(w, `{"success":true,"id":"bo-%s","price":%d,"status":"confirmed","courier":{"tracking_id":"trk-%s","waybill_id":%q,"company":%q,"type":%q}}`,
				ref, 41000, ref, wb, body["courier_company"], body["courier_type"])
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/orders/bo-"):
			ref := strings.TrimPrefix(r.URL.Path, "/v1/orders/bo-")
			if _, ok := f.orders[ref]; !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"success":true,"id":"bo-%s","price":41000,"status":"confirmed","courier":{"tracking_id":"trk-%s","waybill_id":%q,"company":"jne","type":"reg"}}`,
				ref, ref, f.waybills[ref])
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/trackings/trk-"):
			ref := strings.TrimPrefix(r.URL.Path, "/v1/trackings/trk-")
			st, ok := f.tracking["trk-"+ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"success":true,"id":"trk-%s","waybill_id":%q,"order_id":"bo-%s","status":%q,"history":[]}`, ref, f.waybills[ref], ref, st)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBiteship) client() *biteship.Client {
	c := biteship.New(testBiteshipKey)
	c.BaseURL = f.srv.URL
	return c
}

func (f *fakeBiteship) setTracking(orderID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracking["trk-"+orderID] = status
}

func (f *fakeBiteship) lastRate() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rateReqs) == 0 {
		return nil
	}
	return f.rateReqs[len(f.rateReqs)-1]
}

type shipFixture struct {
	*checkoutFixture
	fb *fakeBiteship
}

func setupShipping(t *testing.T) *shipFixture {
	f := setupCheckout(t)
	fb := newFakeBiteship(t)
	f.s.SetShipping(ShippingConfig{
		Biteship:      fb.client(),
		WebhookSecret: testHookSecret,
		Origin: ShipOrigin{AreaID: testOriginArea, PostalCode: "12410", ContactName: "Gudang KaryaKita",
			ContactPhone: "081200000000", Address: "Jl. Gudang No. 1, Jakarta Selatan"},
	})
	return &shipFixture{f, fb}
}

func (f *shipFixture) quote(courier, area string, items ...map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	return call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": items, "courier": courier, "areaId": area}, f.buyer)
}

func (f *shipFixture) place(courier, area string, items ...map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	cust := map[string]any{}
	for k, v := range testCustomer {
		cust[k] = v
	}
	cust["areaId"] = area
	cust["city"] = "Palembang"
	return call(f.s, "POST", "/api/orders", map[string]any{"customer": cust, "items": items, "courier": courier,
		"cost": 1, "shipping": 1, "total": 1}, f.buyer)
}

func (f *shipFixture) productionOrder(t *testing.T, courier string) string {
	t.Helper()
	rec, out := f.place(courier, testDestArea, f.line("M", 2), f.line("L", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, f.buyer)
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("ke produksi: %d %v", rec.Code, out)
	}
	return id
}

func (f *shipFixture) status(t *testing.T, id string) (status, tracking string, events int) {
	t.Helper()
	ctx := context.Background()
	f.pool.QueryRow(ctx, `SELECT status::text, COALESCE(tracking_number,'') FROM orders WHERE id=$1`, id).Scan(&status, &tracking)
	f.pool.QueryRow(ctx, `SELECT count(*) FROM order_events WHERE order_id=$1`, id).Scan(&events)
	return
}

func (f *shipFixture) webhook(secretHeader, secretQuery string, body any) int {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	path := "/api/shipping/biteship/webhook"
	if secretQuery != "" {
		path += "?secret=" + secretQuery
	}
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if secretHeader != "" {
		req.Header.Set("X-Webhook-Secret", secretHeader)
	}
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	return rec.Code
}

func TestShippingConfigAndAreaSearch(t *testing.T) {
	f := setupShipping(t)
	if _, out := call(f.s, "GET", "/api/shipping/config", nil); out["provider"] != "biteship" {
		t.Errorf("config: %v", out)
	}
	if rec, _ := call(f.s, "GET", "/api/shipping/areas?q=sako", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("area tanpa login: %d, want 401", rec.Code)
	}
	if _, out := call(f.s, "GET", "/api/shipping/areas?q=sa", nil, f.buyer); len(out["areas"].([]any)) != 0 {
		t.Errorf("query pendek harus kosong tanpa memanggil Biteship: %v", out)
	}
	rec, out := call(f.s, "GET", "/api/shipping/areas?q=sako+baru", nil, f.buyer)
	areas, _ := out["areas"].([]any)
	if rec.Code != http.StatusOK || len(areas) != 1 {
		t.Fatalf("area: %d %v", rec.Code, out)
	}
	a := areas[0].(map[string]any)
	if a["id"] != testDestArea || a["city"] != "Palembang" || a["postal"] != "30161" {
		t.Errorf("area: %v", a)
	}
	if f.fb.badAuth != 0 {
		t.Error("API key tidak dikirim ke Biteship")
	}
}

func TestQuoteUsesCartWeightAndRejectsForgedCourier(t *testing.T) {
	f := setupShipping(t)
	ctx := context.Background()
	// 3 kaos × 200 g = 600 g → dibulatkan 1 kg: JNE REG 12.000, SiCepat BEST 15.000
	rec, out := f.quote("", testDestArea, f.line("M", 2), f.line("L", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("quote: %d %v", rec.Code, out)
	}
	q := quoteOf(t, out)
	if q["provider"] != "biteship" || num(q["weight"]) != 600 {
		t.Fatalf("provider/berat: %v %v", q["provider"], q["weight"])
	}
	items := f.fb.lastRate()["items"].([]any)
	if w := int(items[0].(map[string]any)["weight"].(float64)) * int(items[0].(map[string]any)["quantity"].(float64)); w != 600 {
		t.Errorf("berat ke Biteship %d, want 600", w)
	}
	if f.fb.lastRate()["origin_area_id"] != testOriginArea || f.fb.lastRate()["destination_area_id"] != testDestArea {
		t.Errorf("area: %v", f.fb.lastRate())
	}
	opts := q["couriers"].([]any)
	if len(opts) != 2 || opts[0].(map[string]any)["id"] != "jne:reg" || num(opts[0].(map[string]any)["cost"]) != 12000 {
		t.Fatalf("opsi kurir (termurah dulu): %v", opts)
	}
	if num(q["shipping"]) != 12000 || q["courier"].(map[string]any)["id"] != "jne:reg" {
		t.Errorf("default termurah: %v %v", q["shipping"], q["courier"])
	}
	sub := num(q["subtotal"])
	if num(q["total"]) != sub+12000 {
		t.Errorf("total %v, want %d", q["total"], sub+12000)
	}

	// admin mengubah berat kaos → ongkir ikut berubah (3 × 450 = 1350 g → 2 kg)
	if rec, _ := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"weightG": 450}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("ubah berat: %d", rec.Code)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `UPDATE product_types SET weight_g = 200 WHERE id='kaos'`) })
	_, out = f.quote("sicepat:best", testDestArea, f.line("M", 2), f.line("L", 1))
	if q := quoteOf(t, out); num(q["weight"]) != 1350 || num(q["shipping"]) != 30000 {
		t.Errorf("setelah berat 450 g: berat %v ongkir %v, want 1350 / 30000", q["weight"], q["shipping"])
	}
	for _, bad := range []int{0, -5, 30001} {
		if rec, _ := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"weightG": bad}, f.admin); rec.Code != http.StatusBadRequest {
			t.Errorf("berat %d: %d, want 400", bad, rec.Code)
		}
	}
	if rec, _ := call(f.s, "PATCH", "/api/product-types/kaos", map[string]any{"weightG": 100}, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator ubah berat: %d, want 403", rec.Code)
	}
	f.pool.Exec(ctx, `UPDATE product_types SET weight_g = 200 WHERE id='kaos'`)

	// pesanan: harga/ongkir dari klien (cost, shipping, total, line price) diabaikan
	rec, out = f.place("sicepat:best", testDestArea, f.line("M", 2), f.line("L", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("buat pesanan: %d %v", rec.Code, out)
	}
	o := out["order"].(map[string]any)
	sh := o["shipping"].(map[string]any)
	if num(sh["cost"]) != 15000 || sh["company"] != "sicepat" || sh["type"] != "best" || sh["courier"] != "SiCepat Besok Sampai Tujuan" {
		t.Errorf("pengiriman tersimpan: %v", sh)
	}
	if num(o["total"]) != num(o["subtotal"])+15000 {
		t.Errorf("total %v", o["total"])
	}
	var area string
	var weight int
	f.pool.QueryRow(ctx, `SELECT dest_area_id, weight_g FROM orders WHERE id=$1`, o["id"]).Scan(&area, &weight)
	if area != testDestArea || weight != 600 {
		t.Errorf("area/berat tersimpan: %q %d", area, weight)
	}

	// kurir palsu, tujuan palsu, tanpa tujuan, tanpa kurir → ditolak
	for name, c := range map[string]struct{ courier, area string }{
		"kurir palsu":  {"jne:murah", testDestArea},
		"kurir flat":   {"JNE REG", testDestArea},
		"tanpa kurir":  {"", testDestArea},
		"tanpa tujuan": {"jne:reg", ""},
		"tujuan palsu": {"jne:reg", "IDNP0IDNC0IDND0IDZ00000"},
	} {
		if rec, out := f.place(c.courier, c.area, f.line("M", 1)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, rec.Code, out)
		}
	}
	// pratinjau dengan kurir tak dikenal jatuh ke yang termurah — harga tetap dari Biteship
	_, out = f.quote("jne:murah", testDestArea, f.line("M", 1))
	if q := quoteOf(t, out); q["courier"].(map[string]any)["id"] != "jne:reg" || num(q["shipping"]) != 12000 {
		t.Errorf("pratinjau kurir palsu: %v", q["courier"])
	}
	// pratinjau sebelum tujuan dipilih: belum ada ongkir
	_, out = f.quote("", "", f.line("M", 1))
	if q := quoteOf(t, out); num(q["shipping"]) != 0 || len(q["couriers"].([]any)) != 0 {
		t.Errorf("tanpa tujuan: %v", q)
	}
}

func TestAdminShipBooksBiteshipAndStoresWaybill(t *testing.T) {
	f := setupShipping(t)
	id := f.productionOrder(t, "jne:reg")
	act := func(cookie *http.Cookie, action string) (int, map[string]any) {
		rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": action}, cookie)
		return rec.Code, out
	}
	if code, _ := act(f.buyer, "ship-biteship"); code != http.StatusForbidden {
		t.Errorf("pembeli membuat pengiriman: %d, want 403", code)
	}
	code, out := act(f.admin, "ship-biteship")
	if code != http.StatusOK {
		t.Fatalf("buat pengiriman: %d %v", code, out)
	}
	o := out["order"].(map[string]any)
	sh := o["shipment"].(map[string]any)
	wantResi := "WYB" + strings.ReplaceAll(id, "-", "")
	if o["status"] != "dikirim" || sh["trackingNumber"] != wantResi || sh["courier"] != "JNE Reguler" || num(sh["shippedAt"]) == 0 {
		t.Fatalf("pengiriman: status=%v shipment=%v", o["status"], sh)
	}
	req := f.fb.orders[id]
	if req == nil {
		t.Fatal("Biteship tidak menerima order")
	}
	if req["courier_company"] != "jne" || req["courier_type"] != "reg" || req["destination_area_id"] != testDestArea ||
		req["origin_area_id"] != testOriginArea || req["delivery_type"] != "now" || req["origin_contact_name"] != "Gudang KaryaKita" {
		t.Errorf("request order: %v", req)
	}
	if w := req["items"].([]any)[0].(map[string]any)["weight"]; num(w) != 600 {
		t.Errorf("berat order %v, want 600", w)
	}
	var boID, trk string
	var cost int
	f.pool.QueryRow(context.Background(), `SELECT biteship_order_id, biteship_tracking_id, shipment_cost FROM orders WHERE id=$1`, id).Scan(&boID, &trk, &cost)
	if boID != "bo-"+id || trk != "trk-"+id || cost != 41000 {
		t.Errorf("id Biteship tersimpan: %q %q %d", boID, trk, cost)
	}
	// tidak bisa dibuat dua kali
	if code, _ := act(f.admin, "ship-biteship"); code != http.StatusBadRequest {
		t.Errorf("buat pengiriman dua kali: %d, want 400", code)
	}

	// respons Biteship hilang setelah order dibuat (reference_id sudah terpakai) → dipulihkan, bukan ganda
	id2 := f.productionOrder(t, "jne:reg")
	f.fb.orders[id2] = map[string]any{"reference_id": id2}
	f.fb.waybills[id2] = "WYB" + strings.ReplaceAll(id2, "-", "")
	f.fb.tracking["trk-"+id2] = "confirmed"
	rec, out2 := call(f.s, "PATCH", "/api/orders/"+id2, map[string]string{"action": "ship-biteship"}, f.admin)
	if rec.Code != http.StatusOK || out2["order"].(map[string]any)["shipment"].(map[string]any)["trackingNumber"] != "WYB"+strings.ReplaceAll(id2, "-", "") {
		t.Errorf("pemulihan reference_id: %d %v", rec.Code, out2)
	}

	// kurir belum menerbitkan resi → tetap produksi, id Biteship tersimpan, bisa diulang
	id3 := f.productionOrder(t, "jne:reg")
	f.fb.noWaybill = true
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id3, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("tanpa resi: %d %v, want 400", rec.Code, out)
	}
	if st, _, _ := f.status(t, id3); st != "produksi" {
		t.Errorf("status tanpa resi: %s, want produksi", st)
	}
	f.fb.noWaybill = false
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id3, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusOK {
		t.Errorf("ulangi: %d %v", rec.Code, out)
	}

	// pesanan tarif flat (tanpa layanan Biteship) → ditolak, jalur manual tetap ada
	live := f.s.shipping
	f.s.SetShipping(ShippingConfig{})
	flat := f.productionFlat(t)
	f.s.SetShipping(ShippingConfig{Biteship: live.Biteship, WebhookSecret: live.WebhookSecret, Origin: live.Origin})
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+flat, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("pesanan flat via Biteship: %d, want 400", rec.Code)
	}
	manual := map[string]string{"action": "ship", "courier": "JNE REG", "trackingNumber": "JNE" + strings.ToUpper(f.tag) + "0001"}
	if rec, out := call(f.s, "PATCH", "/api/orders/"+flat, manual, f.admin); rec.Code != http.StatusOK {
		t.Errorf("resi manual: %d %v", rec.Code, out)
	}
}

func (f *shipFixture) productionFlat(t *testing.T) string {
	t.Helper()
	rec, out := f.order(t, f.buyer, "", f.line("M", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan flat: %d %v", rec.Code, out)
	}
	id := out["order"].(map[string]any)["id"].(string)
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, f.buyer)
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.admin)
	return id
}

func TestBiteshipWebhookDeliveredOnceAndAuthenticated(t *testing.T) {
	f := setupShipping(t)
	id := f.productionOrder(t, "jne:reg")
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("kirim: %d %v", rec.Code, out)
	}
	hook := map[string]any{"event": "order.status", "order_id": "bo-" + id, "status": "delivered", "courier_waybill_id": "x"}
	f.fb.setTracking(id, "delivered") // keadaan sebenarnya di Biteship

	// tanpa / salah rahasia → ditolak, status tetap
	for name, c := range map[string][2]string{"tanpa rahasia": {"", ""}, "rahasia salah": {"salah", ""}, "query salah": {"", "salah"}} {
		if code := f.webhook(c[0], c[1], hook); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if st, _, _ := f.status(t, id); st != "dikirim" {
		t.Fatalf("webhook tak terautentikasi mengubah status: %s", st)
	}

	// request verifikasi Biteship saat mendaftar webhook: body kosong
	if code := f.webhook(testHookSecret, "", nil); code != http.StatusOK {
		t.Errorf("body kosong: %d, want 200", code)
	}

	// webhook sah tetapi Biteship bilang belum tiba (payload dipalsukan) → status tetap
	f.fb.setTracking(id, "inTransit")
	if code := f.webhook(testHookSecret, "", hook); code != http.StatusOK {
		t.Fatalf("webhook palsu: %d", code)
	}
	if st, _, _ := f.status(t, id); st != "dikirim" {
		t.Fatalf("payload webhook dipercaya: %s", st)
	}

	// tiba: dikirim → tiba sekali; header & query keduanya diterima
	f.fb.setTracking(id, "delivered")
	if code := f.webhook(testHookSecret, "", hook); code != http.StatusOK {
		t.Fatalf("webhook tiba: %d", code)
	}
	st, resi, events := f.status(t, id)
	if st != "tiba" {
		t.Fatalf("status setelah tiba: %s", st)
	}
	var deliveredAt int64
	f.pool.QueryRow(context.Background(), `SELECT (extract(epoch FROM delivered_at)*1000)::bigint FROM orders WHERE id=$1`, id).Scan(&deliveredAt)
	if deliveredAt == 0 {
		t.Fatal("delivered_at kosong")
	}

	// ulang (retry Biteship) → tidak ada perubahan
	for _, q := range []string{"", testHookSecret} {
		hdr := testHookSecret
		if q != "" {
			hdr = ""
		}
		if code := f.webhook(hdr, q, hook); code != http.StatusOK {
			t.Errorf("replay: %d, want 200", code)
		}
	}
	st2, resi2, events2 := f.status(t, id)
	var deliveredAt2 int64
	f.pool.QueryRow(context.Background(), `SELECT (extract(epoch FROM delivered_at)*1000)::bigint FROM orders WHERE id=$1`, id).Scan(&deliveredAt2)
	if st2 != "tiba" || resi2 != resi || events2 != events || deliveredAt2 != deliveredAt {
		t.Errorf("replay mengubah pesanan: %s/%s events %d→%d delivered %d→%d", st2, resi2, events, events2, deliveredAt, deliveredAt2)
	}

	// pembeli menyelesaikan; webhook terlambat tidak membuka kembali pesanan
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "confirm"}, f.buyer); rec.Code != http.StatusOK {
		t.Fatalf("konfirmasi: %d", rec.Code)
	}
	if code := f.webhook(testHookSecret, "", hook); code != http.StatusOK {
		t.Errorf("webhook setelah selesai: %d", code)
	}
	if st, _, _ := f.status(t, id); st != "selesai" {
		t.Errorf("status setelah webhook terlambat: %s", st)
	}
	// pesanan yang bukan milik kita → diabaikan
	if code := f.webhook(testHookSecret, "", map[string]any{"event": "order.status", "order_id": "bo-asing", "status": "delivered"}); code != http.StatusOK {
		t.Errorf("order asing: %d, want 200", code)
	}
}

func TestBiteshipWebhookCarriesWaybillChangeAndStatus(t *testing.T) {
	f := setupShipping(t)
	id := f.productionOrder(t, "jne:reg")
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "ship-biteship"}, f.admin)
	f.fb.setTracking(id, "picked")
	hook := map[string]any{"event": "order.status", "order_id": "bo-" + id, "status": "picked"}
	if code := f.webhook(testHookSecret, "", hook); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	var cs string
	f.pool.QueryRow(context.Background(), `SELECT COALESCE(courier_status,'') FROM orders WHERE id=$1`, id).Scan(&cs)
	if cs != "picked" {
		t.Errorf("courier_status %q, want picked", cs)
	}
	_, out := call(f.s, "GET", "/api/orders/"+id, nil, f.admin)
	if sh := out["order"].(map[string]any)["shipment"].(map[string]any); sh["courierStatus"] != "picked" {
		t.Errorf("courierStatus: %v", sh)
	}
	// admin menyinkronkan manual bila webhook tidak sampai
	f.fb.setTracking(id, "delivered")
	rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "sync-shipment"}, f.admin)
	if rec.Code != http.StatusOK || out["order"].(map[string]any)["status"] != "tiba" {
		t.Errorf("sync manual: %d %v", rec.Code, out)
	}
}

func TestFlatShippingWhenBiteshipDisabled(t *testing.T) {
	f := setupCheckout(t) // tanpa SetShipping = tarif flat (dev)
	if _, out := call(f.s, "GET", "/api/shipping/config", nil); out["provider"] != "flat" {
		t.Errorf("config: %v", out)
	}
	if rec, _ := call(f.s, "GET", "/api/shipping/areas?q=sako", nil, f.buyer); rec.Code != http.StatusNotFound {
		t.Errorf("area saat flat: %d, want 404", rec.Code)
	}
	if code := (&shipFixture{checkoutFixture: f}).webhook(testHookSecret, "", map[string]any{"order_id": "x"}); code != http.StatusNotFound {
		t.Errorf("webhook saat flat: %d, want 404", code)
	}
	rec, out := call(f.s, "POST", "/api/checkout/quote", map[string]any{"items": []map[string]any{f.line("M", 2)}, "courier": "JNE REG"}, f.buyer)
	if rec.Code != http.StatusOK {
		t.Fatalf("quote flat: %d %v", rec.Code, out)
	}
	q := quoteOf(t, out)
	if q["provider"] != "flat" || num(q["shipping"]) != 20000 || len(q["couriers"].([]any)) != len(couriers) || num(q["weight"]) != 400 {
		t.Errorf("quote flat: %v", q)
	}
	rec, out = f.order(t, f.buyer, "", f.line("M", 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("pesanan flat: %d %v", rec.Code, out)
	}
	o := out["order"].(map[string]any)
	if sh := o["shipping"].(map[string]any); num(sh["cost"]) != 20000 || sh["courier"] != "JNE REG" || sh["company"] != nil {
		t.Errorf("pengiriman flat: %v", sh)
	}
	// kurir yang tidak dikenal tetap ditolak di mode flat
	if rec, _ := call(f.s, "POST", "/api/orders", map[string]any{"customer": testCustomer, "items": []map[string]any{f.line("M", 1)}, "courier": "jne:reg"}, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("kurir tak dikenal saat flat: %d, want 400", rec.Code)
	}
	// ship-biteship tidak tersedia saat flat
	id := o["id"].(string)
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "pay", "method": "QRIS"}, f.buyer)
	call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "advance"}, f.admin)
	if rec, _ := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("ship-biteship saat flat: %d, want 400", rec.Code)
	}
}

func (f *shipFixture) finance(t *testing.T) (courier, vendorShip int) {
	t.Helper()
	rec, out := call(f.s, "GET", "/api/finance", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("finance: %d %v", rec.Code, out)
	}
	flow := out["flow"].(map[string]any)
	return num(flow["courierCosts"]), num(flow["shippingCost"])
}

func (f *shipFixture) ongkirCharges(id string) (n int) {
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM producer_charges WHERE order_id=$1 AND kind='ongkir'`, id).Scan(&n)
	return
}

func TestBiteshipShipmentCostReplacesVendorShippingCharge(t *testing.T) {
	f := setupShipping(t)
	ctx := context.Background()

	// Biteship booking: vendor ongkir charge is dropped, courier cost shows up in finance
	id := f.productionOrder(t, "jne:reg")
	var buyerShipping int
	f.pool.QueryRow(ctx, `SELECT shipping_cost FROM orders WHERE id=$1`, id).Scan(&buyerShipping)
	if f.ongkirCharges(id) != 1 {
		t.Fatal("tagihan ongkir vendor tidak ada sebelum dikirim")
	}
	courier0, vendorShip0 := f.finance(t)
	if rec, out := call(f.s, "PATCH", "/api/orders/"+id, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("ship-biteship: %d %v", rec.Code, out)
	}
	if n := f.ongkirCharges(id); n != 0 {
		t.Errorf("tagihan ongkir vendor setelah Biteship: %d, want 0", n)
	}
	courier1, vendorShip1 := f.finance(t)
	if courier1-courier0 != 41000 {
		t.Errorf("courierCosts naik %d, want 41000", courier1-courier0)
	}
	if vendorShip0-vendorShip1 != buyerShipping {
		t.Errorf("ongkir vendor turun %d, want %d", vendorShip0-vendorShip1, buyerShipping)
	}

	// manual ship: vendor ships himself → charge stays, no courier cost
	flat := f.productionOrder(t, "jne:reg")
	courier2, vendorShip2 := f.finance(t)
	manual := map[string]string{"action": "ship", "courier": "JNE Reguler", "trackingNumber": "MAN" + strings.ToUpper(f.tag) + "0002"}
	if rec, out := call(f.s, "PATCH", "/api/orders/"+flat, manual, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("ship manual: %d %v", rec.Code, out)
	}
	if f.ongkirCharges(flat) != 1 {
		t.Error("ship manual menghapus tagihan ongkir vendor")
	}
	courier3, vendorShip3 := f.finance(t)
	if courier3 != courier2 || vendorShip3 != vendorShip2 {
		t.Errorf("ship manual mengubah finance: courier %d→%d vendor %d→%d", courier2, courier3, vendorShip2, vendorShip3)
	}
	var cost *int
	f.pool.QueryRow(ctx, `SELECT shipment_cost FROM orders WHERE id=$1`, flat).Scan(&cost)
	if cost != nil {
		t.Errorf("shipment_cost terisi pada ship manual: %d", *cost)
	}

	// vendor ongkir already paid out: left alone, shipment still succeeds
	paid := f.productionOrder(t, "jne:reg")
	var payoutID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO producer_payouts (producer_id, amount, reference) VALUES ('vendor-utama', 1, $1) RETURNING id`, "T"+f.tag).Scan(&payoutID); err != nil {
		t.Fatal(err)
	}
	f.pool.Exec(ctx, `UPDATE producer_charges SET payout_id=$2 WHERE order_id=$1 AND kind='ongkir'`, paid, payoutID)
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM producer_charges WHERE payout_id=$1`, payoutID)
		f.pool.Exec(ctx, `DELETE FROM producer_payouts WHERE id=$1`, payoutID)
	})
	if rec, out := call(f.s, "PATCH", "/api/orders/"+paid, map[string]string{"action": "ship-biteship"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("ship-biteship dengan ongkir sudah dibayar: %d %v", rec.Code, out)
	}
	if f.ongkirCharges(paid) != 1 {
		t.Error("tagihan ongkir vendor yang sudah dibayar ikut terhapus")
	}
}
