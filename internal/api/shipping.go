package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/biteship"
)

// Pengiriman.
//
// Penyedia (dipilih di cmd/api/main.go dari env):
//   biteship — BITESHIP_API_KEY diisi: ongkir langsung dari Biteship (berat paket × tujuan),
//              admin membuat pengiriman (resi otomatis), status "tiba" datang dari webhook
//              pelacakan Biteship.
//   flat     — tanpa kunci (dev): tarif flat di checkout.go, resi diisi admin manual.
//
// Webhook Biteship TIDAK bertanda tangan: kita mengautentikasi dengan rahasia di
// header (atau ?secret=) dan tetap mengambil ulang status dari API pelacakan
// sebelum bertindak, jadi webhook palsu tidak bisa menandai paket tiba.

type ShipOrigin struct {
	AreaID       string // Biteship area id gudang/vendor
	PostalCode   string
	ContactName  string
	ContactPhone string
	Address      string
	Note         string
}

type ShippingConfig struct {
	Biteship      *biteship.Client
	WebhookSecret string
	Origin        ShipOrigin
	Couriers      string        // kode kurir yang ditanyakan ke Biteship, mis. "jne,sicepat,anteraja"
	CacheTTL      time.Duration // 0 = tanpa cache (tes); hemat kuota Rates/Maps Biteship

	cache *ttlCache
}

// SetShipping memilih penyedia pengiriman. Tanpa pemanggilan ini = flat.
func (s *Server) SetShipping(cfg ShippingConfig) {
	cfg.cache = &ttlCache{items: map[string]cacheEntry{}}
	if cfg.Couriers == "" {
		cfg.Couriers = "jne,sicepat,anteraja"
	}
	s.shipping = cfg
}

func (c *ShippingConfig) enabled() bool { return c != nil && c.Biteship != nil }

func (s *Server) shippingProvider() string {
	if s.shipping.enabled() {
		return "biteship"
	}
	return "flat"
}

// ---- cache sederhana (TTL, dibatasi ukurannya) ---------------------------------

type cacheEntry struct {
	val any
	exp time.Time
}

type ttlCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
}

const cacheMaxEntries = 2000

func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.exp) {
		delete(c.items, key)
		return nil, false
	}
	return e.val, true
}

func (c *ttlCache) put(key string, val any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= cacheMaxEntries {
		now := time.Now()
		for k, e := range c.items {
			if now.After(e.exp) {
				delete(c.items, k)
			}
		}
		if len(c.items) >= cacheMaxEntries {
			clear(c.items)
		}
	}
	c.items[key] = cacheEntry{val: val, exp: time.Now().Add(ttl)}
}

// ---- tarif ------------------------------------------------------------------

// errShippingDown: Biteship tidak bisa dihubungi / menolak karena kuota atau kunci
// (masalah di sisi kita, bukan input pembeli) → 502.
var errShippingDown = errors.New("layanan ongkir tidak tersedia")

// shipTarget = tujuan pengiriman yang dipakai quoteOrder untuk menghitung ongkir.
type shipTarget struct {
	cfg    *ShippingConfig
	areaID string
}

func (t shipTarget) live() bool { return t.cfg.enabled() }

// splitCourierID memecah ID kurir Biteship "jne:reg" → ("jne", "reg"). Tarif flat → ("", "").
func splitCourierID(id string) (company, ctype string) {
	company, ctype, _ = strings.Cut(id, ":")
	return company, ctype
}

func etaText(duration, unit string) string {
	u := map[string]string{"days": "hari", "day": "hari", "hours": "jam", "hour": "jam"}[unit]
	if u == "" {
		u = unit
	}
	return strings.TrimSpace(strings.ReplaceAll(duration, " - ", "–") + " " + u)
}

