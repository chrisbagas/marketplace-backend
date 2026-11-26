package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/db"
	"karyakita/api/internal/midtrans"
)

// Pembatalan & pengembalian dana.
//
//   - Pesanan belum dibayar: pembeli (atau admin) membatalkan lewat
//     POST /api/orders/{id}/cancel. Transaksi yang masih terbuka di Midtrans ikut
//     dibatalkan supaya tidak bisa dibayar belakangan; voucher otomatis bebas.
//   - Pesanan sudah dibayar tapi belum produksi (`dibayar`): hanya admin, lewat
//     POST /api/orders/{id}/refund — pesanan menjadi `dibatalkan`, dana dikembalikan
//     penuh (Midtrans Refund API bila metodenya didukung, selain itu transfer manual
//     dengan nomor referensi), royalti dibalik, tagihan produsen dihapus.
//   - Pengembalian dana karena komplain: lihat complaints.go (memakai issueRefund yang sama).
//
// Pembukuan: payments.status tetap `paid` (uangnya memang masuk); pengembalian
// tercatat di `refunds`. Royalti: royalties.amount = royalti bersih,
// reversed_amount = yang ditarik kembali (db.ReverseRoyalties).

// Refund = satu baris tabel refunds, seperti dikirim ke frontend.
type Refund struct {
	ID        int64  `json:"id"`
	Amount    int    `json:"amount"`
	Method    string `json:"method"` // gateway | manual
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"createdAt"`
}

// gatewayFailure: Midtrans menolak / tidak terjangkau saat refund. Transaksi DB
// dibatalkan; admin bisa mencoba lagi atau memilih transfer manual.
type gatewayFailure string

func (e gatewayFailure) Error() string { return string(e) }

// gatewayRefundMode: "full" | "partial" bila pembayaran ini bisa direfund lewat
// Midtrans API ("full" = hanya nominal penuh), "" bila harus transfer manual.
func (s *Server) gatewayRefundMode(gateway, paymentType string) string {
	if s.paymentProvider() != "midtrans" || s.payments.Midtrans == nil || gateway != "midtrans" {
		return ""
	}
	ok, partial := midtrans.RefundSupport(paymentType)
	switch {
	case !ok:
		return ""
	case partial:
		return "partial"
	}
	return "full"
}

type refundInput struct {
	OrderID   string
	Amount    int
	Method    string // gateway | manual
	Reference string // wajib untuk manual
	Reason    string
	AdminID   string
	Cancel    bool // batalkan pesanan (status dibayar → dibatalkan) + balik tagihan produsen
}

// issueRefund mencatat satu refund di dalam tx. Pemanggil sudah mengunci baris
// pesanan dan memeriksa statusnya. Mengembalikan id baris refunds.
//
// Urutan: validasi → panggil Midtrans (bila gateway) → balik buku → catat.
// Panggilan Midtrans memakai refund_key tetap per pesanan+urutan, jadi mengulang
// permintaan yang sama tidak mengembalikan dana dua kali.
func (s *Server) issueRefund(ctx context.Context, tx pgx.Tx, in refundInput) (int64, error) {
	var paid, already, count int
	var payStatus, gateway, gwOrderID, payType string
	if err := tx.QueryRow(ctx, `
		SELECT p.amount, p.status::text, p.gateway, COALESCE(p.gateway_order_id,''), COALESCE(p.payment_type,''),
		       COALESCE((SELECT sum(amount) FROM refunds WHERE order_id = p.order_id), 0)::int,
		       (SELECT count(*) FROM refunds WHERE order_id = p.order_id)::int
		FROM payments p WHERE p.order_id = $1 FOR UPDATE`, in.OrderID).Scan(
		&paid, &payStatus, &gateway, &gwOrderID, &payType, &already, &count); err != nil {
		return 0, err
	}
	switch {
	case payStatus != "paid":
		return 0, checkoutError("Pesanan ini belum dibayar, tidak ada dana yang bisa dikembalikan")
	case paid-already <= 0:
		return 0, checkoutError("Tidak ada dana yang bisa dikembalikan untuk pesanan ini")
	case in.Amount <= 0:
		return 0, checkoutError("Isi nominal pengembalian dana")
	case in.Amount > paid-already:
		return 0, checkoutError("Nominal melebihi sisa dana yang bisa dikembalikan (" + rupiah(paid-already) + ")")
	}

	reference := truncate(strings.TrimSpace(in.Reference), 60)
	switch in.Method {
	case "manual":
		if reference == "" {
			return 0, checkoutError("Isi nomor referensi transfer pengembalian dana")
		}
	case "gateway":
		mode := s.gatewayRefundMode(gateway, payType)
		switch {
		case mode == "":
			return 0, checkoutError("Metode pembayaran ini tidak bisa direfund otomatis lewat Midtrans — pilih transfer manual")
		case mode == "full" && (in.Amount != paid || already != 0):
			return 0, checkoutError("Metode pembayaran ini hanya mendukung refund penuh lewat Midtrans — kembalikan seluruh nominal atau pilih transfer manual")
		}
		key := fmt.Sprintf("kk-%s-r%d", in.OrderID, count+1)
		if _, err := s.payments.Midtrans.Refund(ctx, gwOrderID, midtrans.RefundRequest{
			RefundKey: key, Amount: in.Amount, Reason: truncate(in.Reason, 200),
		}); err != nil {
			log.Printf("midtrans Refund %s: %v", gwOrderID, err)
			return 0, gatewayFailure("Refund lewat Midtrans gagal: " + err.Error() + ". Coba lagi, atau pilih transfer manual.")
		}
		reference = key
	default:
		return 0, checkoutError("Pilih cara pengembalian dana")
	}

	vendor := 0
	if in.Cancel {
		tag, err := tx.Exec(ctx, `UPDATE orders SET status='dibatalkan', cancelled_at=now() WHERE id=$1 AND status='dibayar'`, in.OrderID)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, checkoutError("Hanya pesanan berstatus dibayar (belum produksi) yang bisa dibatalkan dengan refund")
		}
		if vendor, err = db.ReverseCancelledOrder(ctx, tx, in.OrderID); err != nil {
			return 0, err
		}
	}
	royalty, err := db.ReverseRoyalties(ctx, tx, in.OrderID, already+in.Amount, paid)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO refunds (order_id, amount, method, reference, reason, royalty_reversed, vendor_reversed, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		in.OrderID, in.Amount, in.Method, reference, in.Reason, royalty, vendor, nilIfEmpty(in.AdminID)).Scan(&id); err != nil {
		return 0, err
	}
	via := "transfer manual · " + reference
	if in.Method == "gateway" {
		via = "Midtrans"
	}
	label := fmt.Sprintf("Dana %s dikembalikan lewat %s — %s", rupiah(in.Amount), via, in.Reason)
	if in.Cancel {
		label = "Pesanan dibatalkan admin. " + label
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, in.OrderID, label); err != nil {
		return 0, err
	}
	return id, nil
}

// writeRefundError memetakan galat pembatalan/refund ke respons HTTP.
// Mengembalikan false bila err == nil.
func writeRefundError(w http.ResponseWriter, err error) bool {
	var ce checkoutError
	var gf gatewayFailure
	switch {
	case err == nil:
		return false
	case errors.As(err, &ce):
		errJSON(w, http.StatusBadRequest, ce.Error())
	case errors.As(err, &gf):
		errJSON(w, http.StatusBadGateway, gf.Error())
	case errors.Is(err, db.ErrChargesPaidOut):
		errJSON(w, http.StatusConflict, "Tagihan produsen pesanan ini sudah dibayarkan — selesaikan manual dengan produsen")
	case errors.Is(err, pgx.ErrNoRows):
		errJSON(w, http.StatusNotFound, "Pesanan tidak ditemukan")
	default:
		errJSON(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// POST /api/orders/{id}/cancel (pemilik / admin) — batalkan pesanan yang belum dibayar.
func (s *Server) postOrderCancel(w http.ResponseWriter, r *http.Request) {
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	u := userFrom(r)
	ctx := r.Context()
	switch {
	case order.Status == "dibatalkan":
		errJSON(w, http.StatusConflict, "Pesanan ini sudah dibatalkan")
		return
	case order.Payment.Status == "paid" || order.Status != "menunggu-pembayaran":
		if u.Role != "admin" {
			errJSON(w, http.StatusForbidden, "Pesanan yang sudah dibayar tidak bisa dibatalkan sendiri — hubungi admin untuk pembatalan dan pengembalian dana")
		} else {
			errJSON(w, http.StatusBadRequest, "Pesanan ini sudah dibayar — batalkan lewat pengembalian dana")
		}
		return
	}

	// transaksi yang masih terbuka di Midtrans ditutup dulu; bila ternyata sudah
	// dibayar, pesanan tidak jadi dibatalkan
	paidNow, err := s.cancelAtGateway(ctx, order.ID)
	if err != nil {
		log.Printf("cancel gateway %s: %v", order.ID, err)
		errJSON(w, http.StatusBadGateway, "Gagal membatalkan transaksi di gateway pembayaran. Coba lagi sebentar lagi.")
		return
	}
	if paidNow {
		errJSON(w, http.StatusConflict, "Pembayaran ternyata sudah masuk — pesanan tidak bisa dibatalkan. Hubungi admin bila ingin pengembalian dana.")
		return
	}
	who := "dibatalkan oleh pembeli"
	if u.Role == "admin" && order.UserID != u.ID {
		who = "dibatalkan oleh admin"
	}
	if err := s.cancelUnpaidOrder(ctx, order.ID, who); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	fresh, err := s.loadOrder(ctx, order.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fresh.Status != "dibatalkan" { // dibayar tepat di antara dua langkah di atas
		errJSON(w, http.StatusConflict, "Pembayaran ternyata sudah masuk — pesanan tidak bisa dibatalkan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": fresh})
}

// cancelAtGateway menutup transaksi Midtrans yang masih terbuka untuk pesanan
// ini. paidNow=true bila Midtrans ternyata sudah menerima pembayaran (status
// diterapkan ke pesanan, jadi jangan dibatalkan).
func (s *Server) cancelAtGateway(ctx context.Context, orderID string) (paidNow bool, err error) {
	if s.paymentProvider() != "midtrans" {
		return false, nil
	}
	var gwID string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(gateway_order_id,'') FROM payments WHERE order_id=$1`, orderID).Scan(&gwID); err != nil {
		return false, err
	}
	if gwID == "" {
		return false, nil // belum pernah membuka popup bayar
	}
	c := s.payments.Midtrans
	settled := func(n *midtrans.Notification) bool {
		return n.TransactionStatus == "settlement" || (n.TransactionStatus == "capture" && n.FraudStatus != "challenge")
	}
	apply := func(n *midtrans.Notification) error {
		_, err := s.applyGatewayStatus(ctx, n, "cancel", true)
		return err
	}
	n, err := c.Status(ctx, gwID)
	if errors.Is(err, midtrans.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if settled(n) {
		return true, apply(n)
	}
	switch n.TransactionStatus {
	case "pending", "authorize", "capture":
		_, err := c.Cancel(ctx, gwID)
		var apiErr *midtrans.APIError
		switch {
		case err == nil || errors.Is(err, midtrans.ErrNotFound):
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusPreconditionFailed:
			// 412: berubah di tengah jalan (kedaluwarsa / baru dibayar) — baca ulang
			n2, err2 := c.Status(ctx, gwID)
			if err2 != nil && !errors.Is(err2, midtrans.ErrNotFound) {
				return false, err2
			}
			if err2 == nil && settled(n2) {
				return true, apply(n2)
			}
		default:
			return false, err
		}
	}
	return false, nil
}

