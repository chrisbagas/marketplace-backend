package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Penarikan saldo kreator (transfer manual oleh admin).
//
// Royalti dibukukan saat pesanan dibayar (lihat payOrder) dan berstatus
// "tertahan" sampai pesanan selesai — dikonfirmasi pembeli atau selesai
// otomatis. Saldo tidak disimpan di kolom mana pun; selalu dihitung ulang dari
// royalties + payouts (designerBalance), jadi tidak bisa melenceng.

// PayoutMinimum: nominal penarikan terkecil (Rp).
const PayoutMinimum = 50_000

type PayoutBalance struct {
	Pending   int `json:"pending"`   // royalti dari pesanan dibayar yang belum selesai
	Available int `json:"available"` // bisa ditarik sekarang
	InProcess int `json:"inProcess"` // pengajuan diminta/diproses
	Withdrawn int `json:"withdrawn"` // sudah ditransfer
	Released  int `json:"released"`  // total royalti dari pesanan selesai
}

type BankAccount struct {
	BankName string `json:"bankName"`
	Account  string `json:"account"`
	Holder   string `json:"holder"`
}

type Payout struct {
	ID           int64        `json:"id"`
	DesignerID   string       `json:"designerId"`
	DesignerName string       `json:"designerName,omitempty"`
	Amount       int          `json:"amount"`
	Status       string       `json:"status"` // diminta | diproses | dibayar | ditolak
	RequestedAt  int64        `json:"requestedAt"`
	ProcessedAt  *int64       `json:"processedAt,omitempty"`
	PaidAt       *int64       `json:"paidAt,omitempty"`
	Bank         *BankAccount `json:"bank,omitempty"`
	Reference    string       `json:"reference,omitempty"`
	Note         string       `json:"note,omitempty"`
}

func designerBalance(ctx context.Context, q querier, designerID string) (PayoutBalance, error) {
	var b PayoutBalance
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(sum(r.amount) FILTER (WHERE o.status = 'selesai'), 0)::int,
		       COALESCE(sum(r.amount) FILTER (WHERE o.status NOT IN ('selesai', 'dibatalkan')), 0)::int
		FROM royalties r
		JOIN order_items oi ON oi.id = r.order_item_id
		JOIN orders o       ON o.id = oi.order_id
		WHERE r.designer_id = $1`, designerID).Scan(&b.Released, &b.Pending); err != nil {
		return b, err
	}
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE status IN ('diminta', 'diproses')), 0)::int,
		       COALESCE(sum(amount) FILTER (WHERE status = 'dibayar'), 0)::int
		FROM payouts WHERE designer_id = $1`, designerID).Scan(&b.InProcess, &b.Withdrawn); err != nil {
		return b, err
	}
	b.Available = max(0, b.Released-b.InProcess-b.Withdrawn)
	return b, nil
}

const payoutCols = `
	p.id, p.designer_id, dr.name, p.amount, p.status::text,
	(extract(epoch FROM p.requested_at)*1000)::bigint,
	(extract(epoch FROM p.processed_at)*1000)::bigint,
	(extract(epoch FROM p.paid_at)*1000)::bigint,
	COALESCE(p.bank_name,''), COALESCE(p.bank_account,''), COALESCE(p.bank_holder,''),
	COALESCE(p.reference,''), COALESCE(p.note,'')`

func scanPayout(row pgx.Row) (Payout, error) {
	var p Payout
	var bank BankAccount
	err := row.Scan(&p.ID, &p.DesignerID, &p.DesignerName, &p.Amount, &p.Status,
		&p.RequestedAt, &p.ProcessedAt, &p.PaidAt, &bank.BankName, &bank.Account, &bank.Holder,
		&p.Reference, &p.Note)
	if bank.Account != "" {
		p.Bank = &bank
	}
	return p, err
}

