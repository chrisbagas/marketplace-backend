-- Pembayaran lewat gateway (Midtrans Snap): sesi pembayaran per pesanan,
-- log notifikasi, dan status "dibatalkan" untuk pesanan yang tidak dibayar
-- sampai batas waktu.
--
-- Constraint resi/tiba (010, 011) ditulis ulang dengan status::text: nilai enum
-- baru ('dibatalkan') tidak boleh dipakai di transaksi yang sama dengan ADD VALUE,
-- dan constraint lama mendaftar status "belum dikirim" sehingga akan ikut
-- menolak 'dibatalkan'. Versi baru mendaftar status yang BUTUH data pengiriman.

ALTER TABLE orders DROP CONSTRAINT orders_shipped_has_tracking;
ALTER TABLE orders ADD CONSTRAINT orders_shipped_has_tracking CHECK (
  status::text NOT IN ('dikirim', 'tiba', 'selesai')
  OR (tracking_number IS NOT NULL AND shipped_courier IS NOT NULL AND shipped_at IS NOT NULL)
);
ALTER TABLE orders DROP CONSTRAINT orders_delivered_has_time;
ALTER TABLE orders ADD CONSTRAINT orders_delivered_has_time CHECK (
  status::text NOT IN ('tiba', 'selesai') OR delivered_at IS NOT NULL
);

ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'dibatalkan';
ALTER TABLE orders ADD COLUMN cancelled_at timestamptz;

-- sesi pembayaran di gateway. gateway_order_id = "<no. pesanan>-<percobaan>":
-- Midtrans menolak order_id yang sama dipakai dua kali, jadi percobaan bayar
-- ulang (mis. setelah ditolak) memakai nomor percobaan berikutnya.
ALTER TABLE payments ADD COLUMN gateway          text NOT NULL DEFAULT 'mock';   -- mock | midtrans | manual
ALTER TABLE payments ADD COLUMN gateway_order_id text;
ALTER TABLE payments ADD COLUMN attempt          int  NOT NULL DEFAULT 1;
ALTER TABLE payments ADD COLUMN snap_token       text;
ALTER TABLE payments ADD COLUMN snap_expires_at  timestamptz;
ALTER TABLE payments ADD COLUMN payment_type     text;                          -- payment_type dari gateway
CREATE UNIQUE INDEX payments_gateway_order_key ON payments (gateway_order_id) WHERE gateway_order_id IS NOT NULL;

-- jejak setiap notifikasi/cek status dari gateway (audit & debug sengketa)
CREATE TABLE payment_notifications (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id           text REFERENCES orders(id) ON DELETE CASCADE,
  gateway_order_id   text NOT NULL,
  source             text NOT NULL,                 -- webhook | sync
  transaction_status text NOT NULL,
  payment_type       text,
  gross_amount       int,
  signature_ok       boolean NOT NULL,
  applied            text NOT NULL DEFAULT '',      -- hasil: paid | pending | failed | expired | ignored: <alasan>
  received_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_notifications_order_idx ON payment_notifications (order_id, received_at DESC);
