package api

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// JSON shapes match the frontend types in lib/types.ts (epoch ms timestamps).

type CartLine struct {
	ProductID string `json:"productId"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Color     string `json:"color"`
	Size      string `json:"size"`
	Qty       int    `json:"qty"`
	Price     int    `json:"price"`
	DesignURI string `json:"designUri,omitempty"`
}

type Customer struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Address string `json:"address"`
	City    string `json:"city"`
	Postal  string `json:"postal"`
}

type Shipping struct {
	Courier string `json:"courier"`
	Cost    int    `json:"cost"`
}

type Payment struct {
	Method string `json:"method"`
	Status string `json:"status"`
	Ref    string `json:"ref"`
	PaidAt *int64 `json:"paidAt,omitempty"`
}

type TimelineEntry struct {
	Status string `json:"status"`
	T      int64  `json:"t"`
}

type Order struct {
	ID          string          `json:"id"`
	CreatedAt   int64           `json:"createdAt"`
	UserID      string          `json:"userId,omitempty"`
	Username    string          `json:"username,omitempty"` // akun pemesan (kosong = guest lama)
	Customer    Customer        `json:"customer"`
	Notes       string          `json:"notes,omitempty"`
	Items       []CartLine      `json:"items"`
	Subtotal    int             `json:"subtotal"`
	Shipping    Shipping        `json:"shipping"`
	Discount    int             `json:"discount"`
	VoucherCode string          `json:"voucherCode,omitempty"`
	Total       int             `json:"total"`
	Payment     Payment         `json:"payment"`
	Status      string          `json:"status"`
	Timeline    []TimelineEntry `json:"timeline"`
}

var phoneRe = regexp.MustCompile(`^\+?[0-9]{8,15}$`)

// validCustomer merapikan & memvalidasi data pengiriman dari form checkout.
func validCustomer(c *Customer, fallbackEmail string) error {
	trim := func(s *string, max int) { *s = truncate(strings.TrimSpace(*s), max) }
	trim(&c.Name, 80)
	trim(&c.Address, 300)
	trim(&c.City, 60)
	trim(&c.Postal, 10)
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	c.Phone = strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(strings.TrimSpace(c.Phone))
	if c.Email == "" {
		c.Email = fallbackEmail
	}
	switch {
	case c.Name == "":
		return checkoutError("Nama penerima wajib diisi")
	case !phoneRe.MatchString(c.Phone):
		return checkoutError("Nomor WhatsApp tidak valid (8–15 digit)")
	case len(c.Address) < 10:
		return checkoutError("Alamat lengkap wajib diisi (jalan, nomor, RT/RW)")
	case c.City == "":
		return checkoutError("Kota / kabupaten wajib diisi")
	case c.Postal != "" && !regexp.MustCompile(`^[0-9]{5}$`).MatchString(c.Postal):
		return checkoutError("Kode pos harus 5 digit")
	case c.Email != "" && !validEmail(c.Email):
		return checkoutError("Format email tidak valid")
	}
	return nil
}

// POST /api/orders (login) — membuat pesanan dari keranjang. Harga, ongkir,
// dan voucher dihitung ulang di server lewat quoteOrder.
func (s *Server) postOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Customer    Customer   `json:"customer"`
		Notes       string     `json:"notes"`
		Items       []CartLine `json:"items"`
		Courier     string     `json:"courier"`
		Voucher     string     `json:"voucher"`
		SaveProfile bool       `json:"saveProfile"`
		Session     string     `json:"session"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	u := userFrom(r)
	if err := validCustomer(&body.Customer, u.Email); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := courierByID(body.Courier); !ok {
		errJSON(w, http.StatusBadRequest, "Pilih kurir pengiriman")
		return
	}
	notes := truncate(strings.TrimSpace(body.Notes), 300)

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	quote, err := quoteOrder(ctx, tx, u, body.Items, body.Courier, body.Voucher, true)
	var ce checkoutError
	if errors.As(err, &ce) {
		errJSON(w, http.StatusBadRequest, ce.Error())
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// voucher yang diketik tapi tidak berlaku → tolak, jangan diam-diam ditagih harga penuh
	if quote.VoucherError != "" {
		errJSON(w, http.StatusBadRequest, "Voucher: "+quote.VoucherError)
		return
	}

	id, err := newOrderID(ctx, tx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var voucherCode any
	if quote.Voucher != nil {
		voucherCode = quote.Voucher.Code
	}
	c := body.Customer
	if _, err := tx.Exec(ctx, `
		INSERT INTO orders (id, user_id, cust_name, cust_email, cust_phone, cust_address, cust_city, cust_postal, notes,
		                    subtotal, courier, shipping_cost, discount, voucher_code, total)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		id, u.ID, c.Name, c.Email, c.Phone, c.Address, c.City, c.Postal, notes,
		quote.Subtotal, quote.Courier.ID, quote.Shipping, quote.Discount, voucherCode, quote.Total); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, it := range quote.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, listing_id, title, type, color, size, qty, unit_price, design_uri)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id, nilIfEmpty(it.listingID), it.Title, it.Type, it.Color, it.Size, it.Qty, it.Price, nilIfEmpty(it.DesignURI)); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (order_id, method, ref, amount) VALUES ($1,'belum dipilih',$2,$3)`,
		id, fmt.Sprintf("MID-%08d", rand.IntN(100_000_000)), quote.Total); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := "Pesanan dibuat"
	if quote.Voucher != nil {
		label += " · voucher " + quote.Voucher.Code
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1, $2)`, id, label); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.SaveProfile {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET phone=$2, address=$3, city=$4, postal_code=$5 WHERE id=$1`,
			u.ID, c.Phone, c.Address, c.City, c.Postal); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	session := body.Session
	if session == "" {
		session = "anon"
	}
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO track_events (session_id, type, label, value) VALUES ($1,'begin_checkout',$2,$3)`,
		session, id, quote.Total)

	order, err := s.loadOrder(ctx, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

// newOrderID: KK-YYMMDD-nnnn, diulang bila kebetulan sudah dipakai.
func newOrderID(ctx context.Context, q querier) (string, error) {
	day := time.Now().Format("060102")
	for range 20 {
		id := fmt.Sprintf("KK-%s-%d", day, 1000+rand.IntN(9000))
		var taken bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM orders WHERE id=$1)`, id).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return id, nil
		}
	}
	return "", errors.New("gagal membuat nomor pesanan unik")
}

