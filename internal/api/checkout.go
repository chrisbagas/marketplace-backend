package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Checkout: harga, ongkir, dan voucher SELALU dihitung di server. Browser
// hanya mengirim "apa" yang dibeli (produk, warna, ukuran, qty), bukan harga.
// quoteOrder dipakai oleh POST /api/checkout/quote (ringkasan di halaman
// checkout) dan POST /api/orders (pembuatan pesanan), jadi angka yang dilihat
// pembeli = angka yang ditagih.

const (
	customFee    = 25_000 // biaya cetak desain custom di atas harga dasar produk
	maxLineQty   = 99
	maxCartLines = 30
)

type Courier struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	ETA   string `json:"eta"`
	Cost  int    `json:"cost"`
}

// tarif flat (prototipe) — nanti diganti API ongkir (RajaOngkir/Biteship)
var couriers = []Courier{
	{"SiCepat REG", "SiCepat REG", "3–4 hari", 18_000},
	{"JNE REG", "JNE REG", "3–5 hari", 20_000},
	{"AnterAja Next Day", "AnterAja Next Day", "1–2 hari", 34_000},
}

func courierByID(id string) (Courier, bool) {
	for _, c := range couriers {
		if c.ID == id {
			return c, true
		}
	}
	return Courier{}, false
}

