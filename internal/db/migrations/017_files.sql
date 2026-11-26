-- Object storage (feat/object-storage): one row per file uploaded to the bucket.
--
-- Browsers PUT straight to S3/R2 with a presigned URL, then POST /api/uploads/complete;
-- the server inspects the object (real type + pixel size) and records it here.
-- The public URL is never stored: it is FILES_PUBLIC_URL + '/' + key.
--
-- Numbered 017 because 016 belongs to feat/print-spec. Written to be re-runnable
-- (IF NOT EXISTS) in case the version bookkeeping differs between branches.

CREATE TABLE IF NOT EXISTS files (
  key        text PRIMARY KEY,                           -- <purpose>/<random>.<ext>, legacy/<old name> for migrated files
  mime       text NOT NULL,                              -- sniffed from the bytes, not the client's claim
  bytes      bigint NOT NULL CHECK (bytes > 0),
  width_px   int CHECK (width_px > 0),                   -- NULL for SVG
  height_px  int CHECK (height_px > 0),
  owner_id   text REFERENCES users(id) ON DELETE SET NULL,  -- NULL for migrated legacy files
  purpose    text NOT NULL,                              -- designs | custom | avatars | complaints | legacy
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((width_px IS NULL) = (height_px IS NULL))
);
CREATE INDEX IF NOT EXISTS files_owner_idx ON files (owner_id, created_at DESC);
