package db

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seed_data.json is exported from the frontend catalog (lib/data.ts +
// lib/designs.ts) so both sides ship identical demo content.
//
//go:embed seed_data.json
var seedJSON []byte

type seedCatalog struct {
	ProductTypes map[string]struct {
		Label    string   `json:"label"`
		Base     int      `json:"base"`
		Sizes    []string `json:"sizes"`
		ColorIds []string `json:"colorIds"`
	} `json:"productTypes"`
	Colors map[string]struct {
		Label string `json:"label"`
		Hex   string `json:"hex"`
	} `json:"colors"`
	Designers []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		City      string  `json:"city"`
		Hue       int     `json:"hue"`
		Followers int     `json:"followers"`
		Rating    float64 `json:"rating"`
		Bio       string  `json:"bio"`
	} `json:"designers"`
	Designs []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		DesignerID string   `json:"designerId"`
		URI        string   `json:"uri"`
		Tags       []string `json:"tags"`
	} `json:"designs"`
	Listings []struct {
		ID       string  `json:"id"`
		DesignID string  `json:"designId"`
		Type     string  `json:"type"`
		Price    int     `json:"price"`
		Sold     int     `json:"sold"`
		Rating   float64 `json:"rating"`
		Badge    string  `json:"badge"`
	} `json:"listings"`
}

// physical print areas per product type (cm) — mirrors PRINT_CM in the frontend
var printAreas = map[string][2]float64{
	"kaos": {30, 40}, "hoodie": {28, 30}, "mug": {9, 8.5}, "totebag": {25, 30},
}

// AvatarURI: foto profil bawaan — inisial di atas warna khas kreator.
// Sama dengan backfill di migrations/003 dan lib/avatar.ts di frontend.
func AvatarURI(hue int, name string) string {
	initials := ""
	for _, part := range strings.Fields(name) {
		initials += strings.ToUpper(part[:1])
		if len(initials) == 2 {
			break
		}
	}
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 80 80"><rect width="80" height="80" rx="40" fill="hsl(%d 48%% 36%%)"/><text x="40" y="51" font-family="Arial,sans-serif" font-size="30" font-weight="700" fill="#fff" text-anchor="middle">%s</text></svg>`, hue, initials)
	return "data:image/svg+xml," + url.PathEscape(svg)
}

