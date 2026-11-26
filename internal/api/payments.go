package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/db"
	"karyakita/api/internal/mail"
	"karyakita/api/internal/midtrans"
)

// Pembayaran lewat gateway.
//
// Penyedia (ditentukan di cmd/api/main.go dari env):
//   midtrans — Midtrans Snap; pesanan HANYA menjadi lunas lewat webhook yang
//              ditandatangani / pengecekan status yang dilakukan server sendiri.
//   mock     — pembayaran tiruan (dev tanpa kunci Midtrans): pemilik pesanan
//              boleh memanggil {action:"pay"}.
//   none     — produksi tanpa kunci: pembayaran dinonaktifkan; hanya admin
//              yang bisa menandai lunas manual.
//
// Pesanan yang tidak dibayar dalam UnpaidOrderTTL dibatalkan otomatis (job di
// RunOrderJobs) dan voucher yang dipakainya dibebaskan kembali.

const (
	// UnpaidOrderTTL = batas bayar. Dipakai sebagai masa berlaku sesi Snap juga.
	UnpaidOrderTTL = 24 * time.Hour
	// grace sebelum job membatalkan, supaya notifikasi "expire" dari Midtrans
	// (yang biasanya tiba tepat di batas) diproses lebih dulu.
	cancelGrace = time.Hour
)

type PaymentsConfig struct {
	Provider        string // midtrans | mock | none
	Midtrans        *midtrans.Client
	EnabledPayments []string // opsional: batasi/urutkan kanal Snap, mis. qris,bca_va,bni_va
}

// SetPayments memilih penyedia pembayaran. Tanpa pemanggilan ini = mock.
func (s *Server) SetPayments(cfg PaymentsConfig) { s.payments = cfg }

func (s *Server) paymentProvider() string {
	if s.payments.Provider == "" {
		return "mock"
	}
	return s.payments.Provider
}

// GET /api/payments/config (publik) — dipakai frontend untuk memilih popup.
func (s *Server) getPaymentConfig(w http.ResponseWriter, r *http.Request) {
	cfg := map[string]any{"provider": s.paymentProvider()}
	if c := s.payments.Midtrans; s.paymentProvider() == "midtrans" && c != nil {
		cfg["clientKey"] = c.ClientKey
		cfg["snapUrl"] = c.SnapURL + "/snap/snap.js"
		cfg["production"] = c.Production
	}
	writeJSON(w, http.StatusOK, cfg)
}