// liveRates menanyakan tarif ke Biteship untuk berat & tujuan ini; hasil terurut dari termurah.
func (c *ShippingConfig) liveRates(ctx context.Context, destAreaID string, weightG, value int) ([]Courier, error) {
	key := fmt.Sprintf("r:%s|%s|%d|%d", c.Origin.AreaID, destAreaID, weightG, value)
	if c.CacheTTL > 0 {
		if v, ok := c.cache.get(key); ok {
			return v.([]Courier), nil
		}
	}
	rates, err := c.Biteship.Rates(ctx, biteship.RateRequest{
		OriginAreaID: c.Origin.AreaID,
		DestAreaID:   destAreaID,
		Couriers:     c.Couriers,
		Items:        []biteship.Item{{Name: "Pesanan KaryaKita", Value: value, Quantity: 1, Weight: weightG}},
	})
	if err != nil {
		var apiErr *biteship.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
			return nil, checkoutError("Tujuan pengiriman tidak valid atau belum dilayani kurir — pilih ulang kota/kecamatan")
		}
		return nil, fmt.Errorf("%w: %v", errShippingDown, err)
	}
	seen := map[string]bool{}
	out := []Courier{}
	for _, r := range rates {
		id := r.Company + ":" + r.Type
		if r.Company == "" || r.Type == "" || r.Price < 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Courier{ID: id, Label: strings.TrimSpace(r.CompanyName + " " + r.TypeName), ETA: etaText(r.Duration, r.DurationUnit), Cost: r.Price})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost < out[j].Cost
		}
		return out[i].ID < out[j].ID
	})
	if c.CacheTTL > 0 {
		c.cache.put(key, out, c.CacheTTL)
	}
	return out, nil
}

// resolveLiveCourier memilih layanan kurir dari tarif Biteship. Nilai dari browser
// hanya dipakai sebagai KUNCI pemilihan; harganya selalu dari Biteship.
// create=true (membuat pesanan): kurir wajib dipilih. Pratinjau tanpa pilihan → termurah.
func resolveLiveCourier(ctx context.Context, t shipTarget, courierID string, weightG, subtotal int, create bool) (picked Courier, options []Courier, err error) {
	options = []Courier{}
	if t.areaID == "" {
		if create {
			return Courier{}, options, checkoutError("Pilih kota/kecamatan tujuan dari daftar")
		}
		return Courier{}, options, nil // pratinjau sebelum tujuan dipilih
	}
	options, err = t.cfg.liveRates(ctx, t.areaID, weightG, subtotal)
	if err != nil {
		return Courier{}, []Courier{}, err
	}
	if len(options) == 0 {
		return Courier{}, options, checkoutError("Belum ada kurir yang melayani tujuan ini")
	}
	if courierID == "" {
		if create {
			return Courier{}, options, checkoutError("Pilih kurir pengiriman")
		}
		return options[0], options, nil
	}
	for _, o := range options {
		if o.ID == courierID {
			return o, options, nil
		}
	}
	if !create {
		return options[0], options, nil // pratinjau: pilihan lama tidak berlaku lagi → termurah
	}
	return Courier{}, options, checkoutError("Layanan kurir tidak tersedia untuk tujuan ini — pilih ulang kurir")
}

// orderWeight: Σ qty × product_types.weight_g (gram).
func orderWeight(ctx context.Context, q querier, lines []QuoteLine) (int, error) {
	perType := map[string]int{}
	total := 0
	for _, l := range lines {
		w, ok := perType[l.Type]
		if !ok {
			if err := q.QueryRow(ctx, `SELECT weight_g FROM product_types WHERE id = $1`, l.Type).Scan(&w); err != nil {
				return 0, err
			}
			perType[l.Type] = w
		}
		total += w * l.Qty
	}
	return total, nil
}

// ---- endpoint publik ----------------------------------------------------------

// GET /api/shipping/config (publik) → {provider: "biteship" | "flat"}
func (s *Server) getShippingConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"provider": s.shippingProvider()})
}

type AreaOption struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	City     string `json:"city"`
	Province string `json:"province"`
	District string `json:"district"`
	Postal   string `json:"postal"`
}

