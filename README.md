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
`UPLOAD_DIR`, `APP_ENV`, `DEMO_PASSWORD`, `COOKIE_SECURE`, `APP_URL`, `SMTP_*`, `MAIL_FROM`, `GOOGLE_*`,
`BITESHIP_API_KEY`, `BITESHIP_WEBHOOK_SECRET`, `BITESHIP_COURIERS`, `SHIP_ORIGIN_*`.

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
| `/api/products`, `/api/products/{id}` | GET | publik | Katalog (daftar tanpa halaman + detail) |
| `/api/search` | GET | publik | Pencarian katalog: `q`, `type`, `category`, `attr`, `color`, `min`, `max`, `designer`, `sort`, `page`, `limit` → produk + jumlah per filter |
| `/api/search/suggest`, `/api/taxonomy` | GET | publik | Saran saat mengetik · kategori, atribut & warna beserta jumlah produknya |
| `/api/search/insights`, `/api/search/synonyms(/{id})`, `/api/attributes/{id}/values`, `/api/attribute-values/{id}` | GET/POST/PATCH/DELETE | admin | Pencarian tanpa hasil, sinonim, pilihan atribut |
| `/api/checkout/quote` | POST | login | `{items, courier, voucher}` → ringkasan harga dari server (harga, ongkir, diskon) |
| `/api/orders` | POST | login | Buat pesanan — harga dihitung ulang di server, tertaut ke akun |
| `/api/orders` | GET | admin | Daftar pesanan `?status=&q=&limit=&offset=` → `{orders, total}` |
| `/api/orders/mine` | GET | login | Riwayat pesanan akun ini |
| `/api/orders/{id}` | GET | pemilik, admin | Detail (akun lain → 404) |
| `/api/payments/config` | GET | publik | `{provider: midtrans, mock, atau none; clientKey; snapUrl}` untuk frontend |
| `/api/orders/{id}/payment` | POST | pemilik, admin | Buat/lanjutkan sesi Snap → `{token}` |
| `/api/orders/{id}/payment/sync` | POST | pemilik, admin | Server menanyakan status ke Midtrans & menerapkannya |
| `/api/payments/midtrans/notification` | POST | Midtrans | Webhook (tanda tangan + konfirmasi Status API) |
| `/api/orders/{id}` | PATCH | pemilik, admin | `{action:"pay"}` — hanya mode **mock** untuk pembeli; di mode Midtrans hanya admin (lunas manual). Khusus admin: `advance` (dibayar→produksi), `ship` `{courier, trackingNumber}` (produksi→dikirim, **resi wajib**), `ship-biteship` (pesan kurir lewat Biteship, resi otomatis), `sync-shipment` (tarik status pelacakan Biteship), `update-shipment` (koreksi resi), `delivered` (dikirim→tiba, laporan kurir). Khusus **pembeli**: `confirm` (tiba→selesai) |
| `/api/shipping/config` | GET | publik | `{provider: "biteship" \| "flat"}` — frontend memilih autocomplete area atau tarif flat |
| `/api/shipping/areas?q=` | GET | login | Autocomplete kota/kecamatan tujuan (Biteship Maps; ≥3 huruf; 404 saat mode flat) |
| `/api/shipping/biteship/webhook` | POST | Biteship | Webhook pelacakan (rahasia `X-Webhook-Secret` / `?secret=` + verifikasi ulang ke API pelacakan) |
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

## Pengiriman

Pesanan **tidak bisa** berstatus `dikirim`/`selesai` tanpa nama kurir + nomor resi: `advance` dari
`produksi` ditolak (400), admin harus memakai `ship` dengan resi (6–30 huruf/angka, dirapikan jadi
huruf besar tanpa spasi). Dijaga tiga lapis — form admin, API, dan CHECK constraint
`orders_shipped_has_tracking` di database. Satu resi tidak boleh dipakai dua pesanan pada kurir yang
sama (409). Pembeli melihat kurir & resi di halaman pesanannya.

**Penyedia** dipilih dari env (seperti pembayaran):

- `BITESHIP_API_KEY` diisi → **Biteship**. Tujuan dipilih lewat autocomplete area
  (`GET /api/shipping/areas`, Maps API `GET /v1/maps/areas`); `quoteOrder` meminta tarif langsung
  (`POST /v1/rates/couriers`, berat = Σ qty × `product_types.weight_g`) dari area asal
  `SHIP_ORIGIN_AREA_ID` ke area tujuan, dan **memvalidasi kurir pilihan** (`<kode>:<layanan>`, mis.
  `jne:reg`) terhadap tarif itu — harga/kurir kiriman browser tidak pernah dipercaya. Pesanan menyimpan
  `dest_area_id`, `weight_g`, `courier_company`, `courier_type`. Berat per jenis produk diubah admin
  di tab Keuangan (`PATCH /api/product-types/{id} {weightG}`).
- Admin `ship-biteship` (tombol "Buat pengiriman") memanggil `POST /v1/orders` dengan
  `reference_id` = nomor pesanan (idempoten), lalu mencatat resi yang dikembalikan lewat `shipOrder`
  (jalur yang sama dengan resi manual, jadi CHECK constraint tetap berlaku). `ship` manual tetap ada
  sebagai cadangan. Id order/pelacakan Biteship dan ongkos kurir sebenarnya disimpan di
  `orders.biteship_order_id`, `biteship_tracking_id`, `shipment_cost`.
