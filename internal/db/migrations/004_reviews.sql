-- Ulasan pembeli: hanya dari pesanan yang sudah selesai (verified purchase),
-- satu ulasan per item per pesanan, tayang setelah lolos moderasi admin.

CREATE TYPE review_status AS ENUM ('review', 'disetujui', 'ditolak');

CREATE TABLE reviews (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  listing_id text NOT NULL REFERENCES listings(id),
  order_id   text NOT NULL REFERENCES orders(id),
  author     text NOT NULL,                -- nama pembeli dari pesanan
  rating     int  NOT NULL CHECK (rating BETWEEN 1 AND 5),
  comment    text NOT NULL DEFAULT '',
  status     review_status NOT NULL DEFAULT 'review',
  note       text,                         -- catatan moderasi
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (order_id, listing_id)
);
CREATE INDEX reviews_listing_idx ON reviews (listing_id, status);

-- Ulasan demo dari pesanan seed yang sudah selesai (DB baru diisi seeder)
INSERT INTO reviews (listing_id, order_id, author, rating, comment, status)
SELECT DISTINCT ON (o.id) oi.listing_id, o.id, o.cust_name, 5,
       'Kualitas cetaknya bagus, warna tajam dan bahannya adem. Recommended!',
       'disetujui'::review_status
FROM orders o
JOIN order_items oi ON oi.order_id = o.id
WHERE o.status = 'selesai' AND oi.listing_id IS NOT NULL
ON CONFLICT DO NOTHING;
