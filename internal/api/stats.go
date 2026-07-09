package api

import (
	"net/http"
	"time"
)

// getStats returns the aggregate payload the /admin dashboard renders.
// Shape matches the old in-memory computeStats() exactly.
func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type DayStat struct {
		Date         string `json:"date"`
		Visits       int    `json:"visits"`
		ProductViews int    `json:"productViews"`
		AddToCart    int    `json:"addToCart"`
		Checkout     int    `json:"checkout"`
		Paid         int    `json:"paid"`
		Revenue      int64  `json:"revenue"`
	}

	// --- 14 hari terakhir dari day_stats ---
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(date, 'YYYY-MM-DD'), visits, product_views, add_to_cart, checkout, paid, revenue
		FROM day_stats ORDER BY date DESC LIMIT 14`)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var days []DayStat
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Date, &d.Visits, &d.ProductViews, &d.AddToCart, &d.Checkout, &d.Paid, &d.Revenue); err != nil {
			rows.Close()
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		days = append([]DayStat{d}, days...) // balik jadi urutan naik
	}
	rows.Close()

	// --- gabungkan event live hari ini ---
	today := time.Now().Format("2006-01-02")
	var live struct {
		visits, views, carts, checkout, paid int
		revenue                              int64
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT session_id) FILTER (WHERE type = 'page_view'),
		       count(*) FILTER (WHERE type = 'view_product'),
		       count(*) FILTER (WHERE type = 'add_to_cart'),
		       count(*) FILTER (WHERE type = 'begin_checkout'),
		       count(*) FILTER (WHERE type = 'purchase'),
		       COALESCE(sum(value) FILTER (WHERE type = 'purchase'), 0)
		FROM track_events WHERE created_at >= date_trunc('day', now())`).
		Scan(&live.visits, &live.views, &live.carts, &live.checkout, &live.paid, &live.revenue); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	merged := false
	for i := range days {
		if days[i].Date == today {
			days[i].Visits += live.visits
			days[i].ProductViews += live.views
			days[i].AddToCart += live.carts
			days[i].Checkout += live.checkout
			days[i].Paid += live.paid
			days[i].Revenue += live.revenue
			merged = true
		}
	}
	if !merged {
		days = append(days, DayStat{Date: today, Visits: live.visits, ProductViews: live.views,
			AddToCart: live.carts, Checkout: live.checkout, Paid: live.paid, Revenue: live.revenue})
		if len(days) > 14 {
			days = days[1:]
		}
	}

	totals := map[string]int64{}
	for _, d := range days {
		totals["visits"] += int64(d.Visits)
		totals["views"] += int64(d.ProductViews)
		totals["carts"] += int64(d.AddToCart)
		totals["checkout"] += int64(d.Checkout)
		totals["paid"] += int64(d.Paid)
		totals["revenue"] += d.Revenue
	}
	var aov int64
	if totals["paid"] > 0 {
		aov = totals["revenue"] / totals["paid"]
	}
	var conversion float64
	if totals["visits"] > 0 {
		conversion = float64(totals["paid"]) / float64(totals["visits"]) * 100
		conversion = float64(int(conversion*100)) / 100
	}

	// --- produk terlaris ---
	type TopProduct struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Sold    int    `json:"sold"`
		Revenue int64  `json:"revenue"`
	}
	topProducts := []TopProduct{}
	rows, err = s.pool.Query(ctx, `
		SELECT l.id, pt.label || ' ' || d.title, l.sold, (l.sold::bigint * l.price)
		FROM listings l
		JOIN designs d ON d.id = l.design_id
		JOIN product_types pt ON pt.id = l.product_type_id
		ORDER BY l.sold DESC LIMIT 6`)
	if err == nil {
		for rows.Next() {
			var p TopProduct
			if err := rows.Scan(&p.ID, &p.Title, &p.Sold, &p.Revenue); err == nil {
				topProducts = append(topProducts, p)
			}
		}
		rows.Close()
	}

	// --- pencarian terpopuler ---
	type Search struct {
		Term  string `json:"term"`
		Count int    `json:"count"`
	}
	searches := []Search{}
	rows, err = s.pool.Query(ctx, `SELECT term, count FROM search_terms ORDER BY count DESC LIMIT 8`)
	if err == nil {
		for rows.Next() {
			var t Search
			if err := rows.Scan(&t.Term, &t.Count); err == nil {
				searches = append(searches, t)
			}
		}
		rows.Close()
	}

	// --- perangkat (baseline seed + live) ---
	devices := map[string]int{"mobile": 62, "desktop": 33, "tablet": 5}
	rows, err = s.pool.Query(ctx, `
		SELECT COALESCE(device,'desktop'), count(*) FROM track_events
		WHERE type = 'page_view' GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var dev string
			var n int
			if err := rows.Scan(&dev, &n); err == nil {
				if dev != "mobile" {
					dev = "desktop"
				}
				devices[dev] += n
			}
		}
		rows.Close()
	}

	// --- Web Vitals p75 ---
	vitals := map[string]any{"lcp": 0.0, "inp": 0.0, "cls": 0.0, "ttfb": 0.0, "samples": 0}
	rows, err = s.pool.Query(ctx, `
		SELECT lower(name::text), percentile_cont(0.75) WITHIN GROUP (ORDER BY value)
		FROM web_vitals GROUP BY name`)
	if err == nil {
		for rows.Next() {
			var name string
			var p75 float64
			if err := rows.Scan(&name, &p75); err == nil {
				vitals[name] = p75
			}
		}
		rows.Close()
	}
	var vitalSamples int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM web_vitals`).Scan(&vitalSamples)
	vitals["samples"] = vitalSamples

	// --- kesehatan API 24 jam terakhir ---
	var apiStats struct {
		p50, p95  *float64
		errorRate float64
		count     int
	}
	var errCount int
	_ = s.pool.QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY ms),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY ms),
		       count(*), count(*) FILTER (WHERE NOT ok)
		FROM api_metrics WHERE created_at >= now() - interval '24 hours'`).
		Scan(&apiStats.p50, &apiStats.p95, &apiStats.count, &errCount)
	if apiStats.count > 0 {
		apiStats.errorRate = float64(int(float64(errCount)/float64(apiStats.count)*100*100)) / 100
	}
	p50, p95 := 0, 0
	if apiStats.p50 != nil {
		p50 = int(*apiStats.p50 + 0.5)
	}
	if apiStats.p95 != nil {
		p95 = int(*apiStats.p95 + 0.5)
	}

	// --- feed aktivitas + pesanan terbaru + antrean moderasi ---
	recentEvents, err := s.recentEvents(ctx, 40)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	orders, err := s.loadOrders(ctx, 10)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var pendingDesigns int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM design_submissions WHERE status = 'review'`).Scan(&pendingDesigns)

	writeJSON(w, http.StatusOK, map[string]any{
		"days": days,
		"totals": map[string]int64{
			"visits": totals["visits"], "views": totals["views"], "carts": totals["carts"],
			"checkout": totals["checkout"], "paid": totals["paid"], "revenue": totals["revenue"],
		},
		"aov":            aov,
		"conversion":     conversion,
		"topProducts":    topProducts,
		"searches":       searches,
		"devices":        devices,
		"vitals":         vitals,
		"api":            map[string]any{"p50": p50, "p95": p95, "errorRate": apiStats.errorRate, "count": apiStats.count},
		"recentEvents":   recentEvents,
		"orders":         orders,
		"pendingDesigns": pendingDesigns,
	})
}
