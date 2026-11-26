package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Keuangan platform (admin): ke mana uang setiap pesanan pergi, berapa yang
// masih menjadi hak produsen & kreator, dan pembayaran tagihan produsen.
// Pembukuan per pesanan dibuat saat lunas oleh db.BookPaidOrder.

type FinanceFlow struct {
	Orders         int `json:"orders"`
	Received       int `json:"received"`       // dibayar pembeli (setelah diskon)
	Subtotal       int `json:"subtotal"`       // nilai produk
	Shipping       int `json:"shipping"`       // ongkir dibayar pembeli
	Discount       int `json:"discount"`       // voucher, ditanggung platform
	Production     int `json:"production"`     // hak produsen: biaya produksi
	ShippingCost   int `json:"shippingCost"`   // hak produsen: ongkir (produsen yang mengirim)
	CourierCosts   int `json:"courierCosts"`   // ongkos kurir Biteship yang dibayar platform (bukan lewat produsen)
	Creators       int `json:"creators"`       // royalti kreator
	GatewayFees    int `json:"gatewayFees"`    // estimasi biaya Midtrans
	PlatformIncome int `json:"platformIncome"` // sisanya
}

type FinanceOwed struct {
	ProducersReady        int `json:"producersReady"`        // pesanan sudah dikirim, belum dibayar ke produsen
	ProducersInProduction int `json:"producersInProduction"` // pesanan belum dikirim
	CreatorsHeld          int `json:"creatorsHeld"`          // royalti pesanan belum selesai
	CreatorsAvailable     int `json:"creatorsAvailable"`     // bisa/sedang ditarik kreator
	CashEstimate          int `json:"cashEstimate"`          // perkiraan saldo rekening dari transaksi platform
	PlatformCash          int `json:"platformCash"`          // bagian saldo yang benar-benar milik platform
}

// period: from inklusif, to eksklusif (nol = tanpa batas).
func (s *Server) financeFlow(ctx context.Context, from, to time.Time) (FinanceFlow, error) {
	var f FinanceFlow
	err := s.pool.QueryRow(ctx, `
		WITH paid AS (
			SELECT o.id, o.subtotal, o.shipping_cost, o.discount, o.shipment_cost, p.amount, p.gateway_fee
			FROM orders o JOIN payments p ON p.order_id = o.id
			WHERE p.status = 'paid'
			  AND ($1::timestamptz IS NULL OR p.paid_at >= $1)
			  AND ($2::timestamptz IS NULL OR p.paid_at <  $2)
		)
		SELECT count(*)::int,
		       COALESCE(sum(amount), 0)::int, COALESCE(sum(subtotal), 0)::int,
		       COALESCE(sum(shipping_cost), 0)::int, COALESCE(sum(discount), 0)::int,
		       COALESCE(sum(gateway_fee), 0)::int, COALESCE(sum(shipment_cost), 0)::int,
		       COALESCE((SELECT sum(c.amount) FROM producer_charges c WHERE c.kind = 'produksi' AND c.order_id IN (SELECT id FROM paid)), 0)::int,
		       COALESCE((SELECT sum(c.amount) FROM producer_charges c WHERE c.kind = 'ongkir'   AND c.order_id IN (SELECT id FROM paid)), 0)::int,
		       COALESCE((SELECT sum(r.amount) FROM royalties r JOIN order_items oi ON oi.id = r.order_item_id
		                 WHERE oi.order_id IN (SELECT id FROM paid)), 0)::int
		FROM paid`, nullTime(from), nullTime(to)).Scan(
		&f.Orders, &f.Received, &f.Subtotal, &f.Shipping, &f.Discount, &f.GatewayFees, &f.CourierCosts,
		&f.Production, &f.ShippingCost, &f.Creators)
	f.PlatformIncome = f.Received - f.Production - f.ShippingCost - f.CourierCosts - f.Creators - f.GatewayFees
	return f, err
}

