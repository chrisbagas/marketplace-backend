-- Search + Etsy-style taxonomy.
--
-- A listing is described three ways:
--   category   : what the product is. A small tree, category -> product type
--                (Pakaian -> Kaos). Replaces the old placeholder categories.
--   attributes : curated facets of the design (Tema, Gaya, Acara). The old
--                categories were themes, so they move here as "tema".
--   tags       : free keywords from the creator (designs.tags), max 13.
-- Search matches all three plus the title and creator name.
--
-- Numbered 015 because 012-014 belong to feat/payments.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ===== Attributes =====
CREATE TABLE attributes (
  id             text PRIMARY KEY,
  label          text NOT NULL,
  sort           int  NOT NULL DEFAULT 0,
  max_per_design int  NOT NULL DEFAULT 3 CHECK (max_per_design > 0)
);

CREATE TABLE attribute_values (
  id           text PRIMARY KEY,
  attribute_id text NOT NULL REFERENCES attributes(id) ON DELETE CASCADE,
  label        text NOT NULL,
  emoji        text NOT NULL DEFAULT '',
  sort         int  NOT NULL DEFAULT 0,
  UNIQUE (attribute_id, label)
);

CREATE TABLE design_attributes (
  design_id text NOT NULL REFERENCES designs(id) ON DELETE CASCADE,
  value_id  text NOT NULL REFERENCES attribute_values(id) ON DELETE CASCADE,
  PRIMARY KEY (design_id, value_id)
);
CREATE INDEX design_attributes_value_idx ON design_attributes (value_id);

INSERT INTO attributes (id, label, sort, max_per_design) VALUES
  ('tema',  'Tema',  1, 3),
  ('gaya',  'Gaya',  2, 2),
  ('acara', 'Acara', 3, 2);

-- the old categories were themes; "tipografi" is really a style
INSERT INTO attribute_values (id, attribute_id, label, emoji, sort)
SELECT id, 'tema', label, emoji, sort FROM categories;
UPDATE attribute_values SET attribute_id = 'gaya', sort = 1 WHERE id = 'tipografi';

INSERT INTO attribute_values (id, attribute_id, label, emoji, sort) VALUES
  ('hewan',        'tema',  'Hewan',          '🐾', 13),
  ('wisata',       'tema',  'Wisata',         '🧭', 14),
  ('retro',        'gaya',  'Retro',          '📼', 2),
  ('minimalis',    'gaya',  'Minimalis',      '◻️', 3),
  ('ilustrasi',    'gaya',  'Ilustrasi',      '🎨', 4),
  ('kawaii',       'gaya',  'Kawaii',         '🌸', 5),
  ('streetwear',   'gaya',  'Streetwear',     '🛹', 6),
  ('lucu',         'gaya',  'Lucu',           '😄', 7),
  ('lebaran',      'acara', 'Lebaran',        '🌙', 1),
  ('natal',        'acara', 'Natal',          '🎄', 2),
  ('kemerdekaan',  'acara', 'Kemerdekaan',    '🇮🇩', 3),
  ('wisuda',       'acara', 'Wisuda',         '🎓', 4),
  ('ulang-tahun',  'acara', 'Ulang Tahun',    '🎂', 5),
  ('valentine',    'acara', 'Valentine',      '💝', 6)
ON CONFLICT DO NOTHING;

INSERT INTO design_attributes (design_id, value_id)
SELECT design_id, category_id FROM design_categories;
DROP TABLE design_categories;

-- extra style/theme for the seed designs (old DBs only; on a new DB the
-- designs don't exist yet and seed.go seedDesignAttributes fills them)
INSERT INTO design_attributes (design_id, value_id)
SELECT v.design_id, v.value_id
FROM (VALUES
  ('anak-senja', 'retro'),
  ('kawung-modern', 'minimalis'),
  ('komodo-trail', 'wisata'), ('komodo-trail', 'hewan'),
  ('ombak-nusantara', 'ilustrasi'),
  ('jaga-laut', 'hewan'),
  ('rendang-love', 'lucu'),
  ('tropis', 'ilustrasi')
) AS v(design_id, value_id)
WHERE EXISTS (SELECT 1 FROM designs d WHERE d.id = v.design_id)
ON CONFLICT DO NOTHING;

ALTER TABLE design_submissions RENAME COLUMN category_ids TO attribute_ids;

-- ===== Categories: the product tree =====
DELETE FROM categories;
INSERT INTO categories (id, label, emoji, sort) VALUES
  ('pakaian', 'Pakaian',        '👕', 1),
  ('tas',     'Tas & Aksesori', '👜', 2),
  ('rumah',   'Rumah & Dapur',  '☕', 3);

ALTER TABLE product_types ADD COLUMN category_id text REFERENCES categories(id);
ALTER TABLE product_types ADD COLUMN sort int NOT NULL DEFAULT 0;
-- no rows on a new DB: seed.go sets these when it inserts the product types
UPDATE product_types SET
  category_id = CASE id WHEN 'totebag' THEN 'tas' WHEN 'mug' THEN 'rumah' ELSE 'pakaian' END,
  sort        = CASE id WHEN 'kaos' THEN 1 WHEN 'hoodie' THEN 2 WHEN 'totebag' THEN 3 ELSE 4 END;

-- ===== Search =====
-- One weighted document per listing, rebuilt by db.ReindexSearch.
CREATE TABLE listing_search (
  listing_id text PRIMARY KEY REFERENCES listings(id) ON DELETE CASCADE,
  doc        tsvector NOT NULL
);
CREATE INDEX listing_search_doc_idx ON listing_search USING gin (doc);

-- Every word that occurs in an active listing; a misspelled query word is
-- corrected to its closest entry (trigram similarity).
CREATE TABLE search_words (
  word text PRIMARY KEY,
  ndoc int  NOT NULL
);
CREATE INDEX search_words_trgm_idx ON search_words USING gin (word gin_trgm_ops);

-- Words that mean the same thing; searching one also matches the others.
CREATE TABLE search_synonyms (
  id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  terms text[] NOT NULL CHECK (cardinality(terms) >= 2)
);
INSERT INTO search_synonyms (terms) VALUES
  ('{kaos,kaus,tshirt,baju}'),
  ('{hoodie,jaket,sweater}'),
  ('{mug,gelas,cangkir}'),
  ('{totebag,tote,tas}'),
  ('{kopi,coffee,ngopi}'),
  ('{anime,manga,wibu}'),
  ('{senja,sunset}'),
  ('{laut,pantai,ocean}'),
  ('{lucu,humor,kocak}'),
  ('{tipografi,typography,quotes}'),
  ('{kuliner,makanan,food}');

-- results = how many products the term returned last time (0 = a gap in the catalog)
ALTER TABLE search_terms ADD COLUMN results int;
ALTER TABLE search_terms ADD COLUMN last_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX listings_price_idx   ON listings (price);
CREATE INDEX listings_created_idx ON listings (created_at DESC);
