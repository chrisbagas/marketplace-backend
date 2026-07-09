-- KaryaKita — skema database PostgreSQL
-- Konvensi: snake_case, uang dalam Rupiah (integer), waktu timestamptz.

-- ===== Enum =====
CREATE TYPE user_role         AS ENUM ('customer', 'designer', 'admin');
CREATE TYPE order_status      AS ENUM ('menunggu-pembayaran', 'dibayar', 'produksi', 'dikirim', 'selesai');
CREATE TYPE payment_status    AS ENUM ('pending', 'paid', 'expired', 'failed');
CREATE TYPE submission_status AS ENUM ('review', 'disetujui', 'ditolak');
CREATE TYPE payout_status     AS ENUM ('diminta', 'diproses', 'dibayar');
CREATE TYPE vital_name        AS ENUM ('LCP', 'INP', 'CLS', 'TTFB');

-- ===== Identitas =====
CREATE TABLE users (
  id         text PRIMARY KEY,
  email      text NOT NULL UNIQUE,
  name       text NOT NULL,
  role       user_role NOT NULL DEFAULT 'customer',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE designers (
  id            text PRIMARY KEY,
  user_id       text UNIQUE REFERENCES users(id),
  name          text NOT NULL,
  city          text NOT NULL,
  bio           text NOT NULL DEFAULT '',
  hue           int  NOT NULL DEFAULT 0,          -- warna avatar di UI
  followers     int  NOT NULL DEFAULT 0,
  rating        numeric(2,1) NOT NULL DEFAULT 0,
  royalty_share numeric(4,3) NOT NULL DEFAULT 0.120,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- ===== Katalog =====
CREATE TABLE product_types (
  id             text PRIMARY KEY,                -- kaos | hoodie | mug | totebag
  label          text NOT NULL,
  base_price     int  NOT NULL,                   -- harga dasar produksi
  print_max_w_cm numeric(5,1) NOT NULL,           -- area cetak fisik
  print_max_h_cm numeric(5,1) NOT NULL
);

CREATE TABLE colors (
  id    text PRIMARY KEY,                          -- putih | hitam | ...
  label text NOT NULL,
  hex   text NOT NULL
);

CREATE TABLE product_type_colors (
  product_type_id text NOT NULL REFERENCES product_types(id) ON DELETE CASCADE,
  color_id        text NOT NULL REFERENCES colors(id) ON DELETE CASCADE,
  sort            int  NOT NULL DEFAULT 0,
  PRIMARY KEY (product_type_id, color_id)
);

CREATE TABLE product_type_sizes (
  product_type_id text NOT NULL REFERENCES product_types(id) ON DELETE CASCADE,
  size            text NOT NULL,
  sort            int  NOT NULL DEFAULT 0,
  PRIMARY KEY (product_type_id, size)
);

CREATE TABLE designs (
  id          text PRIMARY KEY,
  designer_id text NOT NULL REFERENCES designers(id),
  title       text NOT NULL,
  uri         text NOT NULL,                       -- data-URI (prototipe); URL object storage (produksi)
  tags        text[] NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE listings (
  id              text PRIMARY KEY,
  design_id       text NOT NULL REFERENCES designs(id),
  product_type_id text NOT NULL REFERENCES product_types(id),
  price           int  NOT NULL,
  sold            int  NOT NULL DEFAULT 0,
  rating          numeric(2,1) NOT NULL DEFAULT 0,
  badge           text,
  active          boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listings_type_idx ON listings (product_type_id);
CREATE INDEX listings_sold_idx ON listings (sold DESC);

-- ===== Transaksi =====
CREATE TABLE orders (
  id            text PRIMARY KEY,                  -- KK-YYMMDD-nnnn
  user_id       text REFERENCES users(id),         -- NULL = guest checkout
  cust_name     text NOT NULL,
  cust_email    text NOT NULL DEFAULT '',
  cust_phone    text NOT NULL DEFAULT '',
  cust_address  text NOT NULL DEFAULT '',
  cust_city     text NOT NULL DEFAULT '',
  subtotal      int  NOT NULL,
  courier       text NOT NULL,
  shipping_cost int  NOT NULL,
  total         int  NOT NULL,
  status        order_status NOT NULL DEFAULT 'menunggu-pembayaran',
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_created_idx ON orders (created_at DESC);

CREATE TABLE order_items (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id   text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  listing_id text REFERENCES listings(id),
  title      text NOT NULL,                        -- snapshot saat pembelian
  type       text NOT NULL,
  color      text NOT NULL,
  size       text NOT NULL,
  qty        int  NOT NULL CHECK (qty > 0),
  unit_price int  NOT NULL,
  design_uri text
);
CREATE INDEX order_items_order_idx ON order_items (order_id);

CREATE TABLE payments (
  id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id text NOT NULL UNIQUE REFERENCES orders(id) ON DELETE CASCADE,
  method   text NOT NULL DEFAULT 'belum dipilih',  -- QRIS | GoPay | VA BCA | ...
  status   payment_status NOT NULL DEFAULT 'pending',
  ref      text NOT NULL,                          -- ref gateway (mis. Midtrans order ref)
  amount   int  NOT NULL,
  paid_at  timestamptz
);

CREATE TABLE order_events (
  id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  label    text NOT NULL,                          -- teks timeline utk pelanggan
  at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_events_order_idx ON order_events (order_id, at);

-- ===== Kreator =====
CREATE TABLE design_submissions (
  id              text PRIMARY KEY,                -- ds-xxxxxx
  designer_id     text REFERENCES designers(id),   -- NULL bila nama tak dikenal
  designer_name   text NOT NULL,
  title           text NOT NULL,
  product_type_id text NOT NULL REFERENCES product_types(id),
  color_id        text NOT NULL REFERENCES colors(id),
  price           int  NOT NULL,
  uri             text NOT NULL,
  status          submission_status NOT NULL DEFAULT 'review',
  note            text,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX design_submissions_status_idx ON design_submissions (status);

CREATE TABLE royalties (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  designer_id   text NOT NULL REFERENCES designers(id),
  order_item_id bigint NOT NULL UNIQUE REFERENCES order_items(id),
  amount        int  NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payouts (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  designer_id  text NOT NULL REFERENCES designers(id),
  amount       int  NOT NULL,
  status       payout_status NOT NULL DEFAULT 'diminta',
  requested_at timestamptz NOT NULL DEFAULT now(),
  paid_at      timestamptz
);

-- ===== Analitik & monitoring =====
CREATE TABLE track_events (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  session_id text NOT NULL,                        -- id sesi anonim per-tab
  type       text NOT NULL,                        -- page_view | view_product | search | ...
  page       text,
  label      text,
  device     text,                                 -- mobile | desktop
  value      bigint,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX track_events_created_idx ON track_events (created_at DESC);
CREATE INDEX track_events_type_idx ON track_events (type, created_at DESC);

CREATE TABLE web_vitals (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name       vital_name NOT NULL,
  value      double precision NOT NULL,            -- ms (LCP/INP/TTFB) atau skor (CLS)
  page       text NOT NULL DEFAULT '?',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX web_vitals_name_idx ON web_vitals (name);

CREATE TABLE api_metrics (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  route      text NOT NULL,                        -- pola route, mis. /api/orders
  method     text NOT NULL DEFAULT '',
  status     int  NOT NULL DEFAULT 200,
  ms         double precision NOT NULL,
  ok         boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_metrics_created_idx ON api_metrics (created_at DESC);

CREATE TABLE search_terms (
  term  text PRIMARY KEY,
  count int NOT NULL DEFAULT 1
);

CREATE TABLE day_stats (
  date          date PRIMARY KEY,
  visits        int    NOT NULL DEFAULT 0,
  product_views int    NOT NULL DEFAULT 0,
  add_to_cart   int    NOT NULL DEFAULT 0,
  checkout      int    NOT NULL DEFAULT 0,
  paid          int    NOT NULL DEFAULT 0,
  revenue       bigint NOT NULL DEFAULT 0
);
