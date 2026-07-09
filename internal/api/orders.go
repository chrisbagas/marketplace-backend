package api

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
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
	ID        string          `json:"id"`
	CreatedAt int64           `json:"createdAt"`
	Customer  Customer        `json:"customer"`
	Items     []CartLine      `json:"items"`
	Subtotal  int             `json:"subtotal"`
	Shipping  Shipping        `json:"shipping"`
	Total     int             `json:"total"`
	Payment   Payment         `json:"payment"`
	Status    string          `json:"status"`
	Timeline  []TimelineEntry `json:"timeline"`
}

func (s *Server) postOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Customer Customer   `json:"customer"`
		Items    []CartLine `json:"items"`
		Shipping Shipping   `json:"shipping"`
		Method   string     `json:"method"`
		Session  string     `json:"session"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	if body.Customer.Name == "" || len(body.Items) == 0 || body.Shipping.Courier == "" || body.Method == "" {
		errJSON(w, http.StatusBadRequest, "Data pesanan tidak lengkap")
		return
	}

	subtotal := 0
	for _, it := range body.Items {
		if it.Qty <= 0 || it.Price < 0 {
			errJSON(w, http.StatusBadRequest, "Item pesanan tidak valid")
			return
		}
		subtotal += it.Price * it.Qty
	}
	total := subtotal + body.Shipping.Cost
	now := time.Now()
	id := fmt.Sprintf("KK-%s-%d", now.Format("060102"), 1000+rand.IntN(9000))

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO orders (id, cust_name, cust_email, cust_phone, cust_address, cust_city, subtotal, courier, shipping_cost, total)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, body.Customer.Name, body.Customer.Email, body.Customer.Phone, body.Customer.Address, body.Customer.City,
		subtotal, body.Shipping.Courier, body.Shipping.Cost, total); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, it := range body.Items {
		// listing_id hanya diisi bila produk katalog masih ada (snapshot tetap tersimpan)
		var listingID any
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listings WHERE id=$1)`, it.ProductID).Scan(&exists); err == nil && exists {
			listingID = it.ProductID
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_items (order_id, listing_id, title, type, color, size, qty, unit_price, design_uri)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id, listingID, it.Title, it.Type, it.Color, it.Size, it.Qty, it.Price, nilIfEmpty(it.DesignURI)); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (order_id, method, ref, amount) VALUES ($1,$2,$3,$4)`,
		id, body.Method, fmt.Sprintf("MID-%08d", rand.IntN(100_000_000)), total); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1, 'Pesanan dibuat')`, id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
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
		session, id, total)

	order, err := s.loadOrder(ctx, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.loadOrders(r.Context(), 50)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	order, err := s.loadOrder(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "Pesanan tidak ditemukan")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Server) patchOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
		err = s.payOrder(ctx, id, body.Method)
	case "advance":
		err = s.advanceOrder(ctx, id)
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
	order, err := s.loadOrder(ctx, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
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

// ---- loading ------------------------------------------------------------

func (s *Server) loadOrder(ctx context.Context, id string) (*Order, error) {
	orders, err := s.loadOrdersWhere(ctx, `WHERE o.id = $1`, []any{id}, 1)
	if err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return nil, pgx.ErrNoRows
	}
	return &orders[0], nil
}

func (s *Server) loadOrders(ctx context.Context, limit int) ([]Order, error) {
	return s.loadOrdersWhere(ctx, ``, nil, limit)
}

func (s *Server) loadOrdersWhere(ctx context.Context, where string, args []any, limit int) ([]Order, error) {
	query := fmt.Sprintf(`
		SELECT o.id, (extract(epoch FROM o.created_at)*1000)::bigint,
		       o.cust_name, o.cust_email, o.cust_phone, o.cust_address, o.cust_city,
		       o.subtotal, o.courier, o.shipping_cost, o.total, o.status,
		       p.method, p.status, p.ref,
		       (extract(epoch FROM p.paid_at)*1000)::bigint
		FROM orders o
		JOIN payments p ON p.order_id = o.id
		%s
		ORDER BY o.created_at DESC
		LIMIT %d`, where, limit)

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
		if err := rows.Scan(&o.ID, &o.CreatedAt,
			&o.Customer.Name, &o.Customer.Email, &o.Customer.Phone, &o.Customer.Address, &o.Customer.City,
			&o.Subtotal, &o.Shipping.Courier, &o.Shipping.Cost, &o.Total, &o.Status,
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