type QuoteLine struct {
	ProductID string `json:"productId"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Color     string `json:"color"`
	Size      string `json:"size"`
	Qty       int    `json:"qty"`
	Price     int    `json:"price"` // harga satuan dari database
	LineTotal int    `json:"lineTotal"`
	DesignURI string `json:"designUri,omitempty"`
	listingID string // kosong = desain custom (tanpa royalti)
}

type VoucherInfo struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

type Quote struct {
	Items        []QuoteLine  `json:"items"`
	Subtotal     int          `json:"subtotal"`
	Courier      Courier      `json:"courier"`
	Shipping     int          `json:"shipping"`
	Discount     int          `json:"discount"`
	Total        int          `json:"total"`
	Voucher      *VoucherInfo `json:"voucher,omitempty"`
	VoucherError string       `json:"voucherError,omitempty"`
	Couriers     []Courier    `json:"couriers"`
}

// checkoutError = kesalahan input pembeli → 400 dengan pesan yang bisa ditampilkan.
type checkoutError string

func (e checkoutError) Error() string { return string(e) }

// querier: *pgxpool.Pool dan pgx.Tx sama-sama memenuhi ini.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// quoteOrder menghitung ulang seluruh keranjang. lockVoucher=true dipakai saat
// membuat pesanan (di dalam transaksi) supaya kuota voucher tidak terlampaui
// oleh dua checkout bersamaan.
func quoteOrder(ctx context.Context, q querier, user *User, lines []CartLine, courierID, voucherCode string, lockVoucher bool) (*Quote, error) {
	if len(lines) == 0 {
		return nil, checkoutError("Keranjang kosong")
	}
	if len(lines) > maxCartLines {
		return nil, checkoutError(fmt.Sprintf("Maksimal %d jenis barang per pesanan", maxCartLines))
	}

	// gabungkan baris kembar (produk + warna + ukuran sama) — ukuran berbeda tetap terpisah
	type key struct{ product, color, size string }
	merged := []CartLine{}
	pos := map[key]int{}
	for _, l := range lines {
		if l.Qty < 1 || l.Qty > maxLineQty {
			return nil, checkoutError(fmt.Sprintf("Jumlah per barang 1–%d", maxLineQty))
		}
		k := key{l.ProductID, l.Color, l.Size}
		if i, ok := pos[k]; ok {
			merged[i].Qty += l.Qty
			if merged[i].Qty > maxLineQty {
				return nil, checkoutError(fmt.Sprintf("Jumlah per barang 1–%d", maxLineQty))
			}
			continue
		}
		pos[k] = len(merged)
		merged = append(merged, l)
	}

	quote := &Quote{Items: []QuoteLine{}, Couriers: couriers}
	for _, l := range merged {
		var line QuoteLine
		var err error
		if strings.HasPrefix(l.ProductID, "custom-") {
			line, err = quoteCustomLine(ctx, q, l)
		} else {
			line, err = quoteCatalogLine(ctx, q, l)
		}
		if err != nil {
			return nil, err
		}
		line.LineTotal = line.Price * line.Qty
		quote.Subtotal += line.LineTotal
		quote.Items = append(quote.Items, line)
	}

	c, ok := courierByID(courierID)
	if !ok {
		c = couriers[0]
	}
	quote.Courier, quote.Shipping = c, c.Cost

	if code := normalizeVoucher(voucherCode); code != "" {
		discount, info, err := evalVoucher(ctx, q, user, code, quote.Subtotal, quote.Shipping, lockVoucher)
		if err != nil {
			var ce checkoutError
			if !errors.As(err, &ce) {
				return nil, err
			}
			quote.VoucherError = ce.Error()
		} else {
			quote.Discount, quote.Voucher = discount, info
		}
	}
	quote.Total = quote.Subtotal + quote.Shipping - quote.Discount
	return quote, nil
}

func quoteCatalogLine(ctx context.Context, q querier, l CartLine) (QuoteLine, error) {
	p, err := scanProduct(q.QueryRow(ctx, productQuery+` AND l.id = $1`, l.ProductID))
	if errors.Is(err, pgx.ErrNoRows) {
		return QuoteLine{}, checkoutError("Produk tidak ditemukan: " + l.Title)
	}
	if err != nil {
		return QuoteLine{}, err
	}
	if !p.Active {
		return QuoteLine{}, checkoutError(fmt.Sprintf("%s sudah tidak dijual — hapus dari keranjang", p.Title))
	}
	if !slices.Contains(p.ColorIds, l.Color) {
		return QuoteLine{}, checkoutError(fmt.Sprintf("Warna %q tidak tersedia untuk %s", l.Color, p.Title))
	}
	if !slices.Contains(p.Sizes, l.Size) {
		return QuoteLine{}, checkoutError(fmt.Sprintf("Ukuran %q tidak tersedia untuk %s", l.Size, p.Title))
	}
	return QuoteLine{
		ProductID: p.ID, Title: p.Title, Type: p.Type, Color: l.Color, Size: l.Size,
		Qty: l.Qty, Price: p.Price, DesignURI: p.DesignURI, listingID: p.ID,
	}, nil
}

// Desain custom pembeli: tidak ada listing, harga = harga dasar + biaya custom.
func quoteCustomLine(ctx context.Context, q querier, l CartLine) (QuoteLine, error) {
	var label string
	var base int
	var colorOK, sizeOK bool
	err := q.QueryRow(ctx, `
		SELECT pt.label, pt.base_price,
		       EXISTS (SELECT 1 FROM product_type_colors WHERE product_type_id = pt.id AND color_id = $2),
		       EXISTS (SELECT 1 FROM product_type_sizes  WHERE product_type_id = pt.id AND size = $3)
		FROM product_types pt WHERE pt.id = $1`, l.Type, l.Color, l.Size).Scan(&label, &base, &colorOK, &sizeOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuoteLine{}, checkoutError("Jenis produk custom tidak valid")
	}
	if err != nil {
		return QuoteLine{}, err
	}
	if !colorOK || !sizeOK {
		return QuoteLine{}, checkoutError("Warna atau ukuran produk custom tidak tersedia")
	}
	uri := l.DesignURI
	if !(strings.HasPrefix(uri, "/uploads/") || strings.HasPrefix(uri, "data:image/")) || len(uri) > 1_500_000 {
		return QuoteLine{}, checkoutError("Gambar desain custom tidak valid — unggah ulang dari Studio")
	}
	title := strings.TrimSpace(l.Title)
	if title == "" {
		title = "Custom " + label
	}
	return QuoteLine{
		ProductID: l.ProductID, Title: truncate(title, 100), Type: l.Type, Color: l.Color, Size: l.Size,
		Qty: l.Qty, Price: base + customFee, DesignURI: uri,
	}, nil
}

var voucherCodeRe = regexp.MustCompile(`^[A-Z0-9-]{3,20}$`)

func normalizeVoucher(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// evalVoucher memeriksa syarat voucher dan menghitung potongannya.
// Error bertipe checkoutError = voucher tidak bisa dipakai (pesan untuk pembeli).
func evalVoucher(ctx context.Context, q querier, user *User, code string, subtotal, shipping int, lock bool) (int, *VoucherInfo, error) {
	var v struct {
		desc, kind                  string
		value, minSubtotal, perUser int
		maxDiscount, usageLimit     *int
		startsAt, endsAt            *time.Time
		active                      bool
	}
	sql := `SELECT description, kind::text, value, min_subtotal, max_discount, starts_at, ends_at,
	               usage_limit, per_user_limit, active
	        FROM vouchers WHERE code = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, sql, code).Scan(&v.desc, &v.kind, &v.value, &v.minSubtotal, &v.maxDiscount,
		&v.startsAt, &v.endsAt, &v.usageLimit, &v.perUser, &v.active)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, checkoutError("Kode voucher tidak ditemukan")
	}
	if err != nil {
		return 0, nil, err
	}
	now := time.Now()
	switch {
	case !v.active:
		return 0, nil, checkoutError("Voucher ini sudah tidak aktif")
	case v.startsAt != nil && now.Before(*v.startsAt):
		return 0, nil, checkoutError("Voucher ini belum bisa dipakai")
	case v.endsAt != nil && now.After(*v.endsAt):
		return 0, nil, checkoutError("Voucher ini sudah kedaluwarsa")
	case subtotal < v.minSubtotal:
		return 0, nil, checkoutError(fmt.Sprintf("Minimal belanja %s untuk voucher ini", rupiah(v.minSubtotal)))
	}

	// kuota dihitung dari pesanan yang sudah dibuat (dibayar atau belum)
	if v.usageLimit != nil {
		var used int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM orders WHERE voucher_code = $1`, code).Scan(&used); err != nil {
			return 0, nil, err
		}
		if used >= *v.usageLimit {
			return 0, nil, checkoutError("Kuota voucher ini sudah habis")
		}
	}
	if user != nil {
		var mine int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM orders WHERE voucher_code = $1 AND user_id = $2`, code, user.ID).Scan(&mine); err != nil {
			return 0, nil, err
		}
		if mine >= v.perUser {
			return 0, nil, checkoutError("Kamu sudah memakai voucher ini sebanyak batas yang diizinkan")
		}
	}

	var discount int
	switch v.kind {
	case "percent":
		discount = subtotal * v.value / 100
		if v.maxDiscount != nil && discount > *v.maxDiscount {
			discount = *v.maxDiscount
		}
	case "fixed":
		discount = min(v.value, subtotal)
	case "shipping":
		discount = min(v.value, shipping)
	}
	return discount, &VoucherInfo{Code: code, Description: v.desc, Kind: v.kind}, nil
}

