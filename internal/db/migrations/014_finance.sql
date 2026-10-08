-- Pembagian uang per pesanan: produsen, kreator, gateway, platform.
--
-- Semua uang pembeli masuk ke rekening platform (Midtrans → bank). Saat
-- pesanan dibayar, setiap item dibukukan ke pihak yang berhak:
--   kreator  = (harga jual − harga dasar) × qty           → royalties
--   produsen = biaya produksi × qty, + ongkir (produsen yang mengirim) → producer_charges
--   gateway  = estimasi biaya Midtrans                     → payments.gateway_fee
--   platform = sisanya (harga dasar − biaya produksi, biaya custom, dikurangi diskon & biaya gateway)
--
-- Harga dasar & biaya produksi disalin ke order_items saat checkout, jadi
-- mengubah tarif vendor tidak mengubah pesanan lama.

CREATE TABLE producers (
  id           text PRIMARY KEY,
  name         text NOT NULL,
  contact      text,                 -- nama PIC / no. WA
  bank_name    text,
  bank_account text,
  bank_holder  text,
  active       boolean NOT NULL DEFAULT true,
  created_at   timestamptz NOT NULL DEFAULT now()
);
INSERT INTO producers (id, name) VALUES ('vendor-utama', 'Vendor cetak utama');

-- tarif vendor per jenis produk (diisi admin; seed memberi angka contoh)
ALTER TABLE product_types ADD COLUMN producer_id     text REFERENCES producers(id);
ALTER TABLE product_types ADD COLUMN production_cost int NOT NULL DEFAULT 0;
ALTER TABLE product_types ADD CONSTRAINT product_types_cost_le_base
  CHECK (production_cost >= 0 AND production_cost <= base_price);
-- angka CONTOH sampai ada penawaran vendor sungguhan (sama dengan seed.go)
UPDATE product_types SET producer_id = 'vendor-utama',
  production_cost = LEAST(base_price, CASE id WHEN 'kaos' THEN 55000 WHEN 'hoodie' THEN 150000
                                              WHEN 'mug' THEN 35000 WHEN 'totebag' THEN 45000 ELSE 0 END);

-- salinan harga saat checkout (NULL = pesanan lama sebelum migrasi ini)
ALTER TABLE order_items ADD COLUMN base_price      int;
ALTER TABLE order_items ADD COLUMN production_cost int;
ALTER TABLE order_items ADD COLUMN producer_id     text REFERENCES producers(id);
UPDATE order_items oi SET base_price = pt.base_price, production_cost = pt.production_cost, producer_id = pt.producer_id
FROM product_types pt WHERE pt.id = oi.type AND oi.base_price IS NULL;

-- tagihan vendor: satu baris biaya produksi per item + satu baris ongkir per pesanan
CREATE TABLE producer_payouts (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  producer_id text NOT NULL REFERENCES producers(id),
  amount      int  NOT NULL CHECK (amount > 0),
  reference   text NOT NULL,         -- no. referensi transfer
  note        text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE producer_charges (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  producer_id   text NOT NULL REFERENCES producers(id),
  order_id      text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  order_item_id bigint REFERENCES order_items(id) ON DELETE CASCADE, -- NULL = ongkir
  kind          text NOT NULL CHECK (kind IN ('produksi', 'ongkir')),
  amount        int  NOT NULL CHECK (amount >= 0),
  payout_id     bigint REFERENCES producer_payouts(id),             -- NULL = belum dibayar
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'produksi') = (order_item_id IS NOT NULL))
);
-- idempoten: notifikasi pembayaran berulang tidak menggandakan tagihan
CREATE UNIQUE INDEX producer_charges_item_idx ON producer_charges (order_item_id) WHERE kind = 'produksi';
CREATE UNIQUE INDEX producer_charges_ship_idx ON producer_charges (order_id) WHERE kind = 'ongkir';
CREATE INDEX producer_charges_open_idx ON producer_charges (producer_id) WHERE payout_id IS NULL;

-- estimasi biaya gateway (tarif di internal/db/ledger.go — cocokkan dengan kontrak Midtrans)
ALTER TABLE payments ADD COLUMN gateway_fee int NOT NULL DEFAULT 0;

-- pesanan lama yang sudah lunas: buat tagihan vendornya sekali (logika sama dengan db.BookPaidOrder)
INSERT INTO producer_charges (producer_id, order_id, order_item_id, kind, amount, created_at)
SELECT oi.producer_id, oi.order_id, oi.id, 'produksi', oi.production_cost * oi.qty, p.paid_at
FROM order_items oi JOIN payments p ON p.order_id = oi.order_id
WHERE p.status = 'paid' AND oi.producer_id IS NOT NULL;
INSERT INTO producer_charges (producer_id, order_id, kind, amount, created_at)
SELECT (SELECT oi.producer_id FROM order_items oi WHERE oi.order_id = o.id ORDER BY oi.id LIMIT 1), o.id, 'ongkir', o.shipping_cost, p.paid_at
FROM orders o JOIN payments p ON p.order_id = o.id
WHERE p.status = 'paid' AND o.shipping_cost > 0
  AND (SELECT oi.producer_id FROM order_items oi WHERE oi.order_id = o.id ORDER BY oi.id LIMIT 1) IS NOT NULL;
-- estimasi biaya gateway pesanan lama (tarif sama dengan db.GatewayFee)
UPDATE payments SET gateway_fee = CASE
    WHEN lower(method || ' ' || COALESCE(payment_type, '') || ' ' || COALESCE(gateway, '')) LIKE '%manual%' THEN 0
    WHEN lower(method || ' ' || COALESCE(payment_type, '')) LIKE '%qris%' THEN round(amount * 0.007)
    WHEN lower(method || ' ' || COALESCE(payment_type, '')) ~ '(^va|bank_transfer|echannel| va )' THEN 4000
    ELSE round(amount * 0.02) END
WHERE status = 'paid';

-- royalti kini = selisih harga jual di atas harga dasar, bukan persen
ALTER TABLE designers DROP COLUMN royalty_share;
