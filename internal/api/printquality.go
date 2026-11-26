package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"karyakita/api/internal/files"
)

// Print quality (feat/print-quality): the effective resolution of a raster
// design is width_px / (width_cm / 2.54). Below minPrintDPI the vendor's print
// would look blurry, so designs are refused; below goodPrintDPI the Studio only
// warns. Pixel sizes always come from the files table (measured by the server
// when the upload completed), never from the client. Vector designs (SVG) have
// no resolution and are exempt.

const (
	minPrintDPI  = 150
	goodPrintDPI = 300
	cmPerInch    = 2.54
)

// effectiveDPI is the resolution of an image widthPx wide printed widthCm wide.
func effectiveDPI(widthPx int, widthCm float64) float64 {
	return float64(widthPx) / (widthCm / cmPerInch)
}

// maxWidthForDPI is the widest print (cm, rounded down to the 0.1 cm Studio
// step) that still reaches dpi.
func maxWidthForDPI(widthPx int, dpi float64) float64 {
	return math.Floor(float64(widthPx)/dpi*cmPerInch*10+1e-6) / 10
}

// checkResolution refuses a print width that would drop a widthPx-wide raster
// below minPrintDPI. The message carries the computed DPI and the widest
// printable width.
func checkResolution(widthPx int, widthCm float64) error {
	dpi := effectiveDPI(widthPx, widthCm)
	if dpi >= minPrintDPI {
		return nil
	}
	shown := fmt.Sprintf("%.0f", dpi)
	if math.Round(dpi) >= minPrintDPI { // 149.5–149.99 must not read as "150 of 150"
		shown = fmt.Sprintf("%.1f", dpi)
	}
	maxCm := maxWidthForDPI(widthPx, minPrintDPI)
	if maxCm < minPrintWidthCm {
		return checkoutError(fmt.Sprintf(
			"Resolusi gambar terlalu rendah untuk dicetak (%d px lebar, minimal %d DPI). Unggah gambar dengan piksel lebih banyak.",
			widthPx, minPrintDPI))
	}
	return checkoutError(fmt.Sprintf(
		"Resolusi terlalu rendah untuk lebar cetak %s cm: hanya %s DPI (minimal %d DPI). Perkecil lebar cetak hingga maksimal %s cm atau unggah gambar yang lebih besar.",
		cm(widthCm), shown, minPrintDPI, cm(maxCm)))
}

// designPixelWidth returns the pixel width of a design image, or vector=true
// when it has none. Inline data: URIs are decoded here (the server measures
// them itself); hosted files are looked up in the files table, and a URL that
// is not one of our recorded uploads is refused.
func designPixelWidth(ctx context.Context, q querier, uri string) (widthPx int, vector bool, err error) {
	if strings.HasPrefix(uri, "data:image/svg+xml") {
		return 0, true, nil
	}
	if rest, ok := strings.CutPrefix(uri, "data:image/"); ok {
		_, payload, ok := strings.Cut(rest, ";base64,")
		if !ok {
			return 0, false, checkoutError("Gambar desain tidak valid — unggah ulang dari Studio")
		}
		raw, derr := base64.StdEncoding.DecodeString(payload)
		if derr != nil {
			return 0, false, checkoutError("Gambar desain tidak valid — unggah ulang dari Studio")
		}
		info, derr := files.Inspect(raw, int64(len(raw)))
		if derr != nil {
			return 0, false, checkoutError("Gambar desain tidak terbaca — unggah ulang dari Studio")
		}
		if info.Width == 0 {
			return 0, true, nil
		}
		return info.Width, false, nil
	}
	notFound := checkoutError("Gambar desain tidak ditemukan di penyimpanan KaryaKita — unggah ulang dari Studio")
	if hostedFilePrefix == "" {
		return 0, false, notFound
	}
	key, ok := strings.CutPrefix(uri, hostedFilePrefix)
	if !ok {
		return 0, false, notFound
	}
	if _, ok := files.ParseKey(key); !ok {
		return 0, false, notFound
	}
	var px *int
	if err := q.QueryRow(ctx, `SELECT width_px FROM files WHERE key = $1`, key).Scan(&px); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, notFound
		}
		return 0, false, err
	}
	if px == nil {
		return 0, true, nil
	}
	return *px, false, nil
}

// checkDesignResolution enforces minPrintDPI for a design image printed
// widthCm wide. Errors of type checkoutError are the user's to fix (400).
func checkDesignResolution(ctx context.Context, q querier, uri string, widthCm float64) error {
	px, vector, err := designPixelWidth(ctx, q, uri)
	if err != nil || vector {
		return err
	}
	return checkResolution(px, widthCm)
}

// fillItemDPI fills CartLine.DPI for order items whose design is a recorded
// raster upload, so production can spot risky prints. Items with unknown or
// vector designs keep DPI 0 (omitted from the JSON).
func (s *Server) fillItemDPI(ctx context.Context, orders []Order) error {
	keys := map[string]bool{}
	if hostedFilePrefix == "" {
		return nil
	}
	for _, o := range orders {
		for _, it := range o.Items {
			if k, ok := strings.CutPrefix(it.DesignURI, hostedFilePrefix); ok {
				keys[k] = true
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	rows, err := s.pool.Query(ctx, `SELECT key, width_px FROM files WHERE key = ANY($1) AND width_px IS NOT NULL`, list)
	if err != nil {
		return err
	}
	defer rows.Close()
	widths := map[string]int{}
	for rows.Next() {
		var k string
		var w int
		if err := rows.Scan(&k, &w); err != nil {
			return err
		}
		widths[k] = w
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range orders {
		for j := range orders[i].Items {
			it := &orders[i].Items[j]
			k, ok := strings.CutPrefix(it.DesignURI, hostedFilePrefix)
			if w := widths[k]; ok && w > 0 && it.WidthCm > 0 {
				it.DPI = int(math.Round(effectiveDPI(w, it.WidthCm)))
			}
		}
	}
	return nil
}
