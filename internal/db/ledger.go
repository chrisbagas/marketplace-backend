package db

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Pembukuan pesanan yang lunas: siapa berhak atas berapa (lihat migrasi 014).
// Dipakai payOrder (pembayaran sungguhan) dan seeder, supaya keduanya sama.

// PlaceholderProductionCost: biaya produksi contoh per jenis produk sampai ada
// penawaran vendor sungguhan. Sama dengan angka di migrasi 014.
var PlaceholderProductionCost = map[string]int{"kaos": 55000, "hoodie": 150000, "mug": 35000, "totebag": 45000}

// BookPaidOrder membukukan royalti kreator dan tagihan produsen untuk satu
// pesanan. Idempoten: aman dipanggil ulang untuk pesanan yang sama.
func BookPaidOrder(ctx context.Context, tx pgx.Tx, orderID string) error {
	// kreator: selisih harga jual di atas harga dasar (desain custom tanpa listing → tanpa royalti)
	if _, err := tx.Exec(ctx, `
		INSERT INTO royalties (designer_id, order_item_id, amount)
		SELECT d.designer_id, oi.id, (GREATEST(0, oi.unit_price - COALESCE(oi.base_price, pt.base_price)) * oi.qty)::int
		FROM order_items oi
		JOIN listings l       ON l.id = oi.listing_id
		JOIN designs d        ON d.id = l.design_id
		JOIN product_types pt ON pt.id = oi.type
		WHERE oi.order_id = $1
		ON CONFLICT (order_item_id) DO NOTHING`, orderID); err != nil {
		return err
	}
	// produsen: biaya produksi per item
	if _, err := tx.Exec(ctx, `
		INSERT INTO producer_charges (producer_id, order_id, order_item_id, kind, amount)
		SELECT COALESCE(oi.producer_id, pt.producer_id), oi.order_id, oi.id, 'produksi',
		       COALESCE(oi.production_cost, pt.production_cost) * oi.qty
		FROM order_items oi
		JOIN product_types pt ON pt.id = oi.type
		WHERE oi.order_id = $1 AND COALESCE(oi.producer_id, pt.producer_id) IS NOT NULL
		ON CONFLICT (order_item_id) WHERE kind = 'produksi' DO NOTHING`, orderID); err != nil {
		return err
	}
	// produsen juga mengirim paket: ongkir yang dibayar pembeli diteruskan ke
	// produsen item pertama (dengan satu vendor, selalu tepat)
	_, err := tx.Exec(ctx, `
		INSERT INTO producer_charges (producer_id, order_id, kind, amount)
		SELECT first.producer_id, o.id, 'ongkir', o.shipping_cost
		FROM orders o
		CROSS JOIN LATERAL (
			SELECT COALESCE(oi.producer_id, pt.producer_id) AS producer_id
			FROM order_items oi JOIN product_types pt ON pt.id = oi.type
			WHERE oi.order_id = o.id ORDER BY oi.id LIMIT 1
		) first
		WHERE o.id = $1 AND o.shipping_cost > 0 AND first.producer_id IS NOT NULL
		ON CONFLICT (order_id) WHERE kind = 'ongkir' DO NOTHING`, orderID)
	return err
}

// GatewayFee memperkirakan biaya Midtrans untuk satu pembayaran.
//
// ESTIMASI dari tarif publik yang umum — bukan angka dari Midtrans. Cocokkan
// dengan kontrak/dashboard Midtrans sebelum dipakai untuk laporan pajak.
func GatewayFee(method, paymentType string, amount int) int {
	k := strings.ToLower(paymentType + " " + method)
	pct := func(p float64) int { return int(float64(amount)*p + 0.5) }
	switch {
	case strings.Contains(k, "manual"):
		return 0 // dibayar di luar gateway (dicatat admin)
	case strings.Contains(k, "qris"):
		return pct(0.007)
	case strings.Contains(k, "credit_card"), strings.Contains(k, "kartu"):
		return pct(0.029) + 2000
	case strings.Contains(k, "bank_transfer"), strings.Contains(k, "echannel"), strings.Contains(k, "va "), strings.HasPrefix(strings.TrimSpace(k), "va"):
		return 4000
	case strings.Contains(k, "cstore"), strings.Contains(k, "indomaret"), strings.Contains(k, "alfamart"):
		return 5000
	default: // GoPay, ShopeePay, OVO, DANA, dan metode lain: anggap tarif e-wallet
		return pct(0.02)
	}
}