func (s *Server) financeOwed(ctx context.Context) (FinanceOwed, error) {
	var o FinanceOwed
	var received, fees, creatorPaid, producerPaid, creatorsBooked, courierPaid int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT sum(c.amount) FROM producer_charges c JOIN orders o ON o.id = c.order_id
		            WHERE c.payout_id IS NULL AND o.status IN ('dikirim','tiba','selesai')), 0)::int,
		  COALESCE((SELECT sum(c.amount) FROM producer_charges c JOIN orders o ON o.id = c.order_id
		            WHERE c.payout_id IS NULL AND o.status NOT IN ('dikirim','tiba','selesai')), 0)::int,
		  COALESCE((SELECT sum(r.amount) FROM royalties r JOIN order_items oi ON oi.id = r.order_item_id
		            JOIN orders o ON o.id = oi.order_id WHERE o.status NOT IN ('selesai','dibatalkan')), 0)::int,
		  COALESCE((SELECT sum(r.amount) FROM royalties r JOIN order_items oi ON oi.id = r.order_item_id
		            JOIN orders o ON o.id = oi.order_id WHERE o.status <> 'dibatalkan'), 0)::int,
		  COALESCE((SELECT sum(amount) FROM payments WHERE status = 'paid'), 0)::int,
		  COALESCE((SELECT sum(gateway_fee) FROM payments WHERE status = 'paid'), 0)::int,
		  COALESCE((SELECT sum(amount) FROM payouts WHERE status = 'dibayar'), 0)::int,
		  COALESCE((SELECT sum(amount) FROM producer_payouts), 0)::int,
		  COALESCE((SELECT sum(o.shipment_cost) FROM orders o JOIN payments p ON p.order_id = o.id WHERE p.status = 'paid'), 0)::int`).Scan(
		&o.ProducersReady, &o.ProducersInProduction, &o.CreatorsHeld, &creatorsBooked,
		&received, &fees, &creatorPaid, &producerPaid, &courierPaid)
	o.CreatorsAvailable = max(0, creatorsBooked-o.CreatorsHeld-creatorPaid)
	o.CashEstimate = received - fees - creatorPaid - producerPaid - courierPaid
	o.PlatformCash = o.CashEstimate - o.ProducersReady - o.ProducersInProduction - o.CreatorsHeld - o.CreatorsAvailable
	return o, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// GET /api/finance?from=YYYY-MM-DD&to=YYYY-MM-DD (to inklusif, zona WIB) — laporan admin.
func (s *Server) getFinance(w http.ResponseWriter, r *http.Request) {
	wib := time.FixedZone("WIB", 7*3600)
	parse := func(k string) (time.Time, bool) {
		v := r.URL.Query().Get(k)
		if v == "" {
			return time.Time{}, true
		}
		t, err := time.ParseInLocation("2006-01-02", v, wib)
		return t, err == nil
	}
	from, ok1 := parse("from")
	to, ok2 := parse("to")
	if !ok1 || !ok2 {
		errJSON(w, http.StatusBadRequest, "Format tanggal YYYY-MM-DD")
		return
	}
	if !to.IsZero() {
		to = to.AddDate(0, 0, 1)
	}
	flow, err := s.financeFlow(r.Context(), from, to)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	owed, err := s.financeOwed(r.Context())
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flow": flow, "owed": owed})
}

// ---- produsen ---------------------------------------------------------------

type ProductCost struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	BasePrice      int    `json:"basePrice"`
	ProductionCost int    `json:"productionCost"`
	WeightG        int    `json:"weightG"` // berat per pcs untuk ongkir
}

type ProducerCharge struct {
	OrderID     string `json:"orderId"`
	OrderStatus string `json:"orderStatus"`
	PaidAt      int64  `json:"paidAt"`
	Items       int    `json:"items"` // jumlah pcs
	Production  int    `json:"production"`
	Shipping    int    `json:"shipping"`
	Ready       bool   `json:"ready"` // sudah dikirim → boleh dibayar
}

type ProducerPayout struct {
	ID        int64  `json:"id"`
	Amount    int    `json:"amount"`
	Reference string `json:"reference"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

