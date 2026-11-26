package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/db"
)

// Komplain pembeli.
//
// Selama pesanan berstatus `tiba` (sebelum selesai otomatis), pembeli bisa
// mengajukan SATU komplain: alasan + keterangan + maks 3 foto. Komplain
// terbuka menahan selesai-otomatis (AutoCompleteOrders) dan menggantikan tombol
// "Pesanan diterima". Admin menyelesaikannya dengan:
//
//	reprint — pesanan baru bertotal 0 yang tertaut ke pesanan asal; tagihan produsen
//	          (produksi + ongkir) dibukukan, tanpa royalti & tanpa biaya gateway;
//	          berjalan lewat produksi/dikirim seperti pesanan biasa
//	refund  — pengembalian dana penuh/sebagian (royalti dibalik sebanding);
//	          tagihan produsen pesanan asal TIDAK dibalik: barang sudah dicetak
//	          dan dikirim, jadi biayanya tetap ditanggung platform
//	reject  — ditolak dengan catatan; selesai-otomatis jalan lagi dihitung dari waktu keputusan

const (
	maxComplaintPhotos  = 3
	complaintStatusOpen = "terbuka"
)

// ComplaintReasons: nilai reason yang diterima (cermin di frontend lib/orders.ts).
var ComplaintReasons = map[string]string{
	"cacat-cetak":  "Cacat cetak",
	"salah-produk": "Produk / ukuran / warna salah",
	"rusak":        "Rusak saat pengiriman",
	"tidak-sesuai": "Tidak sesuai pesanan atau gambar",
	"lainnya":      "Lainnya",
}

type Complaint struct {
	ID             int64    `json:"id"`
	OrderID        string   `json:"orderId"`
	Reason         string   `json:"reason"`
	Description    string   `json:"description"`
	Photos         []string `json:"photos"`
	Status         string   `json:"status"` // terbuka | dicetak-ulang | direfund | ditolak
	ResolutionNote string   `json:"resolutionNote,omitempty"`
	RefundAmount   int      `json:"refundAmount,omitempty"`
	ReprintOrderID string   `json:"reprintOrderId,omitempty"`
	CreatedAt      int64    `json:"createdAt"`
	ResolvedAt     int64    `json:"resolvedAt,omitempty"`
	// hanya di daftar admin
	Buyer string `json:"buyer,omitempty"`
	Total int    `json:"total,omitempty"`
}

const complaintCols = `
	c.id, c.order_id, c.reason, c.description, c.photos, c.status, COALESCE(c.resolution_note,''),
	COALESCE(rf.amount, 0), COALESCE(c.reprint_order_id,''),
	(extract(epoch FROM c.created_at)*1000)::bigint, COALESCE((extract(epoch FROM c.resolved_at)*1000)::bigint, 0)`

const complaintFrom = ` FROM complaints c LEFT JOIN refunds rf ON rf.id = c.refund_id `

func scanComplaint(row pgx.Row, extra ...any) (Complaint, error) {
	var c Complaint
	dest := append([]any{&c.ID, &c.OrderID, &c.Reason, &c.Description, &c.Photos, &c.Status, &c.ResolutionNote,
		&c.RefundAmount, &c.ReprintOrderID, &c.CreatedAt, &c.ResolvedAt}, extra...)
	err := row.Scan(dest...)
	if c.Photos == nil {
		c.Photos = []string{}
	}
	return c, err
}