// POST /api/orders/{id}/refund (admin) {method?: "gateway"|"manual", reference, reason}
// — batalkan pesanan berstatus `dibayar` dan kembalikan dana penuh.
func (s *Server) postOrderRefund(w http.ResponseWriter, r *http.Request) {
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	var body struct {
		Method    string `json:"method"`
		Reference string `json:"reference"`
		Reason    string `json:"reason"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	reason := truncate(strings.TrimSpace(body.Reason), 200)
	if len([]rune(reason)) < 3 {
		errJSON(w, http.StatusBadRequest, "Tulis alasan pembatalan")
		return
	}
	method := body.Method
	if method == "" {
		method = "manual"
		if order.Payment.RefundMode != "" {
			method = "gateway"
		}
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	var status string
	if err := tx.QueryRow(ctx, `SELECT status::text FROM orders WHERE id=$1 FOR UPDATE`, order.ID).Scan(&status); err != nil {
		writeRefundError(w, err)
		return
	}
	if status != "dibayar" {
		errJSON(w, http.StatusBadRequest, "Hanya pesanan berstatus dibayar (belum masuk produksi) yang bisa dibatalkan dengan refund")
		return
	}
	var paid int
	if err := tx.QueryRow(ctx, `SELECT amount FROM payments WHERE order_id=$1`, order.ID).Scan(&paid); err != nil {
		writeRefundError(w, err)
		return
	}
	if _, err := s.issueRefund(ctx, tx, refundInput{
		OrderID: order.ID, Amount: paid, Method: method, Reference: body.Reference,
		Reason: reason, AdminID: userFrom(r).ID, Cancel: true,
	}); writeRefundError(w, err) {
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	fresh, err := s.loadOrder(ctx, order.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": fresh})
}

// attachRefundData fills the refund / complaint / reprint fields of loaded
// orders (index maps order id → position in orders).
func (s *Server) attachRefundData(ctx context.Context, orders []Order, index map[string]int) error {
	ids := make([]string, 0, len(orders))
	for i := range orders {
		orders[i].Refunds = []Refund{}
		ids = append(ids, orders[i].ID)
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT order_id, id, amount, method, reference, reason, (extract(epoch FROM created_at)*1000)::bigint
		FROM refunds WHERE order_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var oid string
		var rf Refund
		if err := rows.Scan(&oid, &rf.ID, &rf.Amount, &rf.Method, &rf.Reference, &rf.Reason, &rf.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		o := &orders[index[oid]]
		o.Refunds = append(o.Refunds, rf)
		o.Refunded += rf.Amount
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.pool.Query(ctx, `SELECT `+complaintCols+complaintFrom+` WHERE c.order_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		c, err := scanComplaint(rows)
		if err != nil {
			rows.Close()
			return err
		}
		o := &orders[index[c.OrderID]]
		o.Complaint = &c
		if o.Status == "tiba" && o.Shipment != nil {
			switch {
			case c.Status == complaintStatusOpen:
				o.AutoCompleteAt = 0 // paused while the complaint is open
			case c.ResolvedAt > 0:
				o.AutoCompleteAt = max(o.AutoCompleteAt, c.ResolvedAt+AutoCompleteAfter.Milliseconds())
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT o.id, COALESCE(o.reprint_of,''), p.status::text, p.gateway, COALESCE(p.payment_type,'')
		FROM orders o JOIN payments p ON p.order_id = o.id WHERE o.id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, reprintOf, payStatus, gateway, ptype string
		if err := rows.Scan(&id, &reprintOf, &payStatus, &gateway, &ptype); err != nil {
			return err
		}
		o := &orders[index[id]]
		o.ReprintOf = reprintOf
		if payStatus == "paid" && o.Total > 0 {
			o.Payment.RefundMode = s.gatewayRefundMode(gateway, ptype)
		}
	}
	return rows.Err()
}