type Producer struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Contact  string           `json:"contact"`
	Bank     *BankAccount     `json:"bank,omitempty"`
	Active   bool             `json:"active"`
	Products []ProductCost    `json:"products"`
	Ready    int              `json:"ready"`
	InProd   int              `json:"inProduction"`
	Charges  []ProducerCharge `json:"charges"` // belum dibayar, per pesanan
	Payouts  []ProducerPayout `json:"payouts"`
}

const readyStatuses = `('dikirim','tiba','selesai')`

// GET /api/producers — produsen beserta tarif, tagihan berjalan, dan riwayat pembayaran.
func (s *Server) getProducers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, COALESCE(contact,''), COALESCE(bank_name,''), COALESCE(bank_account,''), COALESCE(bank_holder,''), active
		FROM producers ORDER BY created_at`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := []Producer{}
	for rows.Next() {
		var p Producer
		var b BankAccount
		if err := rows.Scan(&p.ID, &p.Name, &p.Contact, &b.BankName, &b.Account, &b.Holder, &p.Active); err != nil {
			rows.Close()
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if b.Account != "" {
			p.Bank = &b
		}
		list = append(list, p)
	}
	rows.Close()
	for i := range list {
		if err := s.loadProducerDetail(ctx, &list[i]); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"producers": list})
}

func (s *Server) loadProducerDetail(ctx context.Context, p *Producer) error {
	p.Products, p.Charges, p.Payouts = []ProductCost{}, []ProducerCharge{}, []ProducerPayout{}
	rows, err := s.pool.Query(ctx, `
		SELECT id, label, base_price, production_cost, weight_g FROM product_types WHERE producer_id = $1 ORDER BY base_price`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c ProductCost
		if err := rows.Scan(&c.ID, &c.Label, &c.BasePrice, &c.ProductionCost, &c.WeightG); err != nil {
			rows.Close()
			return err
		}
		p.Products = append(p.Products, c)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `
		SELECT o.id, o.status::text, COALESCE((extract(epoch FROM p.paid_at)*1000)::bigint, 0),
		       COALESCE((SELECT sum(oi.qty) FROM order_items oi JOIN producer_charges c2 ON c2.order_item_id = oi.id
		                 WHERE c2.order_id = o.id AND c2.producer_id = $1 AND c2.payout_id IS NULL), 0)::int,
		       COALESCE(sum(c.amount) FILTER (WHERE c.kind = 'produksi'), 0)::int,
		       COALESCE(sum(c.amount) FILTER (WHERE c.kind = 'ongkir'), 0)::int,
		       o.status IN `+readyStatuses+`
		FROM producer_charges c
		JOIN orders o   ON o.id = c.order_id
		JOIN payments p ON p.order_id = o.id
		WHERE c.producer_id = $1 AND c.payout_id IS NULL
		GROUP BY o.id, o.status, p.paid_at
		ORDER BY p.paid_at`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c ProducerCharge
		if err := rows.Scan(&c.OrderID, &c.OrderStatus, &c.PaidAt, &c.Items, &c.Production, &c.Shipping, &c.Ready); err != nil {
			rows.Close()
			return err
		}
		if c.Ready {
			p.Ready += c.Production + c.Shipping
		} else {
			p.InProd += c.Production + c.Shipping
		}
		p.Charges = append(p.Charges, c)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `
		SELECT id, amount, reference, COALESCE(note,''), (extract(epoch FROM created_at)*1000)::bigint
		FROM producer_payouts WHERE producer_id = $1 ORDER BY created_at DESC LIMIT 50`, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var po ProducerPayout
		if err := rows.Scan(&po.ID, &po.Amount, &po.Reference, &po.Note, &po.CreatedAt); err != nil {
			return err
		}
		p.Payouts = append(p.Payouts, po)
	}
	return rows.Err()
}

// PATCH /api/producers/{id} {name, contact, bank:{bankName, account, holder}}
func (s *Server) patchProducer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string       `json:"name"`
		Contact string       `json:"contact"`
		Bank    *BankAccount `json:"bank"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	name := truncate(strings.TrimSpace(body.Name), 80)
	if len(name) < 2 {
		errJSON(w, http.StatusBadRequest, "Isi nama produsen")
		return
	}
	var bankName, account, holder any
	if body.Bank != nil && (body.Bank.Account != "" || body.Bank.Holder != "") {
		b, msg := cleanBank(*body.Bank)
		if msg != "" {
			errJSON(w, http.StatusBadRequest, msg)
			return
		}
		bankName, account, holder = b.BankName, b.Account, b.Holder
	}
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE producers SET name=$2, contact=NULLIF($3,''), bank_name=$4, bank_account=$5, bank_holder=$6 WHERE id=$1`,
		r.PathValue("id"), name, truncate(strings.TrimSpace(body.Contact), 120), bankName, account, holder)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Produsen tidak ditemukan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PATCH /api/product-types/{id} {productionCost?, weightG?} — tarif vendor per pcs
// dan berat paket per pcs (dasar hitung ongkir). Berlaku untuk pesanan BARU
// (pesanan lama menyimpan nilainya saat checkout).
func (s *Server) patchProductType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProductionCost *int `json:"productionCost"`
		WeightG        *int `json:"weightG"`
	}
	if err := readJSON(r, &body); err != nil || (body.ProductionCost == nil && body.WeightG == nil) {
		errJSON(w, http.StatusBadRequest, "Isi biaya produksi atau berat")
		return
	}
	var base int
	err := s.pool.QueryRow(r.Context(), `SELECT base_price FROM product_types WHERE id=$1`, r.PathValue("id")).Scan(&base)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Jenis produk tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.ProductionCost != nil {
		if c := *body.ProductionCost; c < 0 || c > base {
			errJSON(w, http.StatusBadRequest, "Biaya produksi harus antara Rp0 dan harga dasar "+rupiah(base))
			return
		}
	}
	if body.WeightG != nil {
		if g := *body.WeightG; g < 1 || g > 30_000 {
			errJSON(w, http.StatusBadRequest, "Berat harus antara 1 dan 30.000 gram")
			return
		}
	}
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE product_types SET production_cost = COALESCE($2, production_cost), weight_g = COALESCE($3, weight_g) WHERE id=$1`,
		r.PathValue("id"), body.ProductionCost, body.WeightG); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/producers/{id}/payouts {reference, note} — catat transfer ke