// validPhotoURL: foto lewat helper upload (/uploads/..., atau URL https bila
// penyimpanan dipindah ke object storage). Skema lain (javascript:, data:) ditolak.
func validPhotoURL(u string) bool {
	if len(u) > 500 || strings.ContainsAny(u, " \t\r\n\"'<>\\") {
		return false
	}
	return strings.HasPrefix(u, "/uploads/") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// POST /api/orders/{id}/complaint (pembeli) {reason, description, photos[]}
func (s *Server) postComplaint(w http.ResponseWriter, r *http.Request) {
	order, ok := s.ownedOrder(w, r)
	if !ok {
		return
	}
	u := userFrom(r)
	if order.UserID != u.ID {
		errJSON(w, http.StatusForbidden, "Hanya pembeli yang bisa mengajukan komplain")
		return
	}
	var body struct {
		Reason      string   `json:"reason"`
		Description string   `json:"description"`
		Photos      []string `json:"photos"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	desc := truncate(strings.TrimSpace(body.Description), 1000)
	photos := make([]string, 0, len(body.Photos))
	for _, p := range body.Photos {
		if p = strings.TrimSpace(p); p != "" {
			photos = append(photos, p)
		}
	}
	switch {
	case ComplaintReasons[body.Reason] == "":
		errJSON(w, http.StatusBadRequest, "Pilih alasan komplain")
		return
	case len([]rune(desc)) < 10:
		errJSON(w, http.StatusBadRequest, "Ceritakan masalahnya (minimal 10 karakter)")
		return
	case len(photos) > maxComplaintPhotos:
		errJSON(w, http.StatusBadRequest, fmt.Sprintf("Maksimal %d foto", maxComplaintPhotos))
		return
	case slices.ContainsFunc(photos, func(p string) bool { return !validPhotoURL(p) }):
		errJSON(w, http.StatusBadRequest, "Foto tidak valid — unggah ulang fotonya")
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	// kunci pesanan: selesai-otomatis dan komplain tidak bisa saling melewati
	var status string
	if err := tx.QueryRow(ctx, `SELECT status::text FROM orders WHERE id=$1 AND user_id=$2 FOR UPDATE`, order.ID, u.ID).Scan(&status); err != nil {
		errJSON(w, http.StatusNotFound, "Pesanan tidak ditemukan")
		return
	}
	if status != "tiba" {
		errJSON(w, http.StatusBadRequest, "Komplain hanya bisa diajukan setelah paket tiba dan sebelum pesanan selesai")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO complaints (order_id, user_id, reason, description, photos) VALUES ($1,$2,$3,$4,$5)`,
		order.ID, u.ID, body.Reason, desc, photos); err != nil {
		if isUniqueViolation(err) {
			errJSON(w, http.StatusConflict, "Komplain untuk pesanan ini sudah pernah diajukan")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, order.ID,
		"Komplain diajukan pembeli: "+ComplaintReasons[body.Reason]+" — pesanan tidak selesai otomatis sampai komplain diputuskan"); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
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
	writeJSON(w, http.StatusCreated, map[string]any{"order": fresh})
}

// GET /api/complaints?status=terbuka|dicetak-ulang|direfund|ditolak (admin)
func (s *Server) getComplaints(w http.ResponseWriter, r *http.Request) {
	where, args := "true", []any{}
	if st := r.URL.Query().Get("status"); st != "" {
		where, args = "c.status = $1", []any{st}
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+complaintCols+`, o.cust_name, o.total`+complaintFrom+`
		JOIN orders o ON o.id = c.order_id WHERE `+where+` ORDER BY (c.status = 'terbuka') DESC, c.created_at DESC LIMIT 200`, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	list := []Complaint{}
	for rows.Next() {
		var buyer string
		var total int
		c, err := scanComplaint(rows, &buyer, &total)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.Buyer, c.Total = buyer, total
		list = append(list, c)
	}
	if err := rows.Err(); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"complaints": list})
}

// PATCH /api/complaints/{id} (admin)
//
//	{action:"reprint", note?}
//	{action:"refund", amount, method?:"gateway"|"manual", reference?, note?}
//	{action:"reject", note}
func (s *Server) patchComplaint(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusNotFound, "Komplain tidak ditemukan")
		return
	}
	var body struct {
		Action    string `json:"action"`
		Amount    int    `json:"amount"`
		Method    string `json:"method"`
		Reference string `json:"reference"`
		Note      string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	note := truncate(strings.TrimSpace(body.Note), 500)
	switch body.Action {
	case "reject":
		if note == "" {
			errJSON(w, http.StatusBadRequest, "Tulis alasan penolakan untuk pembeli")
			return
		}
	case "refund", "reprint":
	default:
		errJSON(w, http.StatusBadRequest, "Aksi tidak dikenal")
		return
	}

	ctx := r.Context()
	adminID := userFrom(r).ID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var orderID, status, reason string
	if err := tx.QueryRow(ctx, `SELECT order_id, status, reason FROM complaints WHERE id=$1 FOR UPDATE`, id).Scan(&orderID, &status, &reason); err != nil {
		errJSON(w, http.StatusNotFound, "Komplain tidak ditemukan")
		return
	}
	if status != complaintStatusOpen {
		errJSON(w, http.StatusConflict, "Komplain ini sudah diputuskan")
		return
	}
	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT status::text FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&orderStatus); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if orderStatus != "tiba" {
		errJSON(w, http.StatusConflict, "Pesanan tidak lagi berstatus tiba")
		return
	}

	var refundID *int64
	var reprintID *string
	var newStatus, event string
	switch body.Action {
	case "reject":
		newStatus, event = "ditolak", "Komplain ditolak admin — "+note
	case "refund":
		method := body.Method
		if method == "" {
			method = "manual"
			var gw, ptype string
			_ = tx.QueryRow(ctx, `SELECT gateway, COALESCE(payment_type,'') FROM payments WHERE order_id=$1`, orderID).Scan(&gw, &ptype)
			if s.gatewayRefundMode(gw, ptype) != "" {
				method = "gateway"
			}
		}
		why := "Komplain: " + ComplaintReasons[reason]
		if note != "" {
			why += " — " + note
		}
		rid, err := s.issueRefund(ctx, tx, refundInput{
			OrderID: orderID, Amount: body.Amount, Method: method, Reference: body.Reference,
			Reason: truncate(why, 200), AdminID: adminID,
		})
		if writeRefundError(w, err) {
			return
		}
		refundID = &rid
		newStatus, event = "direfund", "Komplain diselesaikan dengan pengembalian dana "+rupiah(body.Amount)
	case "reprint":
		nid, err := s.createReprint(ctx, tx, orderID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		reprintID = &nid
		newStatus, event = "dicetak-ulang", "Komplain diselesaikan dengan cetak ulang — pesanan pengganti "+nid
	}
	if note != "" && body.Action != "reject" {
		event += " · " + note
	}
	if _, err := tx.Exec(ctx, `
		UPDATE complaints SET status=$2, resolution_note=NULLIF($3,''), refund_id=$4, reprint_order_id=$5,
		       resolved_at=now(), resolved_by=$6
		WHERE id=$1`, id, newStatus, note, refundID, reprintID, adminID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, orderID, event); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := scanComplaint(s.pool.QueryRow(ctx, `SELECT `+complaintCols+complaintFrom+` WHERE c.id = $1`, id))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"complaint": c})
}