// SeedIfEmpty populates catalog + 14 days of demo analytics on first run.
func SeedIfEmpty(ctx context.Context, pool *pgxpool.Pool) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM listings`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var cat seedCatalog
	if err := json.Unmarshal(seedJSON, &cat); err != nil {
		return fmt.Errorf("seed_data.json: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rng := rand.New(rand.NewPCG(2026, 708)) // deterministic demo data
	now := time.Now()
	day := 24 * time.Hour

	// --- users (akun demo per peran; password di-set oleh EnsureDemoPasswords) ---
	users := [][4]string{
		{"u-admin", "admin", "admin@karyakita.id", "admin"},
		{"u-raka", "raka", "raka@karyakita.id", "designer"},
		{"u-demo", "demo", "demo@karyakita.id", "customer"},
	}
	names := map[string]string{"u-admin": "Admin KaryaKita", "u-raka": "Raka Wijaya", "u-demo": "Pelanggan Demo"}
	for _, u := range users {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, username, email, name, role, email_verified) VALUES ($1,$2,$3,$4,$5,true)`,
			u[0], u[1], u[2], names[u[0]], u[3]); err != nil {
			return err
		}
	}

	// --- katalog ---
	for id, c := range cat.Colors {
		if _, err := tx.Exec(ctx, `INSERT INTO colors (id, label, hex) VALUES ($1,$2,$3)`, id, c.Label, c.Hex); err != nil {
			return err
		}
	}
	for id, pt := range cat.ProductTypes {
		area := printAreas[id]
		if _, err := tx.Exec(ctx,
			`INSERT INTO product_types (id, label, base_price, print_max_w_cm, print_max_h_cm) VALUES ($1,$2,$3,$4,$5)`,
			id, pt.Label, pt.Base, area[0], area[1]); err != nil {
			return err
		}
		for i, s := range pt.Sizes {
			if _, err := tx.Exec(ctx, `INSERT INTO product_type_sizes (product_type_id, size, sort) VALUES ($1,$2,$3)`, id, s, i); err != nil {
				return err
			}
		}
		for i, c := range pt.ColorIds {
			if _, err := tx.Exec(ctx, `INSERT INTO product_type_colors (product_type_id, color_id, sort) VALUES ($1,$2,$3)`, id, c, i); err != nil {
				return err
			}
		}
	}
	for _, d := range cat.Designers {
		userID := any(nil)
		if d.ID == "d-raka" {
			userID = "u-raka"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO designers (id, user_id, name, city, bio, hue, followers, rating, avatar_uri) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			d.ID, userID, d.Name, d.City, d.Bio, d.Hue, d.Followers, d.Rating, AvatarURI(d.Hue, d.Name)); err != nil {
			return err
		}
	}
	for _, d := range cat.Designs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO designs (id, designer_id, title, uri, tags) VALUES ($1,$2,$3,$4,$5)`,
			d.ID, d.DesignerID, d.Title, d.URI, d.Tags); err != nil {
			return err
		}
	}
	for _, l := range cat.Listings {
		badge := any(nil)
		if l.Badge != "" {
			badge = l.Badge
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO listings (id, design_id, product_type_id, price, sold, rating, badge) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			l.ID, l.DesignID, l.Type, l.Price, l.Sold, l.Rating, badge); err != nil {
			return err
		}
	}

	// --- statistik harian 14 hari (funnel turun wajar) ---
	for i := 13; i >= 0; i-- {
		t := now.Add(-time.Duration(i) * day)
		weekend := t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
		visits := 400 + rng.IntN(160)
		if weekend {
			visits += 120
		}
		views := int(float64(visits) * (0.52 + rng.Float64()*0.16))
		carts := int(float64(visits) * (0.14 + rng.Float64()*0.07))
		checkout := int(float64(carts) * (0.5 + rng.Float64()*0.18))
		paid := int(float64(checkout) * (0.72 + rng.Float64()*0.14))
		revenue := int64(paid) * int64(120000+rng.IntN(70000))
		if _, err := tx.Exec(ctx,
			`INSERT INTO day_stats (date, visits, product_views, add_to_cart, checkout, paid, revenue) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			t.Format("2006-01-02"), visits, views, carts, checkout, paid, revenue); err != nil {
			return err
		}
	}

	// --- web vitals (3 hari terakhir, sebaran realistis) ---
	pages := []string{"/", "/products", "/product/kaos-anak-senja", "/checkout"}
	for i := 0; i < 80; i++ {
		page := pages[rng.IntN(len(pages))]
		t := now.Add(-time.Duration(rng.Int64N(int64(3 * day))))
		samples := []struct {
			name  string
			value float64
		}{
			{"LCP", 1400 + rng.Float64()*rng.Float64()*3400},
			{"INP", 60 + rng.Float64()*rng.Float64()*460},
			{"CLS", rng.Float64() * rng.Float64() * 0.28},
			{"TTFB", 180 + rng.Float64()*700},
		}
		for _, s := range samples {
			if _, err := tx.Exec(ctx,
				`INSERT INTO web_vitals (name, value, page, created_at) VALUES ($1,$2,$3,$4)`,
				s.name, s.value, page, t); err != nil {
				return err
			}
		}
	}

	// --- metrik API (24 jam terakhir) ---
	routes := []string{"/api/orders", "/api/stats", "/api/track", "/api/designs"}
	for i := 0; i < 200; i++ {
		ok := rng.Float64() > 0.015
		status := 200
		if !ok {
			status = 500
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO api_metrics (route, method, status, ms, ok, created_at) VALUES ($1,'GET',$2,$3,$4,$5)`,
			routes[rng.IntN(len(routes))], status, 8+rng.Float64()*rng.Float64()*220, ok,
			now.Add(-time.Duration(rng.Int64N(int64(day))))); err != nil {
			return err
		}
	}

	// --- pencarian populer ---
	terms := map[string]int{
		"kaos batik": 48, "senja": 41, "hoodie couple": 33, "mug kantor": 29,
		"totebag kampus": 27, "kopi": 22, "komodo": 15, "kado wisuda": 11,
	}
	for term, count := range terms {
		if _, err := tx.Exec(ctx, `INSERT INTO search_terms (term, count) VALUES ($1,$2)`, term, count); err != nil {
			return err
		}
	}

	// --- 12 pesanan demo lengkap (item, pembayaran, timeline, royalti) ---
	if err := seedOrders(ctx, tx, &cat, rng, now); err != nil {
		return err
	}

	// --- ulasan demo dari pesanan yang sudah selesai ---
	if _, err := tx.Exec(ctx, `
		INSERT INTO reviews (listing_id, order_id, author, rating, comment, status)
		SELECT DISTINCT ON (o.id) oi.listing_id, o.id, o.cust_name, 5,
		       'Kualitas cetaknya bagus, warna tajam dan bahannya adem. Recommended!',
		       'disetujui'::review_status
		FROM orders o
		JOIN order_items oi ON oi.order_id = o.id
		WHERE o.status = 'selesai' AND oi.listing_id IS NOT NULL
		ON CONFLICT DO NOTHING`); err != nil {
		return err
	}

	// --- contoh payout kreator ---
	payouts := []struct {
		designer string
		amount   int
		status   string
		daysAgo  int
	}{
		{"d-raka", 1250000, "dibayar", 21},
		{"d-raka", 890000, "diminta", 1},
		{"d-tiara", 640000, "dibayar", 14},
	}
	for _, p := range payouts {
		reqAt := now.Add(-time.Duration(p.daysAgo) * day)
		paidAt := any(nil)
		if p.status == "dibayar" {
			paidAt = reqAt.Add(2 * day)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO payouts (designer_id, amount, status, requested_at, paid_at) VALUES ($1,$2,$3,$4,$5)`,
			p.designer, p.amount, p.status, reqAt, paidAt); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func seedOrders(ctx context.Context, tx pgx.Tx, cat *seedCatalog, rng *rand.Rand, now time.Time) error {
	day := 24 * time.Hour
	customers := [][2]string{
		{"Dewi Anggraini", "Jakarta Selatan"}, {"Fajar Ramadhan", "Surabaya"}, {"Putri Ayu", "Bandung"},
		{"Andi Saputra", "Makassar"}, {"Rina Melati", "Semarang"}, {"Yoga Pratama", "Yogyakarta"},
		{"Siti Nurhaliza", "Medan"}, {"Bayu Nugroho", "Malang"}, {"Laras Sekar", "Depok"}, {"Eko Prabowo", "Bekasi"},
	}
	methods := []string{"QRIS", "GoPay", "OVO", "VA BCA", "VA Mandiri", "DANA"}
	statuses := []string{"dibayar", "produksi", "produksi", "dikirim", "dikirim", "selesai"}

	designByID := map[string]string{}
	designerByDesign := map[string]string{}
	for _, d := range cat.Designs {
		designByID[d.ID] = d.Title
		designerByDesign[d.ID] = d.DesignerID
	}

	for i := 0; i < 12; i++ {
		c := customers[i%len(customers)]
		l := cat.Listings[rng.IntN(len(cat.Listings))]
		pt := cat.ProductTypes[l.Type]
		qty := 1 + rng.IntN(2)
		createdAt := now.Add(-time.Duration(rng.Int64N(int64(5 * day))))
		paidAt := createdAt.Add(10 * time.Minute)
		status := statuses[rng.IntN(len(statuses))]
		shipCost := 18000 + rng.IntN(3)*7000
		subtotal := l.Price * qty
		id := fmt.Sprintf("KK-%s-%d", createdAt.Format("060102"), 1000+rng.IntN(9000))
		title := pt.Label + " " + designByID[l.DesignID]

		if _, err := tx.Exec(ctx,
			`INSERT INTO orders (id, cust_name, cust_email, cust_phone, cust_address, cust_city, subtotal, courier, shipping_cost, total, status, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (id) DO NOTHING`,
			id, c[0], fmt.Sprintf("%s@mail.com", c[0][:4]), fmt.Sprintf("08%010d", rng.IntN(1_000_000_000)),
			fmt.Sprintf("Jl. Merdeka No. %d", 1+rng.IntN(99)), c[1],
			subtotal, "JNE REG", shipCost, subtotal+shipCost, status, createdAt); err != nil {
			return err
		}

		var itemID int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO order_items (order_id, listing_id, title, type, color, size, qty, unit_price)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			id, l.ID, title, l.Type, pt.ColorIds[0], pt.Sizes[0], qty, l.Price).Scan(&itemID); err != nil {
			return err
		}

		method := methods[rng.IntN(len(methods))]
		if _, err := tx.Exec(ctx,
			`INSERT INTO payments (order_id, method, status, ref, amount, paid_at) VALUES ($1,$2,'paid',$3,$4,$5)`,
			id, method, fmt.Sprintf("MID-%08d", rng.IntN(100_000_000)), subtotal+shipCost, paidAt); err != nil {
			return err
		}

		for _, ev := range []struct {
			label string
			at    time.Time
		}{{"Pesanan dibuat", createdAt}, {"Pembayaran diterima (" + method + ")", paidAt}} {
			if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, label, at) VALUES ($1,$2,$3)`, id, ev.label, ev.at); err != nil {
				return err
			}
		}

		// royalti kreator 12% dari nilai item
		if designer := designerByDesign[l.DesignID]; designer != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO royalties (designer_id, order_item_id, amount, created_at) VALUES ($1,$2,$3,$4)`,
				designer, itemID, int(float64(subtotal)*0.12), paidAt); err != nil {
				return err
			}
		}
	}
	return nil
}
