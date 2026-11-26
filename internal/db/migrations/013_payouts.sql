-- Penarikan saldo kreator.
--
-- Royalti dibukukan saat pesanan dibayar, tapi baru BISA DITARIK setelah
-- pesanan selesai (pembeli konfirmasi / selesai otomatis). Saldo tidak
-- disimpan: dihitung dari royalties ⨝ orders (status selesai) dikurangi
-- payouts yang tidak ditolak, jadi tidak bisa melenceng dari data pesanan.
--
-- Pembayaran ke kreator masih transfer manual oleh admin:
-- diminta → diproses → dibayar (dengan referensi transfer), atau ditolak
-- (dana kembali ke saldo).
--
-- Catatan: nilai enum baru tidak boleh dipakai di transaksi yang sama dengan
-- ADD VALUE, jadi index di bawah ditulis tanpa menyebut 'ditolak'.

ALTER TYPE payout_status ADD VALUE IF NOT EXISTS 'ditolak';

-- rekening tujuan kreator
ALTER TABLE designers ADD COLUMN bank_name    text;
ALTER TABLE designers ADD COLUMN bank_account text;
ALTER TABLE designers ADD COLUMN bank_holder  text;

-- salinan rekening saat pengajuan (rekening bisa diubah setelahnya) + data proses admin
ALTER TABLE payouts ADD COLUMN bank_name    text;
ALTER TABLE payouts ADD COLUMN bank_account text;
ALTER TABLE payouts ADD COLUMN bank_holder  text;
ALTER TABLE payouts ADD COLUMN reference    text;        -- no. referensi transfer bank
ALTER TABLE payouts ADD COLUMN note         text;        -- alasan penolakan / catatan admin
ALTER TABLE payouts ADD COLUMN processed_at timestamptz; -- terakhir diubah admin
ALTER TABLE payouts ADD CONSTRAINT payouts_amount_positive CHECK (amount > 0);

-- satu pengajuan terbuka per kreator
CREATE UNIQUE INDEX payouts_one_open_idx ON payouts (designer_id) WHERE status IN ('diminta', 'diproses');
CREATE INDEX payouts_status_idx ON payouts (status, requested_at);
