-- 018: pengiriman lewat Biteship (ongkir langsung, resi otomatis, pelacakan).

-- Berat per pcs (gram) untuk menghitung ongkir; admin bisa mengubahnya di tab Keuangan.
ALTER TABLE product_types ADD COLUMN weight_g int NOT NULL DEFAULT 250 CHECK (weight_g > 0 AND weight_g <= 30000);
UPDATE product_types SET weight_g = CASE id
  WHEN 'kaos' THEN 200 WHEN 'hoodie' THEN 600 WHEN 'mug' THEN 400 WHEN 'totebag' THEN 150
  ELSE weight_g END;

-- Area tujuan (Biteship area id) tersimpan di profil dan pesanan; kota & kode pos tetap disimpan.
ALTER TABLE users ADD COLUMN area_id text NOT NULL DEFAULT '';

ALTER TABLE orders ADD COLUMN dest_area_id         text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN weight_g             int  NOT NULL DEFAULT 0;   -- berat paket saat checkout
ALTER TABLE orders ADD COLUMN courier_company      text NOT NULL DEFAULT '';  -- pilihan pembeli (kode Biteship, mis. jne); kosong = tarif flat dev
ALTER TABLE orders ADD COLUMN courier_type         text NOT NULL DEFAULT '';  -- layanan, mis. reg
ALTER TABLE orders ADD COLUMN biteship_order_id    text;                      -- terisi saat admin membuat pengiriman
ALTER TABLE orders ADD COLUMN biteship_tracking_id text;
ALTER TABLE orders ADD COLUMN courier_status       text;                      -- status pelacakan terakhir dari Biteship
ALTER TABLE orders ADD COLUMN shipment_cost        int CHECK (shipment_cost >= 0); -- ongkos kurir sebenarnya (dibayar platform lewat saldo Biteship)

CREATE UNIQUE INDEX orders_biteship_order_key ON orders (biteship_order_id) WHERE biteship_order_id IS NOT NULL;