// GET /api/shipping/areas?q= (login) — autocomplete kota/kecamatan tujuan.
// Hanya untuk akun login dan ≥3 huruf: setiap panggilan memakai kuota Maps Biteship.
func (s *Server) getShippingAreas(w http.ResponseWriter, r *http.Request) {
	if !s.shipping.enabled() {
		errJSON(w, http.StatusNotFound, "Pencarian area tidak aktif")
		return
	}
	q := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if n := len([]rune(q)); n < 3 || n > 60 {
		writeJSON(w, http.StatusOK, map[string]any{"areas": []AreaOption{}})
		return
	}
	key := "a:" + strings.ToLower(q)
	if s.shipping.CacheTTL > 0 {
		if v, ok := s.shipping.cache.get(key); ok {
			writeJSON(w, http.StatusOK, map[string]any{"areas": v})
			return
		}
	}
	areas, err := s.shipping.Biteship.SearchAreas(r.Context(), q)
	if err != nil {
		log.Printf("biteship SearchAreas %q: %v", q, err)
		errJSON(w, http.StatusBadGateway, "Gagal mencari area. Coba lagi sebentar lagi.")
		return
	}
	out := make([]AreaOption, 0, min(len(areas), 15))
	for _, a := range areas {
		if len(out) == 15 {
			break
		}
		postal := ""
		if a.PostalCode > 0 {
			postal = fmt.Sprintf("%05d", a.PostalCode)
		}
		out = append(out, AreaOption{ID: a.ID, Name: a.Name, City: a.City, Province: a.Province, District: a.District, Postal: postal})
	}
	if s.shipping.CacheTTL > 0 {
		s.shipping.cache.put(key, out, 12*s.shipping.CacheTTL)
	}
	writeJSON(w, http.StatusOK, map[string]any{"areas": out})
}

// ---- admin: buat pengiriman -----------------------------------------------------

var courierStatusEvents = map[string]string{
	"picked":          "Paket dijemput kurir",
	"onHold":          "Pengiriman ditahan kurir — menunggu kendala terselesaikan",
	"returnInTransit": "Paket sedang dikembalikan ke pengirim",
	"returned":        "Paket diretur ke pengirim",
	"rejected":        "Pengiriman ditolak kurir",
	"courierNotFound": "Kurir tidak ditemukan — pengiriman dibatalkan",
	"cancelled":       "Pengiriman dibatalkan di Biteship",
	"disposed":        "Paket dimusnahkan oleh kurir",
}

