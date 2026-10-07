-- Kategori terkurasi (dipilih admin) + tag bebas kreator.
-- Kategori = navigasi belanja; tag = kata kunci pencarian ala hashtag.

CREATE TABLE categories (
  id    text PRIMARY KEY,
  label text NOT NULL,
  emoji text NOT NULL DEFAULT '',
  sort  int  NOT NULL DEFAULT 0
);

CREATE TABLE design_categories (
  design_id   text NOT NULL REFERENCES designs(id) ON DELETE CASCADE,
  category_id text NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  PRIMARY KEY (design_id, category_id)
);

-- pengajuan desain ikut membawa kategori & tag pilihannya;
-- saat disetujui, pengajuan diterbitkan sebagai listing (dicatat di sini)
ALTER TABLE design_submissions ADD COLUMN category_ids text[] NOT NULL DEFAULT '{}';
ALTER TABLE design_submissions ADD COLUMN tags         text[] NOT NULL DEFAULT '{}';
ALTER TABLE design_submissions ADD COLUMN published_listing_id text REFERENCES listings(id);

INSERT INTO categories (id, label, emoji, sort) VALUES
  ('alam',      'Alam',      '🌿', 1),
  ('senja',     'Senja',     '🌅', 2),
  ('laut',      'Laut',      '🌊', 3),
  ('batik',     'Batik',     '🧵', 4),
  ('kopi',      'Kopi',      '☕', 5),
  ('kuliner',   'Kuliner',   '🍛', 6),
  ('tipografi', 'Tipografi', '✒️', 7),
  ('anime',     'Anime',     '⛩️', 8),
  ('film',      'Film',      '🎬', 9),
  ('superhero', 'Superhero', '🦸', 10),
  ('musik',     'Musik',     '🎵', 11),
  ('gaming',    'Gaming',    '🎮', 12);

-- pemetaan desain seed ke kategori (backfill untuk DB lama). Di DB baru
-- desainnya belum ada saat migrasi jalan — seeder (seed.go: seedDesignCategories)
-- yang mengisinya, jadi baris untuk desain yang belum ada dilewati.
INSERT INTO design_categories (design_id, category_id)
SELECT v.design_id, v.category_id
FROM (VALUES
  ('anak-senja', 'senja'), ('anak-senja', 'tipografi'),
  ('kopi-dulu', 'kopi'), ('kopi-dulu', 'tipografi'),
  ('kawung-modern', 'batik'),
  ('komodo-trail', 'alam'),
  ('ombak-nusantara', 'laut'), ('ombak-nusantara', 'alam'),
  ('jaga-laut', 'laut'), ('jaga-laut', 'alam'),
  ('rendang-love', 'kuliner'), ('rendang-love', 'tipografi'),
  ('tropis', 'alam')
) AS v(design_id, category_id)
WHERE EXISTS (SELECT 1 FROM designs d WHERE d.id = v.design_id);
