-- Pengiriman: pesanan tidak bisa berstatus "dikirim"/"selesai" tanpa data
-- dari kurir (nama kurir + nomor resi). Ditegakkan oleh CHECK di database,
-- bukan hanya oleh form admin.

ALTER TABLE orders ADD COLUMN tracking_number text;        -- nomor resi dari kurir
ALTER TABLE orders ADD COLUMN shipped_courier text;        -- kurir yang benar-benar mengirim
ALTER TABLE orders ADD COLUMN shipped_at      timestamptz;

-- data demo lama yang sudah "dikirim"/"selesai" diberi resi contoh
UPDATE orders
SET tracking_number = 'DEMO' || replace(substr(id, 4), '-', ''),
    shipped_courier = courier,
    shipped_at      = created_at + interval '1 day'
WHERE status IN ('dikirim', 'selesai') AND tracking_number IS NULL;

ALTER TABLE orders ADD CONSTRAINT orders_shipped_has_tracking CHECK (
  status NOT IN ('dikirim', 'selesai')
  OR (tracking_number IS NOT NULL AND shipped_courier IS NOT NULL AND shipped_at IS NOT NULL)
);

-- satu resi tidak boleh dipakai dua pesanan pada kurir yang sama (cegah salah ketik/salin)
CREATE UNIQUE INDEX orders_tracking_key ON orders (lower(shipped_courier), tracking_number)
  WHERE tracking_number IS NOT NULL;