func (s *Server) listPayouts(ctx context.Context, where string, args ...any) ([]Payout, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+payoutCols+`
		FROM payouts p JOIN designers dr ON dr.id = p.designer_id
		WHERE `+where+` ORDER BY p.requested_at DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Payout{}
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (s *Server) loadBank(ctx context.Context, q querier, designerID string) (*BankAccount, error) {
	var b BankAccount
	err := q.QueryRow(ctx, `
		SELECT COALESCE(bank_name,''), COALESCE(bank_account,''), COALESCE(bank_holder,'')
		FROM designers WHERE id = $1`, designerID).Scan(&b.BankName, &b.Account, &b.Holder)
	if err != nil || b.Account == "" {
		return nil, err
	}
	return &b, nil
}

// payoutDesigner: kreator yang sedang login; admin boleh melihat kreator lain lewat ?designer=.
func payoutDesigner(r *http.Request) string {
	u := userFrom(r)
	if u.Role == "admin" && r.URL.Query().Get("designer") != "" {
		return r.URL.Query().Get("designer")
	}
	if u.Designer != nil {
		return u.Designer.ID
	}
	return ""
}

// GET /api/designer/payouts — saldo, rekening, dan riwayat penarikan.
func (s *Server) getDesignerPayouts(w http.ResponseWriter, r *http.Request) {
	id := payoutDesigner(r)
	if id == "" {
		errJSON(w, http.StatusNotFound, "Akun ini belum punya profil toko")
		return
	}
	ctx := r.Context()
	bal, err := designerBalance(ctx, s.pool, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	bank, err := s.loadBank(ctx, s.pool, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := s.listPayouts(ctx, "p.designer_id = $1", id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balance": bal, "bank": bank, "payouts": list, "minimum": PayoutMinimum,
	})
}

var (
	bankAccountRe = regexp.MustCompile(`^[0-9]{6,20}$`)
	// rekening bank & e-wallet yang bisa ditransfer admin
	payoutBanks = []string{
		"BCA", "BRI", "BNI", "Mandiri", "BSI", "CIMB Niaga", "Permata", "BTN", "Danamon",
		"Bank Jago", "SeaBank", "Jenius (SMBC)", "GoPay", "OVO", "DANA", "ShopeePay",
	}
)

// PATCH /api/designer/bank {bankName, account, holder} — rekening tujuan penarikan.
func (s *Server) patchDesignerBank(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.Designer == nil {
		errJSON(w, http.StatusForbidden, "Hanya kreator yang punya rekening penarikan")
		return
	}
	var b BankAccount
	if err := readJSON(r, &b); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	b.BankName = strings.TrimSpace(b.BankName)
	b.Account = strings.Join(strings.FieldsFunc(b.Account, func(c rune) bool { return c == ' ' || c == '-' || c == '.' }), "")
	b.Holder = truncate(strings.Join(strings.Fields(b.Holder), " "), 80)
	switch {
	case !slices.Contains(payoutBanks, b.BankName):
		errJSON(w, http.StatusBadRequest, "Pilih bank atau e-wallet dari daftar")
		return
	case !bankAccountRe.MatchString(b.Account):
		errJSON(w, http.StatusBadRequest, "Nomor rekening harus 6–20 angka")
		return
	case len(b.Holder) < 3:
		errJSON(w, http.StatusBadRequest, "Isi nama pemilik rekening sesuai buku tabungan")
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`UPDATE designers SET bank_name=$2, bank_account=$3, bank_holder=$4 WHERE id=$1`,
		u.Designer.ID, b.BankName, b.Account, b.Holder); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bank": b})
}

// POST /api/designer/payouts {amount} — ajukan penarikan dari saldo tersedia.
func (s *Server) postDesignerPayout(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.Designer == nil {
		errJSON(w, http.StatusForbidden, "Hanya kreator yang bisa menarik saldo")
		return
	}
	if !u.EmailVerified {
		errJSON(w, http.StatusForbidden, "Verifikasi email dulu sebelum menarik saldo")
		return
	}
	var body struct {
		Amount int `json:"amount"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	p, err := s.requestPayout(r.Context(), u.Designer.ID, body.Amount)
	var ce checkoutError
	switch {
	case errors.As(err, &ce):
		errJSON(w, http.StatusBadRequest, ce.Error())
	case err != nil:
		errJSON(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"payout": p})
	}
}