// createReprint membuat pesanan pengganti gratis dari pesanan asal: alamat &
// kurir sama, item disalin apa adanya (termasuk snapshot tarif produsen), total 0,
// langsung berstatus dibayar supaya admin menjalankannya lewat produksi → dikirim.
// Hanya tagihan produsen yang dibukukan (db.BookReprintOrder).
func (s *Server) createReprint(ctx context.Context, tx pgx.Tx, origID string) (string, error) {
	id, err := newOrderID(ctx, tx)
	if err != nil {
		return "", err
	}
	var shipping int
	if err := tx.QueryRow(ctx, `SELECT shipping_cost FROM orders WHERE id = $1`, origID).Scan(&shipping); err != nil {
		return "", err
	}
	// copy the delivery data with every column except the ones set below, so
	// columns added by later migrations (e.g. shipping destination) follow along
	ocols, err := tableColumns(ctx, tx, "orders",
		"id", "status", "created_at", "subtotal", "shipping_cost", "discount", "voucher_code", "total", "notes",
		"shipped_courier", "tracking_number", "shipped_at", "delivered_at", "completed_at", "cancelled_at", "reprint_of")
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO orders (id, status, subtotal, shipping_cost, discount, total, notes, reprint_of, `+ocols+`)
		SELECT $2, 'dibayar', 0, 0, 0, 0, 'Cetak ulang dari ' || id, id, `+ocols+`
		FROM orders WHERE id = $1`, origID, id); err != nil {
		return "", err
	}
	// same for the items: every column except id/order_id (print spec, rate snapshots, ...)
	icols, err := tableColumns(ctx, tx, "order_items", "id", "order_id")
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, `+icols+`) SELECT $2, `+icols+` FROM order_items WHERE order_id = $1 ORDER BY id`,
		origID, id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payments (order_id, method, status, ref, amount, paid_at, gateway, gateway_fee)
		VALUES ($1, 'Cetak ulang', 'paid', $2, 0, now(), 'manual', 0)`, id, "REPRINT-"+origID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label) VALUES ($1,$2)`, id,
		"Pesanan cetak ulang dari "+origID+" (komplain) — tanpa biaya, siap masuk produksi"); err != nil {
		return "", err
	}
	if err := db.BookReprintOrder(ctx, tx, id, shipping); err != nil {
		return "", err
	}
	return id, nil
}

// tableColumns lists a table's columns (quoted, comma separated) minus the excluded ones.
func tableColumns(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, table string, exclude ...string) (string, error) {
	rows, err := q.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND NOT (column_name = ANY($2))
		ORDER BY ordinal_position`, table, exclude)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		cols = append(cols, pgx.Identifier{c}.Sanitize())
	}
	return strings.Join(cols, ", "), rows.Err()
}
