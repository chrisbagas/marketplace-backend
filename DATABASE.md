# KaryaKita — Skema Database (PostgreSQL)

Database: **PostgreSQL 16** · skema penuh di [`internal/db/schema.sql`](internal/db/schema.sql)
(dimigrasikan otomatis saat API pertama kali start, dicatat di `schema_migrations`).

20 tabel dalam 5 kelompok:

| Kelompok | Tabel |
|---|---|
| **Identitas** | `users`, `designers` |
| **Katalog** | `product_types`, `colors`, `product_type_colors`, `product_type_sizes`, `designs`, `listings` |
| **Transaksi** | `orders`, `order_items`, `payments`, `order_events` |
| **Kreator** | `design_submissions`, `royalties`, `payouts` |
| **Analitik** | `track_events`, `web_vitals`, `api_metrics`, `search_terms`, `day_stats` |

## Diagram ERD

```mermaid
erDiagram
    users ||--o| designers : "profil kreator"
    users ||--o{ orders : "pesanan (nullable, guest ok)"

    designers ||--o{ designs : "membuat"
    designers ||--o{ design_submissions : "mengajukan"
    designers ||--o{ royalties : "menerima"
    designers ||--o{ payouts : "menarik saldo"

    product_types ||--o{ product_type_colors : ""
    colors ||--o{ product_type_colors : ""
    product_types ||--o{ product_type_sizes : ""
    product_types ||--o{ listings : ""
    product_types ||--o{ design_submissions : ""
    colors ||--o{ design_submissions : ""

    designs ||--o{ listings : "dijual sebagai"
    listings ||--o{ order_items : "dibeli via"

    orders ||--o{ order_items : "berisi"
    orders ||--|| payments : "dibayar via"
    orders ||--o{ order_events : "timeline"
    order_items ||--o| royalties : "menghasilkan"

    users {
        text id PK
        text email UK
        text name
        user_role role "customer | designer | admin"
        timestamptz created_at
    }
    designers {
        text id PK
        text user_id FK "nullable"
        text name
        text city
        text bio
        int hue
        int followers
        numeric rating
        numeric royalty_share "default 0.12"
    }
    product_types {
        text id PK "kaos | hoodie | mug | totebag"
        text label
        int base_price "Rupiah"
        numeric print_max_w_cm
        numeric print_max_h_cm
    }
    colors {
        text id PK
        text label
        text hex
    }
    product_type_colors {
        text product_type_id PK,FK
        text color_id PK,FK
        int sort
    }
    product_type_sizes {
        text product_type_id PK,FK
        text size PK
        int sort
    }
    designs {
        text id PK
        text designer_id FK
        text title
        text uri "data-URI / URL storage"
        text_arr tags
        timestamptz created_at
    }
    listings {
        text id PK
        text design_id FK
        text product_type_id FK
        int price
        int sold
        numeric rating
        text badge "nullable"
        bool active
    }
    orders {
        text id PK "KK-YYMMDD-nnnn"
        text user_id FK "nullable (guest)"
        text cust_name
        text cust_email
        text cust_phone
        text cust_address
        text cust_city
        int subtotal
        text courier
        int shipping_cost
        int total
        order_status status "menunggu-pembayaran → selesai"
        timestamptz created_at
    }
    order_items {
        bigint id PK
        text order_id FK
        text listing_id FK "nullable, snapshot tetap ada"
        text title "snapshot"
        text type
        text color
        text size
        int qty
        int unit_price
        text design_uri "snapshot"
    }
    payments {
        bigint id PK
        text order_id FK,UK
        text method "QRIS | GoPay | VA BCA | ..."
        payment_status status "pending | paid | expired | failed"
        text ref "ref gateway"
        int amount
        timestamptz paid_at "nullable"
    }
    order_events {
        bigint id PK
        text order_id FK
        text label "teks timeline"
        timestamptz at
    }
    design_submissions {
        text id PK "ds-xxxxxx"
        text designer_id FK "nullable"
        text designer_name
        text title
        text product_type_id FK
        text color_id FK
        int price
        text uri
        submission_status status "review | disetujui | ditolak"
        text note "catatan moderasi"
        timestamptz created_at
    }
    royalties {
        bigint id PK
        text designer_id FK
        bigint order_item_id FK,UK
        int amount "royalty_share x nilai item"
        timestamptz created_at
    }
    payouts {
        bigint id PK
        text designer_id FK
        int amount
        payout_status status "diminta | diproses | dibayar"
        timestamptz requested_at
        timestamptz paid_at "nullable"
    }
    track_events {
        bigint id PK
        text session_id "sesi anonim per-tab"
        text type "page_view | search | purchase | ..."
        text page
        text label
        text device
        bigint value
        timestamptz created_at
    }
    web_vitals {
        bigint id PK
        vital_name name "LCP | INP | CLS | TTFB"
        double value
        text page
        timestamptz created_at
    }
    api_metrics {
        bigint id PK
        text route
        text method
        int status
        double ms
        bool ok
        timestamptz created_at
    }
    search_terms {
        text term PK
        int count
    }
    day_stats {
        date date PK
        int visits
        int product_views
        int add_to_cart
        int checkout
        int paid
        bigint revenue
    }
```

> Tabel analitik (`track_events`, `web_vitals`, `api_metrics`, `search_terms`, `day_stats`)
> sengaja **tanpa foreign key** ke tabel transaksi — append-only, anonim, dan bisa dipindah ke
> warehouse terpisah tanpa memutus relasi.

## Keputusan desain

- **Uang = integer Rupiah.** Tidak ada pecahan sen di IDR; menghindari error floating point.
- **Snapshot di `order_items`.** Judul, harga, dan gambar desain disalin saat checkout, jadi
  pesanan lama tetap benar walau listing diubah/dihapus (`listing_id` nullable `ON DELETE` aman).
- **`payments` terpisah dari `orders`.** Siap untuk retry pembayaran/metode ganda, dan meniru
  webhook gateway: `PATCH {action:"pay"}` = notifikasi Midtrans/Xendit. Idempoten — bayar dua
  kali tidak menggandakan royalti (`ON CONFLICT (order_item_id) DO NOTHING`).
- **Royalti dibukukan saat pembayaran**, dihitung dari `designers.royalty_share` (default 12%),
  satu baris per `order_item` — auditable, tinggal `SUM` untuk saldo kreator.
- **Guest checkout.** `orders.user_id` nullable; saat auth ditambahkan, kolom sudah siap.
- **Enum PostgreSQL** untuk semua status — nilai tak valid ditolak di level database.
- **`day_stats`** = agregat harian (di-seed untuk demo, di produksi diisi job harian dari
  `track_events`); statistik hari berjalan digabung live dari `track_events` di `GET /api/stats`.

## Cara pakai

```bash
docker compose up -d          # Postgres 16 di :5432 (user/db: karyakita)
go run ./cmd/api              # migrasi + seed otomatis, API di :8081
```

Reset total: `docker compose down -v` lalu jalankan ulang API.

Koneksi manual: `docker exec -it karyakita-db psql -U karyakita -d karyakita`
