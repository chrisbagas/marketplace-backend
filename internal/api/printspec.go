package api

import (
	"context"
	"fmt"
	"math"
)

// Print spec (migration 016): how wide the vendor prints a design and how far
// it sits from the centre of the print area. Width is 0 < w <= the product
// type's print_max_w_cm; the offset is a vertical shift in cm, positive =
// towards the hem, and must stay within half the print area height so the
// design centre never leaves the printable area.

const minPrintWidthCm = 1.0

// printLimits reads the physical print area of a product type.
// It returns pgx.ErrNoRows for an unknown type.
func printLimits(ctx context.Context, q querier, typeID string) (maxW, maxH float64, err error) {
	err = q.QueryRow(ctx, `SELECT print_max_w_cm, print_max_h_cm FROM product_types WHERE id = $1`, typeID).Scan(&maxW, &maxH)
	return maxW, maxH, err
}

// round1 rounds to the one decimal the numeric(5,1) columns keep.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// checkPrintSpec validates a spec against the print area and returns it rounded
// to one decimal.
func checkPrintSpec(width, offset, maxW, maxH float64) (float64, float64, error) {
	width, offset = round1(width), round1(offset)
	if width < minPrintWidthCm || width > maxW {
		return 0, 0, checkoutError(fmt.Sprintf("Lebar cetak harus antara %s dan %s cm untuk produk ini", cm(minPrintWidthCm), cm(maxW)))
	}
	if limit := maxH / 2; math.Abs(offset) > limit {
		return 0, 0, checkoutError(fmt.Sprintf("Posisi vertikal maksimal %s cm dari tengah area cetak", cm(limit)))
	}
	return width, offset, nil
}

// cm formats a centimetre value without a trailing ".0".
func cm(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}
