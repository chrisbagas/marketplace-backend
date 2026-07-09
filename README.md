# KaryaKita API — Backend (Go + PostgreSQL)

Backend marketplace print-on-demand KaryaKita. Frontend Next.js ada di repo terpisah
(`../frontend`) dan mem-proxy semua `/api/*` ke server ini.

## Menjalankan

```bash
docker compose up -d    # 1. Postgres 16 (butuh Docker Desktop jalan)
go run ./cmd/api        # 2. API di http://localhost:8081
```

Saat pertama kali start, API otomatis:
1. membuat skema (20 tabel — lihat [DATABASE.md](DATABASE.md) + diagram ERD),
2. mengisi data demo (katalog, 12 pesanan, statistik 14 hari).

Konfigurasi lewat env var (lihat `.env.example`): `DATABASE_URL`, `PORT` (default 8081).

## Endpoint

| Endpoint | Method | Fungsi |
|---|---|---|
| `/healthz` | GET | Cek kesehatan (ping DB) |
| `/api/products`, `/api/products/{id}` | GET | Katalog dari database (`?type=` filter) |
| `/api/orders` | POST / GET | Buat pesanan / daftar pesanan |
| `/api/orders/{id}` | GET / PATCH | Detail / `{action:"pay"}` (≈ webhook gateway, membukukan royalti) / `{action:"advance"}` |
| `/api/designs` | GET / POST / PATCH | Daftar (`?status=`) / ajukan desain / moderasi |
| `/api/track` | POST / GET | Rekam / baca event perilaku |
| `/api/vitals` | POST | Rekam Web Vitals (LCP/INP/CLS/TTFB) |
| `/api/stats` | GET | Agregasi dashboard admin (funnel, p75 vitals, p50/p95 API) |

Setiap request tercatat ke tabel `api_metrics` (latensi + status) — itulah sumber panel
"Performa" di dashboard admin.

## Struktur

```
cmd/api/main.go        entry point: koneksi DB → migrasi → seed → serve
internal/db/
  schema.sql           skema penuh (dieksekusi sekali, tercatat di schema_migrations)
  seed.go              data demo deterministik
  seed_data.json       katalog (diekspor dari frontend agar identik)
internal/api/
  server.go            router, CORS, middleware metrik
  orders.go            siklus pesanan + pembayaran + royalti
  designs.go           pengajuan & moderasi desain
  track.go             event perilaku + web vitals
  stats.go             agregasi dashboard (SQL percentile_cont, FILTER)
  products.go          katalog
```
