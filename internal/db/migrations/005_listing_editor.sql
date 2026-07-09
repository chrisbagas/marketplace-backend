-- Kreator boleh mengubah listing-nya: nama tampilan, harga, subset warna,
-- dan aktif/nonaktif. Desainnya sendiri tidak bisa diubah (ajukan ulang
-- lewat Studio bila ingin desain baru).

ALTER TABLE listings ADD COLUMN title_override text;   -- NULL = pakai "<jenis> <judul desain>"
ALTER TABLE listings ADD COLUMN color_ids text[];      -- NULL/kosong = semua warna jenis produk
