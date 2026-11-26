package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Reversal side of the ledger (refunds, cancellations, reprints). The booking
// side is BookPaidOrder in ledger.go.

// ErrChargesPaidOut: a producer charge of the order was already paid to the vendor.
var ErrChargesPaidOut = errors.New("tagihan produsen pesanan ini sudah dibayarkan")

// ReverseRoyalties takes back creator royalties in proportion to the money
// refunded so far. refundedTotal is the CUMULATIVE refund of the order
// (including the refund being booked now), paid the amount the buyer paid.
// Per item: reversed = round(original × refundedTotal / paid), never decreasing,
// so a refund that reaches the full amount reverses the royalty completely
// and repeated partial refunds never over- or under-reverse by rounding.
// Returns how much was reversed by this call.
func ReverseRoyalties(ctx context.Context, tx pgx.Tx, orderID string, refundedTotal, paid int) (int, error) {
	if paid <= 0 || refundedTotal <= 0 {
		return 0, nil
	}
	var delta int
	err := tx.QueryRow(ctx, `
		WITH t AS (
			SELECT r.id, r.amount + r.reversed_amount AS orig, r.reversed_amount AS before,
			       LEAST(r.amount + r.reversed_amount,
			             round((r.amount + r.reversed_amount)::numeric * $2::numeric / $3::numeric)::int) AS target
			FROM royalties r JOIN order_items oi ON oi.id = r.order_item_id
			WHERE oi.order_id = $1
		), u AS (
			UPDATE royalties r SET amount = t.orig - t.target, reversed_amount = t.target
			FROM t WHERE t.id = r.id AND t.target > t.before
			RETURNING t.target - t.before AS d
		)
		SELECT COALESCE(sum(d), 0)::int FROM u`, orderID, refundedTotal, paid).Scan(&delta)
	return delta, err
}

// ReverseCancelledOrder undoes what payment booked for an order that is
// cancelled before production: the vendor charges are deleted (a `dibayar`
// order was never shipped, so none can have been paid out — if one was, the
// cancel is refused with ErrChargesPaidOut) and the listings' sold counters
// go back down. Royalties are reversed separately by ReverseRoyalties.
// Returns the vendor amount that was reversed.
func ReverseCancelledOrder(ctx context.Context, tx pgx.Tx, orderID string) (int, error) {
	var paidOut int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)::int FROM producer_charges WHERE order_id = $1 AND payout_id IS NOT NULL`, orderID).Scan(&paidOut); err != nil {
		return 0, err
	}
	if paidOut > 0 {
		return 0, ErrChargesPaidOut
	}
	var vendor int
	if err := tx.QueryRow(ctx, `
		WITH d AS (DELETE FROM producer_charges WHERE order_id = $1 RETURNING amount)
		SELECT COALESCE(sum(amount), 0)::int FROM d`, orderID).Scan(&vendor); err != nil {
		return 0, err
	}
	// same per-listing summing as payOrder (one listing can sit on several lines)
	_, err := tx.Exec(ctx, `
		UPDATE listings l SET sold = GREATEST(0, l.sold - q.qty)
		FROM (SELECT listing_id, sum(qty)::int AS qty FROM order_items
		      WHERE order_id = $1 AND listing_id IS NOT NULL GROUP BY listing_id) q
		WHERE q.listing_id = l.id`, orderID)
	return vendor, err
}

// BookReprintOrder books what a free replacement order costs: the vendor's
// production charge per item (rates snapshotted on the copied items) and the
// shipping the vendor pays again. No royalty and no gateway fee: nobody paid.
// Idempotent like BookPaidOrder.
func BookReprintOrder(ctx context.Context, tx pgx.Tx, orderID string, shippingCharge int) error {
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
	if shippingCharge <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO producer_charges (producer_id, order_id, kind, amount)
		SELECT first.producer_id, $1, 'ongkir', $2
		FROM (
			SELECT COALESCE(oi.producer_id, pt.producer_id) AS producer_id
			FROM order_items oi JOIN product_types pt ON pt.id = oi.type
			WHERE oi.order_id = $1 ORDER BY oi.id LIMIT 1
		) first
		WHERE first.producer_id IS NOT NULL
		ON CONFLICT (order_id) WHERE kind = 'ongkir' DO NOTHING`, orderID, shippingCharge)
	return err
}