// produsen. Melunasi SEMUA tagihan yang siap (pesanan sudah dikirim);
// nominal dihitung server, bukan dari browser.
func (s *Server) postProducerPayout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reference string `json:"reference"`
		Note      string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ref := truncate(strings.TrimSpace(body.Reference), 60)
	if ref == "" {
		errJSON(w, http.StatusBadRequest, "Isi nomor referensi transfer")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	// kunci produsen: dua klik bersamaan tidak membayar tagihan yang sama dua kali
	if err := tx.QueryRow(ctx, `SELECT id FROM producers WHERE id=$1 FOR UPDATE`, id).Scan(&id); err != nil {
		errJSON(w, http.StatusNotFound, "Produsen tidak ditemukan")
		return
	}
	var amount int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(c.amount), 0)::int FROM producer_charges c JOIN orders o ON o.id = c.order_id
		WHERE c.producer_id = $1 AND c.payout_id IS NULL AND o.status IN `+readyStatuses, id).Scan(&amount); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if amount <= 0 {
		errJSON(w, http.StatusBadRequest, "Belum ada tagihan siap bayar (pesanan yang sudah dikirim)")
		return
	}
	var payoutID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO producer_payouts (producer_id, amount, reference, note) VALUES ($1,$2,$3,NULLIF($4,'')) RETURNING id`,
		id, amount, ref, truncate(strings.TrimSpace(body.Note), 300)).Scan(&payoutID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE producer_charges c SET payout_id = $2 FROM orders o
		WHERE o.id = c.order_id AND c.producer_id = $1 AND c.payout_id IS NULL AND o.status IN `+readyStatuses,
		id, payoutID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"payout": ProducerPayout{ID: payoutID, Amount: amount, Reference: ref, CreatedAt: time.Now().UnixMilli()}})
}