// POST /api/orders/{id}/payment (pemilik/admin) → {token, redirectUrl, expiresAt}
// Membuat sesi Snap untuk pesanan, atau memakai ulang sesi yang masih berlaku.
func (s *Server) postPaymentSession(w http.ResponseWriter, r *http.Request) {
	if s.paymentProvider() != "midtrans" {
		errJSON(w, http.StatusBadRequest, "Pembayaran online belum diaktifkan di server ini")
		return
	}
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	if order.Status != "menunggu-pembayaran" || order.Payment.Status == "paid" {
		errJSON(w, http.StatusBadRequest, "Pesanan ini tidak sedang menunggu pembayaran")
		return
	}
	remaining := time.Until(time.UnixMilli(order.CreatedAt).Add(UnpaidOrderTTL))
	if remaining < 5*time.Minute {
		errJSON(w, http.StatusBadRequest, "Batas waktu pembayaran pesanan ini sudah habis — buat pesanan baru")
		return
	}

	ctx := r.Context()
	var status, gwID, token string
	var attempt int
	var tokenExp *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT status::text, COALESCE(gateway_order_id,''), attempt, COALESCE(snap_token,''), snap_expires_at
		FROM payments WHERE order_id = $1`, order.ID).Scan(&status, &gwID, &attempt, &token, &tokenExp); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// sesi lama masih berlaku → lanjutkan (pembeli mungkin menutup popup lalu membukanya lagi)
	if token != "" && status == "pending" && tokenExp != nil && time.Until(*tokenExp) > time.Minute {
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "expiresAt": tokenExp.UnixMilli()})
		return
	}
	// ada percobaan sebelumnya (ditolak / token habis) → nomor percobaan berikutnya
	if gwID != "" {
		attempt++
	}
	gwOrderID := fmt.Sprintf("%s-%d", order.ID, attempt)

	items := make([]midtrans.Item, 0, len(order.Items)+2)
	for i, it := range order.Items {
		items = append(items, midtrans.Item{ID: fmt.Sprintf("%s-%d", order.ID, i+1), Name: fmt.Sprintf("%s (%s, %s)", it.Title, it.Color, it.Size), Price: it.Price, Quantity: it.Qty})
	}
	if order.Shipping.Cost > 0 {
		items = append(items, midtrans.Item{ID: "ongkir", Name: "Ongkir " + order.Shipping.Courier, Price: order.Shipping.Cost, Quantity: 1})
	}
	if order.Discount > 0 {
		items = append(items, midtrans.Item{ID: "diskon", Name: "Diskon " + order.VoucherCode, Price: -order.Discount, Quantity: 1})
	}
	email := order.Customer.Email
	if email == "" {
		email = userFrom(r).Email
	}
	snap, err := s.payments.Midtrans.CreateSnap(ctx, midtrans.SnapRequest{
		OrderID: gwOrderID, GrossAmount: order.Total, Items: items,
		Customer: midtrans.Customer{Name: order.Customer.Name, Email: email, Phone: order.Customer.Phone,
			Address: order.Customer.Address, City: order.Customer.City, Postal: order.Customer.Postal},
		ExpiryMinutes:   int(remaining.Minutes()),
		EnabledPayments: s.payments.EnabledPayments,
		FinishURL:       s.auth.AppURL + "/order/" + order.ID,
	})
	if err != nil {
		log.Printf("midtrans CreateSnap %s: %v", gwOrderID, err)
		errJSON(w, http.StatusBadGateway, "Gagal menghubungi gateway pembayaran. Coba lagi sebentar lagi.")
		return
	}
	expires := time.Now().Add(remaining)
	if _, err := s.pool.Exec(ctx, `
		UPDATE payments SET gateway='midtrans', gateway_order_id=$2, attempt=$3, snap_token=$4, snap_expires_at=$5, status='pending'
		WHERE order_id=$1 AND status <> 'paid'`, order.ID, gwOrderID, attempt, snap.Token, expires); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": snap.Token, "redirectUrl": snap.RedirectURL, "expiresAt": expires.UnixMilli()})
}

// POST /api/orders/{id}/payment/sync (pemilik/admin) — server menanyakan
// status ke Midtrans sendiri dan menerapkannya. Dipakai setelah popup ditutup
// dan sebagai cadangan bila webhook terlambat / tidak sampai (mis. dev tanpa tunnel).
func (s *Server) postPaymentSync(w http.ResponseWriter, r *http.Request) {
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	if s.paymentProvider() == "midtrans" && order.Payment.Status != "paid" {
		var gwID string
		_ = s.pool.QueryRow(r.Context(), `SELECT COALESCE(gateway_order_id,'') FROM payments WHERE order_id=$1`, order.ID).Scan(&gwID)
		if gwID != "" {
			n, err := s.payments.Midtrans.Status(r.Context(), gwID)
			switch {
			case errors.Is(err, midtrans.ErrNotFound):
				// belum ada percobaan bayar di Midtrans — tidak ada yang perlu diterapkan
			case err != nil:
				log.Printf("midtrans Status %s: %v", gwID, err)
				errJSON(w, http.StatusBadGateway, "Gagal memeriksa status ke gateway pembayaran")
				return
			default:
				if _, err := s.applyGatewayStatus(r.Context(), n, "sync", true); err != nil {
					errJSON(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
		}
	}
	fresh, err := s.loadOrder(r.Context(), order.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": fresh})
}

// POST /api/payments/midtrans/notification (publik, dipanggil Midtrans).
// Tanda tangan diperiksa, lalu statusnya DIKONFIRMASI ulang ke Status API —
// isi request tidak pernah dipercaya begitu saja.
func (s *Server) postMidtransNotification(w http.ResponseWriter, r *http.Request) {
	if s.paymentProvider() != "midtrans" {
		errJSON(w, http.StatusNotFound, "Tidak aktif")
		return
	}
	var n midtrans.Notification
	if err := readJSON(r, &n); err != nil || n.OrderID == "" {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ctx := r.Context()
	if !s.payments.Midtrans.VerifySignature(&n) {
		s.logNotification(ctx, &n, "webhook", false, "ignored: tanda tangan tidak valid")
		errJSON(w, http.StatusForbidden, "Tanda tangan tidak valid")
		return
	}
	verified, err := s.payments.Midtrans.Status(ctx, n.OrderID)
	if errors.Is(err, midtrans.ErrNotFound) {
		s.logNotification(ctx, &n, "webhook", true, "ignored: transaksi tidak ada di Midtrans")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err != nil {
		log.Printf("midtrans Status %s: %v", n.OrderID, err)
		errJSON(w, http.StatusBadGateway, "Gagal memverifikasi ke Midtrans") // 5xx → Midtrans mengulang
		return
	}
	if _, err := s.applyGatewayStatus(ctx, verified, "webhook", true); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) logNotification(ctx context.Context, n *midtrans.Notification, source string, sigOK bool, applied string) {
	amount, _ := n.AmountRupiah()
	var orderID any
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT order_id FROM payments WHERE gateway_order_id=$1`, n.OrderID).Scan(&id); err == nil {
		orderID = id
	}
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO payment_notifications (order_id, gateway_order_id, source, transaction_status, payment_type, gross_amount, signature_ok, applied)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		orderID, n.OrderID, source, n.TransactionStatus, nilIfEmpty(n.PaymentType), amount, sigOK, applied)
}