// bookShipment membuat pengiriman di Biteship untuk pesanan berstatus produksi,
// lalu mencatat produksi → dikirim dengan resi dari Biteship (lewat shipOrder,
// jalur yang sama dengan pengisian resi manual). Aman dipanggil ulang: reference_id
// = nomor pesanan, jadi Biteship tidak membuat pesanan ganda.
func (s *Server) bookShipment(ctx context.Context, id string) error {
	if !s.shipping.enabled() {
		return checkoutError("Integrasi Biteship belum aktif — isi kurir dan nomor resi manual")
	}
	var o struct {
		status, courierLabel, company, ctype, area                string
		name, phone, email, address, postal, notes, biteshipOrder string
		weight, subtotal                                          int
	}
	err := s.pool.QueryRow(ctx, `
		SELECT status::text, courier, courier_company, courier_type, dest_area_id,
		       cust_name, cust_phone, cust_email, cust_address, cust_postal, notes,
		       COALESCE(biteship_order_id,''), weight_g, subtotal
		FROM orders WHERE id = $1`, id).Scan(&o.status, &o.courierLabel, &o.company, &o.ctype, &o.area,
		&o.name, &o.phone, &o.email, &o.address, &o.postal, &o.notes, &o.biteshipOrder, &o.weight, &o.subtotal)
	if err != nil {
		return err
	}
	if o.status != "produksi" {
		return checkoutError("Pesanan harus berstatus produksi sebelum dikirim")
	}
	if o.company == "" || o.ctype == "" || o.area == "" || o.weight <= 0 {
		return checkoutError("Pesanan ini dibuat tanpa layanan kurir Biteship — isi kurir dan nomor resi manual")
	}
	org := s.shipping.Origin
	if org.AreaID == "" || org.ContactName == "" || org.ContactPhone == "" || org.Address == "" {
		return errors.New("alamat asal pengiriman belum lengkap (SHIP_ORIGIN_*)")
	}

	bs := s.shipping.Biteship
	var bo *biteship.Order
	if o.biteshipOrder != "" {
		bo, err = bs.GetOrder(ctx, o.biteshipOrder) // percobaan sebelumnya: resi belum terbit
	} else {
		bo, err = bs.CreateOrder(ctx, biteship.OrderRequest{
			ReferenceID: id,
			Origin: biteship.Contact{Name: org.ContactName, Phone: org.ContactPhone, Address: org.Address,
				AreaID: org.AreaID, PostalCode: org.PostalCode, Note: org.Note},
			Destination: biteship.Contact{Name: o.name, Phone: o.phone, Email: o.email, Address: o.address,
				AreaID: o.area, PostalCode: o.postal, Note: o.notes},
			Company: o.company,
			Type:    o.ctype,
			Items:   []biteship.Item{{Name: "Pesanan KaryaKita " + id, Value: o.subtotal, Quantity: 1, Weight: o.weight}},
		})
		if dupID, _, dup := biteship.DuplicateOrder(err); dup {
			bo, err = bs.GetOrder(ctx, dupID) // sudah pernah dibuat (mis. respons sebelumnya hilang)
		}
	}
	if err != nil {
		var apiErr *biteship.APIError
		if errors.As(err, &apiErr) {
			log.Printf("biteship order %s: %v", id, err)
			return checkoutError("Biteship menolak pengiriman: " + apiErr.Message)
		}
		return fmt.Errorf("%w: %v", errShippingDown, err)
	}

	// simpan dulu id Biteship (status masih produksi) supaya pelacakan tetap jalan
	// walau resi belum terbit atau langkah berikutnya gagal
	if _, err := s.pool.Exec(ctx, `
		UPDATE orders SET biteship_order_id=$2, biteship_tracking_id=NULLIF($3,'')
		WHERE id=$1 AND status='produksi'`, id, bo.ID, bo.TrackingID); err != nil {
		return err
	}
	waybill := strings.ToUpper(strings.Join(strings.Fields(bo.WaybillID), ""))
	if waybill == "" {
		return checkoutError("Pengiriman sudah dibuat di Biteship, tetapi kurir belum menerbitkan nomor resi. Coba lagi beberapa saat lagi, atau isi resi manual.")
	}
	if !trackingRe.MatchString(waybill) {
		return checkoutError("Resi dari Biteship (" + waybill + ") tidak sesuai format — isi kurir dan resi manual")
	}
	// Biteship bills the platform's balance, so the buyer's shipping is not passed on to the vendor:
	// in the same transaction as produksi → dikirim, record the real courier cost and drop the
	// vendor's unpaid 'ongkir' charge. shipment_cost is only set on this path (manual ship keeps
	// the vendor charge), which is how finance tells the two apart.
	price := bo.Price
	return s.shipOrderWith(ctx, id, o.courierLabel, waybill, false, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE orders SET shipment_cost=$2 WHERE id=$1`, id, price); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM producer_charges WHERE order_id=$1 AND kind='ongkir' AND payout_id IS NULL`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var paidOut bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM producer_charges WHERE order_id=$1 AND kind='ongkir' AND payout_id IS NOT NULL)`, id).Scan(&paidOut); err != nil {
				return err
			}
			if paidOut {
				log.Printf("PERINGATAN: pesanan %s dikirim lewat Biteship tetapi ongkir vendor sudah dibayarkan — periksa manual", id)
			}
		}
		return nil
	})
}

// ---- pelacakan: webhook + sinkron manual -------------------------------------------

// syncShipment mengambil status pelacakan dari Biteship dan menerapkannya:
// menyimpan status kurir, memperbarui resi bila kurir menggantinya, dan
// memanggil markDelivered saat paket tiba. Idempoten.
func (s *Server) syncShipment(ctx context.Context, id string, refreshPrice bool) error {
	if !s.shipping.enabled() {
		return checkoutError("Integrasi Biteship belum aktif")
	}
	var biteshipOrder, trackingID string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(biteship_order_id,''), COALESCE(biteship_tracking_id,'') FROM orders WHERE id=$1`, id).
		Scan(&biteshipOrder, &trackingID); err != nil {
		return err
	}
	if biteshipOrder == "" {
		return checkoutError("Pesanan ini tidak dikirim lewat Biteship")
	}
	bs := s.shipping.Biteship
	if trackingID == "" || refreshPrice {
		bo, err := bs.GetOrder(ctx, biteshipOrder)
		if err != nil {
			return err
		}
		// price refresh only for orders that went through ship-biteship (shipment_cost set there)
		if _, err := s.pool.Exec(ctx, `UPDATE orders SET biteship_tracking_id=COALESCE(NULLIF($2,''), biteship_tracking_id),
			shipment_cost = CASE WHEN shipment_cost IS NULL THEN NULL ELSE $3::int END WHERE id=$1`,
			id, bo.TrackingID, bo.Price); err != nil {
			return err
		}
		if trackingID == "" {
			trackingID = bo.TrackingID
		}
	}
	if trackingID == "" {
		return nil // kurir belum membuat pelacakan
	}
	tr, err := bs.Tracking(ctx, trackingID)
	if err != nil {
		return err
	}
	if err := s.applyTracking(ctx, id, tr); err != nil {
		return err
	}
	if tr.Status == biteship.StatusDelivered {
		var ce checkoutError
		if err := s.markDelivered(ctx, id); err != nil && !errors.As(err, &ce) {
			return err // checkoutError = sudah tiba/selesai (webhook ulang) → tidak ada yang perlu dilakukan
		}
	}
	return nil
}

