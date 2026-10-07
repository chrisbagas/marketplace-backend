-- Checkout: voucher, detail pengiriman tambahan, dan indeks pesanan per akun.
--
-- Harga, ongkir, dan diskon selalu dihitung ulang di server saat checkout
-- (lihat internal/api/checkout.go) — nilai dari browser tidak dipercaya.

CREATE TYPE voucher_kind AS ENUM ('percent', 'fixed', 'shipping');

CREATE TABLE vouchers (
  code           text PRIMARY KEY,                  -- selalu huruf besar, mis. KARYAKITA10
  description    text NOT NULL DEFAULT '',
  kind           voucher_kind NOT NULL,             -- percent: % subtotal · fixed: potongan Rp · shipping: subsidi ongkir
  value          int  NOT NULL CHECK (value > 0),   -- percent: 1–100 · fixed/shipping: Rupiah
  min_subtotal   int  NOT NULL DEFAULT 0,
  max_discount   int,                               -- batas potongan untuk percent (NULL = tanpa batas)
  starts_at      timestamptz,                       -- NULL = langsung berlaku
  ends_at        timestamptz,                       -- NULL = tanpa kedaluwarsa
  usage_limit    int,                               -- kuota total (NULL = tanpa batas)
  per_user_limit int  NOT NULL DEFAULT 1,
  active         boolean NOT NULL DEFAULT true,
  created_at     timestamptz NOT NULL DEFAULT now(),
  CHECK (kind <> 'percent' OR value <= 100),
  CHECK (code = upper(code))
);

-- Diskon ditanggung platform: royalti kreator tetap dihitung dari harga item.
ALTER TABLE orders ADD COLUMN voucher_code text REFERENCES vouchers(code);
ALTER TABLE orders ADD COLUMN discount     int  NOT NULL DEFAULT 0 CHECK (discount >= 0);
ALTER TABLE orders ADD COLUMN cust_postal  text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN notes        text NOT NULL DEFAULT '';   -- catatan untuk penjual/kurir

ALTER TABLE users ADD COLUMN postal_code text NOT NULL DEFAULT '';

CREATE INDEX orders_user_idx    ON orders (user_id, created_at DESC);
CREATE INDEX orders_voucher_idx ON orders (voucher_code, user_id) WHERE voucher_code IS NOT NULL;
CREATE INDEX order_items_listing_idx ON order_items (listing_id);

-- Voucher demo
INSERT INTO vouchers (code, description, kind, value, min_subtotal, max_discount, usage_limit, per_user_limit) VALUES
  ('KARYAKITA10',  'Diskon 10% (maks. Rp50.000) untuk belanja min. Rp100.000', 'percent',  10,    100000, 50000, NULL, 3),
  ('HEMAT25',      'Potongan Rp25.000 untuk belanja min. Rp150.000',           'fixed',    25000, 150000, NULL,  500,  1),
  ('GRATISONGKIR', 'Gratis ongkir hingga Rp20.000',                            'shipping', 20000, 0,      NULL,  NULL, 5)
ON CONFLICT (code) DO NOTHING;