// applyGatewayStatus menerapkan status yang sudah terverifikasi ke pesanan.
// Idempoten: notifikasi/sync yang berulang tidak menggandakan royalti.
func (s *Server) applyGatewayStatus(ctx context.Context, n *midtrans.Notification, source string, sigOK bool) (string, error) {
	var orderID string
	var amount int
	var payStatus string
	err := s.pool.QueryRow(ctx, `SELECT p.order_id, p.amount, p.status::text FROM payments p WHERE p.gateway_order_id=$1`, n.OrderID).
		Scan(&orderID, &amount, &payStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		s.logNotification(ctx, n, source, sigOK, "ignored: percobaan bayar tidak dikenal")
		return "ignored", nil
	}
	if err != nil {
		return "", err
	}
	if got, err := n.AmountRupiah(); err != nil || got != amount {
		log.Printf("midtrans %s: nominal %q != %d — diabaikan", n.OrderID, n.GrossAmount, amount)
		s.logNotification(ctx, n, source, sigOK, "ignored: nominal tidak cocok")
		return "ignored", nil
	}

	result := "ignored: " + n.TransactionStatus
	switch n.TransactionStatus {
	case "settlement", "capture":
		if n.TransactionStatus == "capture" && n.FraudStatus == "challenge" {
			result = "pending"
			break
		}
		if err := s.payOrderVia(ctx, orderID, n.Method(), n.PaymentType, "midtrans"); err != nil {
			return "", err
		}
		result = "paid"
	case "pending":
		// pembeli sudah memilih metode (mis. VA terbit) tapi belum membayar
		if _, err := s.pool.Exec(ctx,
			`UPDATE payments SET method=$2, payment_type=$3, status='pending' WHERE order_id=$1 AND status <> 'paid'`,
			orderID, n.Method(), nilIfEmpty(n.PaymentType)); err != nil {
			return "", err
		}
		result = "pending"
	case "deny", "cancel", "failure":
		// percobaan ini gagal; pesanan tetap menunggu dan bisa dicoba bayar lagi
		if _, err := s.pool.Exec(ctx, `UPDATE payments SET status='failed' WHERE order_id=$1 AND status <> 'paid'`, orderID); err != nil {
			return "", err
		}
		result = "failed"
	case "expire":
		if err := s.cancelUnpaidOrder(ctx, orderID, "Pembayaran kedaluwarsa di gateway"); err != nil {
			return "", err
		}
		result = "expired"
	}
	s.logNotification(ctx, n, source, sigOK, result)
	return result, nil
}

// payOrderVia: seperti payOrder, tapi mencatat gateway & tipe pembayaran.
func (s *Server) payOrderVia(ctx context.Context, orderID, method, paymentType, gateway string) error {
	if err := s.payOrder(ctx, orderID, truncate(method, 40)); err != nil {
		return err
	}
	var amount int
	var paidMethod string
	if err := s.pool.QueryRow(ctx, `SELECT amount, method FROM payments WHERE order_id=$1`, orderID).Scan(&amount, &paidMethod); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE payments SET gateway=$2, payment_type=COALESCE($3, payment_type), gateway_fee=$4 WHERE order_id=$1`,
		orderID, gateway, nilIfEmpty(paymentType), db.GatewayFee(paidMethod+" "+gateway, paymentType, amount))
	return err
}

// cancelUnpaidOrder membatalkan pesanan yang belum lunas (idempoten). Voucher
// yang dipakainya otomatis bebas kembali karena kuota tidak menghitung pesanan dibatalkan.
func (s *Server) cancelUnpaidOrder(ctx context.Context, orderID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE orders SET status='dibatalkan', cancelled_at=now()
		WHERE id=$1 AND status='menunggu-pembayaran'
		  AND EXISTS (SELECT 1 FROM payments WHERE order_id=$1 AND status <> 'paid')`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // sudah lunas / sudah dibatalkan / bukan menunggu pembayaran
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='expired' WHERE order_id=$1 AND status <> 'paid'`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, orderID, "Pesanan dibatalkan — "+reason); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.mailBuyer(ctx, orderID, mail.OrderCancelled)
	return nil
}

// CancelUnpaidOrders membatalkan pesanan yang melewati batas bayar (+ grace).
func (s *Server) CancelUnpaidOrders(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM orders
		WHERE status='menunggu-pembayaran' AND created_at <= now() - make_interval(secs => $1)`,
		(UnpaidOrderTTL + cancelGrace).Seconds())
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if err := s.cancelUnpaidOrder(ctx, id, "batas waktu pembayaran habis"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
