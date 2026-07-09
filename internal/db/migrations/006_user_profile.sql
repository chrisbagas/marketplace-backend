-- Profil pelanggan (belum ada login — sesi diperlakukan sebagai user demo)
-- + desain custom pribadi: dipakai/dibeli sendiri, TIDAK dijual di katalog,
-- tanpa royalti (order_items.listing_id NULL sehingga tak pernah masuk
-- perhitungan royalti kreator).

ALTER TABLE users ADD COLUMN phone             text  NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN address           text  NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN city              text  NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN preferred_payment text  NOT NULL DEFAULT '';   -- QRIS | GoPay | ...
ALTER TABLE users ADD COLUMN preferred_courier text  NOT NULL DEFAULT '';   -- SiCepat REG | ...
ALTER TABLE users ADD COLUMN settings          jsonb NOT NULL DEFAULT '{}'; -- preferensi situs

CREATE TABLE user_designs (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id         text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title           text NOT NULL,
  product_type_id text NOT NULL REFERENCES product_types(id),
  color_id        text NOT NULL REFERENCES colors(id),
  uri             text NOT NULL,                   -- /uploads/... atau data-URI
  width_cm        numeric(5,1) NOT NULL DEFAULT 24,
  offset_y_cm     numeric(5,1) NOT NULL DEFAULT 0,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_designs_user_idx ON user_designs (user_id, created_at DESC);
