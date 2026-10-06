-- Token sekali pakai untuk verifikasi email & reset password.
-- Sama seperti sessions: hanya sha256(token) yang disimpan; token mentah
-- hanya ada di link email.

CREATE TYPE auth_token_purpose AS ENUM ('verify_email', 'reset_password');

CREATE TABLE auth_tokens (
  id         text PRIMARY KEY,                      -- sha256(token) hex
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose    auth_token_purpose NOT NULL,
  expires_at timestamptz NOT NULL,                  -- verifikasi 24 jam, reset 30 menit
  used_at    timestamptz,                           -- NULL = belum dipakai
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_tokens_user_idx ON auth_tokens (user_id, purpose);
