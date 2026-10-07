# KaryaKita API — Backend (Go + PostgreSQL)

Backend marketplace print-on-demand KaryaKita. Frontend Next.js ada di repo terpisah
(`../frontend`) dan mem-proxy semua `/api/*` ke server ini.

## Menjalankan

```bash
docker compose up -d    # 1. Postgres 16 + Mailpit (butuh Docker Desktop jalan)
go run ./cmd/api        # 2. API di http://localhost:8081
```

Semua email dev (verifikasi, reset password) tertangkap di **Mailpit: http://localhost:8025**.

Saat pertama kali start, API otomatis:
1. membuat skema (25 tabel — lihat [DATABASE.md](DATABASE.md) + diagram ERD),
2. mengisi data demo (katalog, 12 pesanan, statistik 14 hari),
3. di luar produksi: memberi password ke akun demo (lihat di bawah).

Konfigurasi lewat env var (lihat `.env.example`): `DATABASE_URL`, `PORT` (default 8081),
`UPLOAD_DIR`, `APP_ENV`, `DEMO_PASSWORD`, `COOKIE_SECURE`, `APP_URL`, `SMTP_*`, `MAIL_FROM`, `GOOGLE_*`.

## Autentikasi

- **Username/email + password** (bcrypt). Sesi = token acak di cookie `kk_session`
  (HttpOnly, SameSite=Lax, 30 hari); database hanya menyimpan sha256-nya (tabel `sessions`).
- **Peran**: `customer`, `designer` (punya profil toko di `designers`), `admin`.
  Daftar sebagai kreator (`asCreator: true`) langsung membuat profil toko.
- **Login Google** aktif bila `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REDIRECT_URL`
  diisi. Akun Google ditautkan ke akun lama dengan email yang sama, atau dibuat baru (pelanggan).
- **Verifikasi email**: daftar dengan password → email berisi link `{APP_URL}/verifikasi-email?token=…`
  (berlaku 24 jam). Akun tetap bisa dipakai; **kreator wajib terverifikasi untuk mengajukan
  desain**. Akun Google dan akun demo sudah terverifikasi.
- **Reset password**: `/lupa-password` → email berisi link `{APP_URL}/reset-password?token=…`
  (30 menit, sekali pakai). Berhasil = password baru, semua sesi lama dicabut, langsung masuk,
  plus email pemberitahuan. Respons "lupa password" selalu sama agar tidak membocorkan email
  mana yang terdaftar.
- Token email disimpan seperti sesi: hanya sha256-nya (tabel `auth_tokens`); meminta link baru
  membatalkan link lama.
- Batas percobaan: login 10×/15 menit per akun & 30×/15 menit per IP; daftar 10×/jam per IP
  (di memori — pindahkan ke Redis bila API lebih dari satu replika).

Akun demo (password `karyakita123`, atau `DEMO_PASSWORD`; tidak di-set bila `APP_ENV=production`):

| Username | Peran |
|---|---|
| `admin` | admin |
| `raka` | kreator (Raka Wijaya) |
| `demo` | pelanggan |

## Endpoint

| Endpoint | Method | Fungsi |
|---|---|---|
Kolom **Akses**: publik · login (peran apa pun) · kreator · admin. Tanpa sesi → `401`,
peran salah → `403`.