func (s *Server) applyTracking(ctx context.Context, id string, tr *biteship.Tracking) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status, current, last string
	if err := tx.QueryRow(ctx, `SELECT status::text, COALESCE(tracking_number,''), COALESCE(courier_status,'') FROM orders WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &current, &last); err != nil {
		return err
	}
	if tr.Status != "" && tr.Status != last {
		if _, err := tx.Exec(ctx, `UPDATE orders SET courier_status=$2 WHERE id=$1`, id, tr.Status); err != nil {
			return err
		}
		if label := courierStatusEvents[tr.Status]; label != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, id, label); err != nil {
				return err
			}
		}
	}
	// kurir bisa mengganti resi setelah paket dijemput (dialihkan ke kurir berikutnya)
	waybill := strings.ToUpper(strings.Join(strings.Fields(tr.WaybillID), ""))
	if status == "dikirim" && waybill != "" && waybill != current && trackingRe.MatchString(waybill) {
		tag, err := tx.Exec(ctx, `
			UPDATE orders SET tracking_number=$2 WHERE id=$1
			  AND NOT EXISTS (SELECT 1 FROM orders o2 WHERE o2.id <> $1 AND lower(o2.shipped_courier) = lower(orders.shipped_courier) AND o2.tracking_number = $2)`,
			id, waybill)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, id, "Resi diperbarui kurir: "+waybill); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// POST /api/shipping/biteship/webhook (publik, dipanggil Biteship).
// Autentikasi: rahasia di header X-Webhook-Secret (atau ?secret=). Isi webhook tidak
// dipercaya — status diambil ulang dari API pelacakan Biteship.
func (s *Server) postBiteshipWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.shipping.enabled() {
		errJSON(w, http.StatusNotFound, "Tidak aktif")
		return
	}
	got := r.Header.Get("X-Webhook-Secret")
	if got == "" {
		got = r.URL.Query().Get("secret")
	}
	want := s.shipping.WebhookSecret
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		errJSON(w, http.StatusUnauthorized, "Tidak diizinkan")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	// Biteship memverifikasi URL saat webhook didaftarkan dengan body kosong
	if len(bytes.TrimSpace(raw)) == 0 {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	var ev struct {
		Event   string `json:"event"`
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil || ev.OrderID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	ctx := r.Context()
	var orderID string
	err = s.pool.QueryRow(ctx, `SELECT id FROM orders WHERE biteship_order_id = $1`, ev.OrderID).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true}) // bukan pesanan kita
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncShipment(ctx, orderID, ev.Event == "order.price"); err != nil {
		log.Printf("biteship webhook %s (%s): %v", orderID, ev.Event, err)
		errJSON(w, http.StatusBadGateway, "Gagal memverifikasi ke Biteship") // 5xx → Biteship mengulang bila retry aktif
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
