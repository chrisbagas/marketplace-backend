-- Reports of suspected IP infringement / inappropriate designs.
--
-- Anyone (also logged out) can report a listing; an admin reviews the report
-- and either takes the listing down (listings.active = false, creator is
-- emailed) or rejects the report with a note. Status is text + CHECK rather
-- than an enum so values can be added without the ADD VALUE transaction
-- restriction.

CREATE TABLE design_reports (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  listing_id       text NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  design_id        text NOT NULL REFERENCES designs(id)  ON DELETE CASCADE,
  reporter_user_id text REFERENCES users(id) ON DELETE SET NULL, -- NULL = not logged in
  reason           text NOT NULL CHECK (reason IN ('hak-cipta', 'merek-dagang', 'konten-tidak-pantas', 'lainnya')),
  email            text NOT NULL,
  description      text NOT NULL,
  evidence_url     text,
  status           text NOT NULL DEFAULT 'baru' CHECK (status IN ('baru', 'ditinjau', 'ditindak', 'ditolak')),
  admin_note       text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX design_reports_status_idx  ON design_reports (status, created_at DESC);
CREATE INDEX design_reports_listing_idx ON design_reports (listing_id);