| Endpoint | Method | Akses | Fungsi |
|---|---|---|---|
| `/healthz` | GET | publik | Cek kesehatan (ping DB) |
| `/api/auth/signup` | POST | publik | `{username, email, password, name, asCreator, city}` → user + cookie sesi |
| `/api/auth/login` | POST | publik | `{identifier, password}` — identifier = username atau email |
| `/api/auth/logout` | POST | publik | Hapus sesi |
| `/api/auth/me` | GET | publik | `{user}` atau `{user: null}` |
| `/api/auth/providers` | GET | publik | `{password, google}` — metode login yang aktif |
| `/api/auth/verify-email` | POST | publik | `{token}` dari link email |
| `/api/auth/verify-email/resend` | POST | login | Kirim ulang link verifikasi |
| `/api/auth/password/forgot` | POST | publik | `{email}` → kirim link reset (respons selalu sama) |
| `/api/auth/password/reset` | POST | publik | `{token, password}` → password baru + sesi baru |
| `/api/auth/google/start`, `/callback` | GET | publik | Alur OAuth Google (redirect) |
| `/api/products`, `/api/products/{id}`, `/api/categories` | GET | publik | Katalog |
| `/api/checkout/quote` | POST | login | `{items, courier, voucher}` → ringkasan harga dari server (harga, ongkir, diskon) |
| `/api/orders` | POST | login | Buat pesanan — harga dihitung ulang di server, tertaut ke akun |
| `/api/orders` | GET | admin | Daftar pesanan `?status=&q=&limit=&offset=` → `{orders, total}` |
| `/api/orders/mine` | GET | login | Riwayat pesanan akun ini |
| `/api/orders/{id}` | GET | pemilik, admin | Detail (akun lain → 404) |
| `/api/orders/{id}` | PATCH | pemilik, admin | `{action:"pay"}` (≈ webhook gateway); `{action:"advance"}` hanya admin |
| `/api/designer/orders` | GET | kreator, admin | Item pesanan yang memuat produk kreator + royalti (tanpa alamat/kontak pembeli) |
| `/api/vouchers` | GET / POST | admin | Daftar (+ pemakaian) / buat voucher |
| `/api/vouchers/{code}` | PATCH | admin | `{active}` |
| `/api/designs` | GET / POST | kreator, admin | Pengajuan (kreator: hanya miliknya) / ajukan atas nama toko sendiri |
| `/api/designs` | PATCH | admin | Moderasi |
| `/api/listings/{id}` | PATCH | kreator (pemilik), admin | Ubah listing |
| `/api/reviews` | GET | publik | Ulasan yang lolos moderasi |
| `/api/reviews` | POST | login | Ulasan — hanya untuk pesanan selesai milik akun ini |
| `/api/reviews` | PATCH | admin | Moderasi ulasan |
| `/api/profile`, `/api/user-designs` | * | login | Profil & desain custom milik akun |
| `/api/uploads` | POST | login | Unggah gambar |
| `/api/track` | POST / GET | publik / admin | Rekam / baca event perilaku |
| `/api/vitals` | POST | publik | Rekam Web Vitals (LCP/INP/CLS/TTFB) |
| `/api/stats` | GET | kreator, admin | Agregasi dashboard (funnel, p75 vitals, p50/p95 API) |

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
  server.go            router + tabel akses, CORS, middleware metrik
  auth.go              signup/login/logout, sesi cookie, s.authed (cek peran)
  auth_email.go        verifikasi email + lupa/reset password (token sekali pakai)
  google.go            login Google (OAuth code flow + userinfo)
internal/mail/         pengirim SMTP (Mailpit di dev, penyedia di produksi) + template email
  checkout.go          quote (harga dari DB), kurir, voucher, kelola voucher (admin)
  orders.go            buat pesanan, akses pemilik/admin, pembayaran, royalti, pesanan masuk kreator
  designs.go           pengajuan & moderasi desain
  track.go             event perilaku + web vitals
  stats.go             agregasi dashboard (SQL percentile_cont, FILTER)
  products.go          katalog
```

## Checkout & voucher

- Browser hanya mengirim **apa** yang dibeli (produk, warna, ukuran, qty). Harga satuan, ongkir,
  dan diskon dihitung server (`quoteOrder`) — dipakai oleh `/api/checkout/quote` dan saat pesanan
  dibuat, jadi angka yang tampil = angka yang ditagih.
- Ukuran berbeda dari produk yang sama = baris terpisah; baris kembar (produk+warna+ukuran) digabung.
  Maks 99 per baris, 30 baris per pesanan. Ukuran/warna divalidasi terhadap katalog; listing
  nonaktif ditolak. Desain custom: harga dasar produk + Rp25.000, gambar harus `/uploads/…`.
- Voucher: `percent` (dengan batas maks), `fixed`, `shipping` (subsidi ongkir); syarat min. belanja,
  masa berlaku, kuota total, batas per akun. Voucher yang tidak berlaku saat pesanan dibuat → 400
  (tidak diam-diam ditagih harga penuh). Diskon ditanggung platform: royalti tetap dari harga item.
- Voucher demo: `KARYAKITA10` (10%, maks Rp50.000, min Rp100.000), `HEMAT25` (Rp25.000, min
  Rp150.000, 1× per akun), `GRATISONGKIR` (ongkir s.d. Rp20.000).