- **Webhook** `POST /api/shipping/biteship/webhook`: webhook Biteship tidak bertanda tangan, jadi
  didaftarkan dengan header kustom `X-Webhook-Secret: <BITESHIP_WEBHOOK_SECRET>` (atau
  `…/webhook?secret=<…>`). Isi webhook tidak dipercaya: server mengambil ulang `GET /v1/trackings/{id}`
  lalu memanggil `markDelivered` bila `delivered` (idempoten — webhook ulang tidak mengubah apa pun).
  Admin juga bisa menarik status manual (`sync-shipment`).
- Kunci kosong (dev) → **flat**: tarif konstanta di `checkout.go`, kota diketik bebas, resi manual.
- Env: `BITESHIP_API_KEY` (`biteship_test.…` = sandbox), `BITESHIP_WEBHOOK_SECRET` (wajib bila kunci
  diisi), `BITESHIP_COURIERS` (opsional, default `jne,sicepat,anteraja`), `SHIP_ORIGIN_AREA_ID`,
  `SHIP_ORIGIN_NAME`, `SHIP_ORIGIN_PHONE`, `SHIP_ORIGIN_ADDRESS` (wajib bila kunci diisi),
  `SHIP_ORIGIN_POSTAL`, `SHIP_ORIGIN_NOTE` (opsional).
- **Keuangan**: pesanan yang dikirim lewat `ship-biteship` dibayar kurirnya oleh platform (saldo
  Biteship), jadi dalam transaksi yang sama dengan produksi → dikirim, `shipment_cost` dicatat dan
  tagihan vendor jenis `ongkir` yang belum dibayarkan dihapus (bila sudah dibayarkan: dibiarkan +
  peringatan di log, pengiriman tidak gagal). `GET /api/finance` menghitung `courierCosts` (jumlah
  `shipment_cost`) sebagai biaya platform — tab Keuangan: "Ongkir kurir (Biteship)" — sehingga margin
  ongkir = ongkir pembeli − `shipment_cost`. `ship` manual (vendor mengirim sendiri) tetap meneruskan
  ongkir pembeli ke vendor.

## Alur status pesanan

```
menunggu-pembayaran → dibayar → produksi → dikirim → tiba → selesai
        pay (pembeli)   advance    ship +resi   delivered   confirm (pembeli)
                        (admin)    (admin)      (kurir/admin)  atau OTOMATIS 2 hari setelah tiba
```

- **Admin tidak bisa menyelesaikan pesanan.** `tiba` hanya setelah laporan kurir bahwa paket sampai
  (prototipe: admin mencatatnya dari pelacakan kurir; integrasi kurir — webhook Biteship/KiriminAja —
  nanti memanggil `markDelivered` yang sama).
- `selesai` hanya lewat tombol **Pesanan diterima** oleh pemilik pesanan, atau otomatis oleh job
  `RunOrderJobs` (dicek saat API start dan tiap 10 menit) bila `delivered_at` lewat
  `AutoCompleteAfter` (48 jam). Database menolak status `tiba`/`selesai` tanpa `delivered_at`.

## Pembayaran (Midtrans)

- **Penyedia** dipilih dari env: `MIDTRANS_SERVER_KEY` diisi → Midtrans (sandbox, atau produksi bila
  `MIDTRANS_ENV=production`); kosong di dev → **mock** (pembayaran tiruan); kosong di produksi → **none**.
- **Alur**: `POST /orders/{id}/payment` membuat transaksi Snap (item + ongkir + diskon, nominal =
  total pesanan, masa berlaku = sisa batas bayar) → browser membuka popup Snap → Midtrans memberi
  tahu lewat webhook, dan server juga bisa menanyakan sendiri (`payment/sync`).
- **Keamanan**: pesanan HANYA lunas bila **Status API Midtrans** (dipanggil server dengan Server Key)
  menyatakan `settlement`/`capture`. Webhook diperiksa tanda tangannya (SHA512) lalu *dikonfirmasi
  ulang* ke Status API, jadi webhook palsu tidak berguna; nominal harus sama dengan total pesanan;
  tiap notifikasi dicatat di `payment_notifications`. Pembeli tidak bisa memanggil `pay` sendiri.
- **Idempoten**: notifikasi berulang tidak menggandakan royalti/counter terjual.
- **Percobaan ulang**: `order_id` Midtrans = `<no. pesanan>-<percobaan>`; bayar ulang setelah ditolak
  memakai percobaan berikutnya, notifikasi percobaan lama diabaikan.
- **Batas bayar 24 jam** (`UnpaidOrderTTL`): `expire` dari Midtrans atau job (`RunOrderJobs`)
  membatalkan pesanan (`dibatalkan`) dan membebaskan voucher. Pembayaran yang terlambat masuk
  setelah dibatalkan tetap diterima (pesanan aktif kembali).
- **Biaya**: ditentukan Midtrans per kanal (QRIS dan VA paling murah; e-wallet sekitar 2%). Matikan
  kanal yang tidak diperlukan di dashboard Midtrans atau lewat `MIDTRANS_ENABLED_PAYMENTS`.
- **Belum ada**: refund / chargeback (notifikasinya hanya dicatat) dan rekonsiliasi pencairan dana.
