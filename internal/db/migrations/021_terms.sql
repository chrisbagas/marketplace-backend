-- Consent records for the legal pages (Syarat & Ketentuan, Kebijakan Privasi,
-- creator agreement). NULL = account created before consent was collected.
ALTER TABLE users
  ADD COLUMN terms_accepted_at         timestamptz,
  ADD COLUMN creator_terms_accepted_at timestamptz;