func rupiah(n int) string {
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	return "Rp" + b.String()
}

// POST /api/checkout/quote {items, courier, voucher} → ringkasan harga dari server
func (s *Server) postQuote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items   []CartLine `json:"items"`
		Courier string     `json:"courier"`
		Voucher string     `json:"voucher"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	quote, err := quoteOrder(r.Context(), s.pool, userFrom(r), body.Items, body.Courier, body.Voucher, false)
	var ce checkoutError
	if errors.As(err, &ce) {
		errJSON(w, http.StatusBadRequest, ce.Error())
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quote": quote})
}

// ---- admin: kelola voucher ------------------------------------------------

type Voucher struct {
	Code         string `json:"code"`
	Description  string `json:"description"`
	Kind         string `json:"kind"`
	Value        int    `json:"value"`
	MinSubtotal  int    `json:"minSubtotal"`
	MaxDiscount  *int   `json:"maxDiscount,omitempty"`
	StartsAt     *int64 `json:"startsAt,omitempty"`
	EndsAt       *int64 `json:"endsAt,omitempty"`
	UsageLimit   *int   `json:"usageLimit,omitempty"`
	PerUserLimit int    `json:"perUserLimit"`
	Active       bool   `json:"active"`
	Used         int    `json:"used"`
	DiscountSum  int    `json:"discountSum"`
}

const voucherCols = `
	SELECT v.code, v.description, v.kind::text, v.value, v.min_subtotal, v.max_discount,
	       (extract(epoch FROM v.starts_at)*1000)::bigint, (extract(epoch FROM v.ends_at)*1000)::bigint,
	       v.usage_limit, v.per_user_limit, v.active,
	       (SELECT count(*) FROM orders o WHERE o.voucher_code = v.code),
	       (SELECT COALESCE(sum(discount), 0) FROM orders o WHERE o.voucher_code = v.code)
	FROM vouchers v`

func scanVoucher(row pgx.Row) (Voucher, error) {
	var v Voucher
	err := row.Scan(&v.Code, &v.Description, &v.Kind, &v.Value, &v.MinSubtotal, &v.MaxDiscount,
		&v.StartsAt, &v.EndsAt, &v.UsageLimit, &v.PerUserLimit, &v.Active, &v.Used, &v.DiscountSum)
	return v, err
}

// GET /api/vouchers (admin)
func (s *Server) getVouchers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), voucherCols+` ORDER BY v.active DESC, v.created_at DESC`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	list := []Voucher{}
	for rows.Next() {
		v, err := scanVoucher(rows)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vouchers": list})
}

// POST /api/vouchers (admin) — buat voucher baru
func (s *Server) postVoucher(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code         string `json:"code"`
		Description  string `json:"description"`
		Kind         string `json:"kind"`
		Value        int    `json:"value"`
		MinSubtotal  int    `json:"minSubtotal"`
		MaxDiscount  *int   `json:"maxDiscount"`
		EndsAt       *int64 `json:"endsAt"` // epoch ms
		UsageLimit   *int   `json:"usageLimit"`
		PerUserLimit int    `json:"perUserLimit"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	code := normalizeVoucher(body.Code)
	switch {
	case !voucherCodeRe.MatchString(code):
		errJSON(w, http.StatusBadRequest, "Kode 3–20 karakter: huruf, angka, atau tanda hubung")
		return
	case body.Kind != "percent" && body.Kind != "fixed" && body.Kind != "shipping":
		errJSON(w, http.StatusBadRequest, "Jenis voucher tidak valid")
		return
	case body.Value <= 0 || (body.Kind == "percent" && body.Value > 100):
		errJSON(w, http.StatusBadRequest, "Nilai voucher tidak valid (persen 1–100, atau nominal Rupiah)")
		return
	case body.MinSubtotal < 0 || (body.MaxDiscount != nil && *body.MaxDiscount <= 0) || (body.UsageLimit != nil && *body.UsageLimit <= 0):
		errJSON(w, http.StatusBadRequest, "Batas voucher tidak valid")
		return
	}
	if body.PerUserLimit <= 0 {
		body.PerUserLimit = 1
	}
	var endsAt any
	if body.EndsAt != nil {
		endsAt = time.UnixMilli(*body.EndsAt)
	}
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO vouchers (code, description, kind, value, min_subtotal, max_discount, ends_at, usage_limit, per_user_limit)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		code, truncate(strings.TrimSpace(body.Description), 200), body.Kind, body.Value, body.MinSubtotal,
		body.MaxDiscount, endsAt, body.UsageLimit, body.PerUserLimit); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			errJSON(w, http.StatusConflict, "Kode voucher sudah ada")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	v, err := scanVoucher(s.pool.QueryRow(r.Context(), voucherCols+` WHERE v.code = $1`, code))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"voucher": v})
}

// PATCH /api/vouchers/{code} {active} (admin)
func (s *Server) patchVoucher(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active *bool `json:"active"`
	}
	if err := readJSON(r, &body); err != nil || body.Active == nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	code := normalizeVoucher(r.PathValue("code"))
	tag, err := s.pool.Exec(r.Context(), `UPDATE vouchers SET active = $2 WHERE code = $1`, code, *body.Active)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		errJSON(w, http.StatusNotFound, "Voucher tidak ditemukan")
		return
	}
	v, err := scanVoucher(s.pool.QueryRow(r.Context(), voucherCols+` WHERE v.code = $1`, code))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voucher": v})
}