// GET /api/orders (admin) ?status=&q=&limit=&offset= → {orders, total}
func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where := []string{}
	args := []any{}
	if st := q.Get("status"); st != "" {
		args = append(args, st)
		where = append(where, fmt.Sprintf("o.status::text = $%d", len(args)))
	}
	if term := strings.TrimSpace(q.Get("q")); term != "" {
		args = append(args, "%"+strings.ToLower(term)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(lower(o.id) LIKE $%d OR lower(o.cust_name) LIKE $%d OR lower(o.cust_email) LIKE $%d OR lower(COALESCE(u.username,'')) LIKE $%d)", n, n, n, n))
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	limit := clampInt(q.Get("limit"), 50, 1, 200)
	offset := clampInt(q.Get("offset"), 0, 0, 1_000_000)

	ctx := r.Context()
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM orders o LEFT JOIN users u ON u.id = o.user_id `+clause, args...).Scan(&total); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	orders, err := s.loadOrdersPage(ctx, clause, args, limit, offset)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "total": total})
}

// GET /api/orders/mine (login) — riwayat pesanan akun ini
func (s *Server) getMyOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.loadOrdersPage(r.Context(), `WHERE o.user_id = $1`, []any{userFrom(r).ID}, 100, 0)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

// canSeeOrder: pemilik pesanan atau admin. Selain itu dianggap tidak ada
// (404, bukan 403) supaya nomor pesanan orang lain tidak bisa ditebak-tebak.
func (s *Server) ownedOrder(w http.ResponseWriter, r *http.Request) (*Order, bool) {
	order, err := s.loadOrder(r.Context(), r.PathValue("id"))
	u := userFrom(r)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Role != "admin" && order.UserID != u.ID) {
		errJSON(w, http.StatusNotFound, "Pesanan tidak ditemukan")
		return nil, false
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return order, true
}

// GET /api/orders/{id} (pemilik / admin)
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	if order, ok := s.ownedOrder(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"order": order})
	}
}

// PATCH /api/orders/{id}
//
//	{action:"pay", method}  — pemilik / admin (pengganti webhook gateway di prototipe)
//	{action:"advance"}      — admin saja (simulasi produksi & pengiriman)
func (s *Server) patchOrder(w http.ResponseWriter, r *http.Request) {
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	var body struct {
		Action string `json:"action"`
		Method string `json:"method"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	ctx := r.Context()
	var err error
	switch body.Action {
	case "pay":
		err = s.payOrder(ctx, order.ID, truncate(body.Method, 40))
	case "advance":
		if userFrom(r).Role != "admin" {
			errJSON(w, http.StatusForbidden, "Hanya admin yang bisa memajukan status pesanan")
			return
		}
		err = s.advanceOrder(ctx, order.ID)
	default:
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusBadRequest, "Aksi tidak valid")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	order, err = s.loadOrder(ctx, order.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func clampInt(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

// payOrder = pengganti webhook notifikasi gateway pembayaran.
// Menandai lunas, mencatat timeline, menaikkan counter terjual, dan
// membukukan royalti kreator per item.
func (s *Server) payOrder(ctx context.Context, id, method string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var payStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM payments WHERE order_id=$1 FOR UPDATE`, id).Scan(&payStatus); err != nil {
		return err // pgx.ErrNoRows bila pesanan tak ada
	}
	if payStatus != "pending" {
		return nil // sudah dibayar — idempoten seperti webhook sungguhan
	}

	if method != "" {
		if _, err := tx.Exec(ctx, `UPDATE payments SET method=$2 WHERE order_id=$1`, id, method); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='paid', paid_at=now() WHERE order_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status='dibayar' WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO order_events (order_id, label)
		 SELECT $1, 'Pembayaran diterima (' || method || ')' FROM payments WHERE order_id=$1`, id); err != nil {
		return err
	}
	// counter terjual + royalti kreator (share dari designers.royalty_share)
	if _, err := tx.Exec(ctx, `
		UPDATE listings l SET sold = l.sold + oi.qty
		FROM order_items oi WHERE oi.order_id = $1 AND oi.listing_id = l.id`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO royalties (designer_id, order_item_id, amount)
		SELECT d.designer_id, oi.id, round(oi.unit_price * oi.qty * dr.royalty_share)::int
		FROM order_items oi
		JOIN listings l ON l.id = oi.listing_id
		JOIN designs d ON d.id = l.design_id
		JOIN designers dr ON dr.id = d.designer_id
		WHERE oi.order_id = $1
		ON CONFLICT (order_item_id) DO NOTHING`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var nextStatus = map[string][2]string{
	"dibayar":  {"produksi", "Masuk antrean produksi (cetak DTG)"},
	"produksi": {"dikirim", "Paket diserahkan ke kurir"},
	"dikirim":  {"selesai", "Pesanan diterima pelanggan"},
}

func (s *Server) advanceOrder(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		return err
	}
	step, ok := nextStatus[status]
	if !ok {
		return nil // status akhir / belum dibayar — tidak ada langkah berikutnya
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status=$2 WHERE id=$1`, id, step[0]); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, id, step[1]); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- pesanan masuk untuk kreator --------------------------------------------

type CreatorOrderItem struct {
	OrderID       string `json:"orderId"`
	CreatedAt     int64  `json:"createdAt"`
	Status        string `json:"status"`
	PaymentStatus string `json:"paymentStatus"`
	ListingID     string `json:"listingId"`
	Title         string `json:"title"`
	Type          string `json:"type"`
	Color         string `json:"color"`
	Size          string `json:"size"`
	Qty           int    `json:"qty"`
	UnitPrice     int    `json:"unitPrice"`
	DesignURI     string `json:"designUri,omitempty"`
	Royalty       int    `json:"royalty"`       // sudah dibukukan, atau estimasi bila belum dibayar
	RoyaltyBooked bool   `json:"royaltyBooked"` // true = pembayaran lunas, royalti tercatat
	Buyer         string `json:"buyer"`         // nama depan + inisial saja (privasi pembeli)
	City          string `json:"city"`
}

// GET /api/designer/orders (kreator; admin dengan ?designer=) — item pesanan
// yang memuat produk milik kreator ini. Alamat & kontak pembeli tidak dibuka:
// produksi & pengiriman diurus platform.
func (s *Server) getDesignerOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	designerID := ""
	if u.Designer != nil {
		designerID = u.Designer.ID
	}
	if u.Role == "admin" && r.URL.Query().Get("designer") != "" {
		designerID = r.URL.Query().Get("designer")
	}
	if designerID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"items": []CreatorOrderItem{}, "summary": map[string]int{}})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.id, (extract(epoch FROM o.created_at)*1000)::bigint, o.status::text, p.status::text,
		       oi.listing_id, oi.title, oi.type, oi.color, oi.size, oi.qty, oi.unit_price, COALESCE(oi.design_uri,''),
		       COALESCE(ry.amount, round(oi.unit_price * oi.qty * dr.royalty_share)::int), ry.id IS NOT NULL,
		       o.cust_name, o.cust_city
		FROM order_items oi
		JOIN orders o     ON o.id = oi.order_id
		JOIN payments p   ON p.order_id = o.id
		JOIN listings l   ON l.id = oi.listing_id
		JOIN designs d    ON d.id = l.design_id
		JOIN designers dr ON dr.id = d.designer_id
		LEFT JOIN royalties ry ON ry.order_item_id = oi.id
		WHERE d.designer_id = $1
		ORDER BY o.created_at DESC, oi.id
		LIMIT 200`, designerID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	items := []CreatorOrderItem{}
	orders := map[string]bool{}
	summary := map[string]int{"orders": 0, "units": 0, "paidUnits": 0, "royaltyBooked": 0, "royaltyPending": 0}
	for rows.Next() {
		var it CreatorOrderItem
		var name string
		if err := rows.Scan(&it.OrderID, &it.CreatedAt, &it.Status, &it.PaymentStatus,
			&it.ListingID, &it.Title, &it.Type, &it.Color, &it.Size, &it.Qty, &it.UnitPrice, &it.DesignURI,
			&it.Royalty, &it.RoyaltyBooked, &name, &it.City); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		it.Buyer = shortName(name)
		items = append(items, it)
		orders[it.OrderID] = true
		summary["units"] += it.Qty
		if it.RoyaltyBooked {
			summary["paidUnits"] += it.Qty
			summary["royaltyBooked"] += it.Royalty
		} else {
			summary["royaltyPending"] += it.Royalty
		}
	}
	if err := rows.Err(); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary["orders"] = len(orders)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "summary": summary})
}

// shortName: "Budi Santoso Wijaya" → "Budi S."
func shortName(name string) string {
	parts := strings.Fields(name)
	switch len(parts) {
	case 0:
		return "Pembeli"
	case 1:
		return parts[0]
	}
	return parts[0] + " " + strings.ToUpper(string([]rune(parts[1])[:1])) + "."
}

// ---- loading ------------------------------------------------------------

func (s *Server) loadOrder(ctx context.Context, id string) (*Order, error) {
	orders, err := s.loadOrdersPage(ctx, `WHERE o.id = $1`, []any{id}, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return nil, pgx.ErrNoRows
	}
	return &orders[0], nil
}

func (s *Server) loadOrders(ctx context.Context, limit int) ([]Order, error) {
	return s.loadOrdersPage(ctx, ``, nil, limit, 0)
}

// loadOrdersPage: where boleh mengacu ke alias o (orders), p (payments), u (users).
func (s *Server) loadOrdersPage(ctx context.Context, where string, args []any, limit, offset int) ([]Order, error) {
	query := fmt.Sprintf(`
		SELECT o.id, (extract(epoch FROM o.created_at)*1000)::bigint,
		       COALESCE(o.user_id,''), COALESCE(u.username,''),
		       o.cust_name, o.cust_email, o.cust_phone, o.cust_address, o.cust_city, o.cust_postal, o.notes,
		       o.subtotal, o.courier, o.shipping_cost, o.discount, COALESCE(o.voucher_code,''), o.total, o.status,
		       p.method, p.status, p.ref,
		       (extract(epoch FROM p.paid_at)*1000)::bigint
		FROM orders o
		JOIN payments p ON p.order_id = o.id
		LEFT JOIN users u ON u.id = o.user_id
		%s
		ORDER BY o.created_at DESC
		LIMIT %d OFFSET %d`, where, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	orders := []Order{}
	index := map[string]int{}
	ids := []string{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.CreatedAt, &o.UserID, &o.Username,
			&o.Customer.Name, &o.Customer.Email, &o.Customer.Phone, &o.Customer.Address, &o.Customer.City,
			&o.Customer.Postal, &o.Notes,
			&o.Subtotal, &o.Shipping.Courier, &o.Shipping.Cost, &o.Discount, &o.VoucherCode, &o.Total, &o.Status,
			&o.Payment.Method, &o.Payment.Status, &o.Payment.Ref, &o.Payment.PaidAt); err != nil {
			return nil, err
		}
		o.Items = []CartLine{}
		o.Timeline = []TimelineEntry{}
		index[o.ID] = len(orders)
		ids = append(ids, o.ID)
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return orders, nil
	}

	itemRows, err := s.pool.Query(ctx, `
		SELECT order_id, COALESCE(listing_id,''), title, type, color, size, qty, unit_price, COALESCE(design_uri,'')
		FROM order_items WHERE order_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var oid string
		var it CartLine
		if err := itemRows.Scan(&oid, &it.ProductID, &it.Title, &it.Type, &it.Color, &it.Size, &it.Qty, &it.Price, &it.DesignURI); err != nil {
			return nil, err
		}
		i := index[oid]
		orders[i].Items = append(orders[i].Items, it)
	}
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	evRows, err := s.pool.Query(ctx, `
		SELECT order_id, label, (extract(epoch FROM at)*1000)::bigint
		FROM order_events WHERE order_id = ANY($1) ORDER BY at, id`, ids)
	if err != nil {
		return nil, err
	}
	defer evRows.Close()
	for evRows.Next() {
		var oid string
		var e TimelineEntry
		if err := evRows.Scan(&oid, &e.Status, &e.T); err != nil {
			return nil, err
		}
		i := index[oid]
		orders[i].Timeline = append(orders[i].Timeline, e)
	}
	return orders, evRows.Err()
}
