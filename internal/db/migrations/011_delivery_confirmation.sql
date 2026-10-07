-- Paket tiba & konfirmasi pembeli.
--
-- Alur: dikirim → tiba (laporan kurir bahwa paket sampai) → selesai
-- (pembeli menekan "Pesanan diterima", atau otomatis 2 hari setelah tiba).
-- Admin TIDAK bisa menyelesaikan pesanan.
--
-- Catatan: nilai enum baru tidak boleh dipakai di transaksi yang sama dengan
-- ADD VALUE, jadi constraint di bawah ditulis tanpa menyebut 'tiba'.

ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'tiba' BEFORE 'selesai';

ALTER TABLE orders ADD COLUMN delivered_at timestamptz;  -- kurir melaporkan paket sampai
ALTER TABLE orders ADD COLUMN completed_at timestamptz;  -- pembeli konfirmasi / selesai otomatis

-- data lama yang sudah selesai: anggap tiba sehari setelah dikirim
UPDATE orders
SET delivered_at = COALESCE(shipped_at, created_at) + interval '1 day',
    completed_at = COALESCE(shipped_at, created_at) + interval '2 days'
WHERE status = 'selesai' AND delivered_at IS NULL;

-- aturan resi (010) diperluas agar ikut berlaku untuk status tiba
ALTER TABLE orders DROP CONSTRAINT orders_shipped_has_tracking;
ALTER TABLE orders ADD CONSTRAINT orders_shipped_has_tracking CHECK (
  status IN ('menunggu-pembayaran', 'dibayar', 'produksi')
  OR (tracking_number IS NOT NULL AND shipped_courier IS NOT NULL AND shipped_at IS NOT NULL)
);

-- status tiba/selesai wajib punya waktu tiba (= sudah ada laporan kurir)
ALTER TABLE orders ADD CONSTRAINT orders_delivered_has_time CHECK (
  status IN ('menunggu-pembayaran', 'dibayar', 'produksi', 'dikirim') OR delivered_at IS NOT NULL
);

-- dipakai job selesai-otomatis
CREATE INDEX orders_awaiting_confirm_idx ON orders (delivered_at) WHERE completed_at IS NULL AND delivered_at IS NOT NULL;
