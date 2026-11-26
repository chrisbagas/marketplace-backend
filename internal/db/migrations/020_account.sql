-- Pengaturan akun: ganti email lewat tautan verifikasi ke alamat baru, dan
-- hapus akun (anonimisasi, bukan DELETE — pesanan tetap ada untuk pembukuan).
--
-- users.deleted_at   : tanda akun sudah dianonimkan. Baris tetap ada supaya
--                      orders.user_id / designers.user_id tidak putus; email,
--                      username, nama, kredensial sudah diganti tombstone.
-- auth_tokens.payload: data yang ikut terikat pada token. Untuk 'change_email'
--                      berisi email baru, jadi alamat tujuan tidak bisa diganti
--                      setelah link dikirim.
--
-- Catatan: nilai enum baru tidak boleh dipakai di transaksi yang sama dengan
-- ADD VALUE; migrasi ini hanya menambahkannya.

ALTER TABLE users ADD COLUMN deleted_at timestamptz;

ALTER TYPE auth_token_purpose ADD VALUE IF NOT EXISTS 'change_email';
ALTER TYPE auth_token_purpose ADD VALUE IF NOT EXISTS 'delete_account';

ALTER TABLE auth_tokens ADD COLUMN payload text NOT NULL DEFAULT '';