func (s *Server) requestPayout(ctx context.Context, designerID string, amount int) (Payout, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payout{}, err
	}
	defer tx.Rollback(ctx)

	// kunci baris kreator: dua pengajuan bersamaan tidak bisa sama-sama lolos cek saldo
	var bank BankAccount
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(bank_name,''), COALESCE(bank_account,''), COALESCE(bank_holder,'')
		FROM designers WHERE id = $1 FOR UPDATE`, designerID).Scan(&bank.BankName, &bank.Account, &bank.Holder); err != nil {
		return Payout{}, err
	}
	if bank.Account == "" {
		return Payout{}, checkoutError("Isi rekening tujuan dulu sebelum menarik saldo")
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE designer_id=$1 AND status IN ('diminta','diproses')`, designerID).Scan(&open); err != nil {
		return Payout{}, err
	}
	if open > 0 {
		return Payout{}, checkoutError("Masih ada penarikan yang sedang diproses — tunggu sampai selesai")
	}
	bal, err := designerBalance(ctx, tx, designerID)
	if err != nil {
		return Payout{}, err
	}
	switch {
	case amount < PayoutMinimum:
		return Payout{}, checkoutError("Minimal penarikan " + rupiah(PayoutMinimum))
	case amount > bal.Available:
		return Payout{}, checkoutError("Melebihi saldo yang bisa ditarik (" + rupiah(bal.Available) + ")")
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO payouts (designer_id, amount, bank_name, bank_account, bank_holder)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		designerID, amount, bank.BankName, bank.Account, bank.Holder).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return Payout{}, checkoutError("Masih ada penarikan yang sedang diproses — tunggu sampai selesai")
		}
		return Payout{}, err
	}
	p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutCols+`
		FROM payouts p JOIN designers dr ON dr.id = p.designer_id WHERE p.id = $1`, id))
	if err != nil {
		return Payout{}, err
	}
	return p, tx.Commit(ctx)
}

// GET /api/payouts?status= — antrean penarikan untuk admin, plus saldo tiap kreator.
func (s *Server) getPayouts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	where, args := "true", []any{}
	switch status {
	case "":
	case "terbuka":
		where = "p.status IN ('diminta','diproses')"
	case "diminta", "diproses", "dibayar", "ditolak":
		where, args = "p.status::text = $1", []any{status}
	default:
		errJSON(w, http.StatusBadRequest, "Status tidak dikenal")
		return
	}
	list, err := s.listPayouts(r.Context(), where, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	balances := map[string]PayoutBalance{}
	for _, p := range list {
		if _, ok := balances[p.DesignerID]; ok {
			continue
		}
		b, err := designerBalance(r.Context(), s.pool, p.DesignerID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		balances[p.DesignerID] = b
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": list, "balances": balances})
}

// PATCH /api/payouts/{id} — admin memproses pengajuan:
//
//	{action:"process"}                — diminta → diproses (transfer sedang dikerjakan)
//	{action:"paid", reference:"..."}  — diminta/diproses → dibayar (wajib no. referensi transfer)
//	{action:"reject", note:"..."}     — diminta/diproses → ditolak (dana kembali ke saldo)
func (s *Server) patchPayout(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusNotFound, "Penarikan tidak ditemukan")
		return
	}
	var body struct {
		Action    string `json:"action"`
		Reference string `json:"reference"`
		Note      string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	ref := truncate(strings.TrimSpace(body.Reference), 60)
	note := truncate(strings.TrimSpace(body.Note), 300)

	var sql, msg string
	args := []any{id}
	switch body.Action {
	case "process":
		sql = `UPDATE payouts SET status='diproses', processed_at=now() WHERE id=$1 AND status='diminta'`
		msg = "Hanya pengajuan baru yang bisa diproses"
	case "paid":
		if ref == "" {
			errJSON(w, http.StatusBadRequest, "Isi nomor referensi transfer")
			return
		}
		sql = `UPDATE payouts SET status='dibayar', reference=$2, note=NULLIF($3,''), paid_at=now(), processed_at=now()
		       WHERE id=$1 AND status IN ('diminta','diproses')`
		args = append(args, ref, note)
		msg = "Penarikan ini sudah selesai diproses"
	case "reject":
		if note == "" {
			errJSON(w, http.StatusBadRequest, "Tulis alasan penolakan untuk kreator")
			return
		}
		sql = `UPDATE payouts SET status='ditolak', note=$2, processed_at=now()
		       WHERE id=$1 AND status IN ('diminta','diproses')`
		args = append(args, note)
		msg = "Penarikan ini sudah selesai diproses"
	default:
		errJSON(w, http.StatusBadRequest, "Aksi tidak dikenal")
		return
	}
	tag, err := s.pool.Exec(r.Context(), sql, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		s.pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM payouts WHERE id=$1)`, id).Scan(&exists)
		if !exists {
			errJSON(w, http.StatusNotFound, "Penarikan tidak ditemukan")
		} else {
			errJSON(w, http.StatusConflict, msg)
		}
		return
	}
	p, err := scanPayout(s.pool.QueryRow(r.Context(), `SELECT `+payoutCols+`
		FROM payouts p JOIN designers dr ON dr.id = p.designer_id WHERE p.id = $1`, id))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payout": p})
}
