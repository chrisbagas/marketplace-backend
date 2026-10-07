-- Autentikasi: login username/email + password, sesi berbasis cookie,
-- dan kolom untuk login Google (OAuth/OIDC).
--
-- password_hash NULL = akun hanya bisa masuk lewat Google.
-- google_sub     NULL = akun belum ditautkan ke Google.

ALTER TABLE users ADD COLUMN username       text;
ALTER TABLE users ADD COLUMN password_hash  text;                  -- bcrypt
ALTER TABLE users ADD COLUMN google_sub     text UNIQUE;           -- klaim "sub" dari Google
ALTER TABLE users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN last_login_at  timestamptz;

-- akun demo yang sudah ada mendapat username; password di-set oleh API saat start (non-produksi)
UPDATE users SET username = split_part(id, '-', 2) WHERE username IS NULL AND id LIKE 'u-%';
UPDATE users SET username = id WHERE username IS NULL;

ALTER TABLE users ALTER COLUMN username SET NOT NULL;
-- unik tanpa membedakan huruf besar/kecil
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));
CREATE UNIQUE INDEX users_email_lower_key    ON users (lower(email));

-- Token sesi tidak disimpan mentah: id = sha256(token) dalam hex.
CREATE TABLE sessions (
  id           text PRIMARY KEY,
  user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  user_agent   text NOT NULL DEFAULT '',
  ip           text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx    ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
