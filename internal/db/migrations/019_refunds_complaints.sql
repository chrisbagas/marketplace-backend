-- Cancel / refund / complaint.
--
-- * refunds:    every refund of a paid order (cancel of a `dibayar` order, or the
--               refund outcome of a complaint). method = gateway (Midtrans refund
--               API) or manual (admin transferred the money, `reference` = bank
--               transfer reference). The money columns record what the ledger
--               gave back, so the books can be explained from this row alone.
-- * complaints: one per order, filed by the buyer while the order is `tiba`.
--               An open complaint pauses the 48h auto-complete. Admin resolves it
--               with a reprint (new linked order, total 0), a refund, or a rejection.
-- * orders.reprint_of: set on the free replacement order created by a reprint.
-- * royalties.reversed_amount: part of the royalty taken back by a refund.
--               `amount` stays the NET royalty (what the creator still earns), so
--               every balance formula that sums `amount` keeps working unchanged;
--               original royalty = amount + reversed_amount.
--
-- No new enum values: a cancelled-and-refunded order reuses `dibatalkan`, the
-- payment keeps status `paid` (the money did arrive; the refund is in `refunds`).
-- Producer charges of a cancelled order are deleted (a `dibayar` order has never
-- been shipped, so none of them can have been paid out to the vendor).

CREATE TABLE refunds (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id         text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  amount           int  NOT NULL CHECK (amount > 0),
  method           text NOT NULL CHECK (method IN ('gateway', 'manual')),
  reference        text NOT NULL,                       -- Midtrans refund_key | bank transfer reference
  reason           text NOT NULL,
  royalty_reversed int  NOT NULL DEFAULT 0 CHECK (royalty_reversed >= 0),
  vendor_reversed  int  NOT NULL DEFAULT 0 CHECK (vendor_reversed >= 0),
  created_by       text REFERENCES users(id),
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refunds_order_idx ON refunds (order_id);

CREATE TABLE complaints (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id        text NOT NULL UNIQUE REFERENCES orders(id) ON DELETE CASCADE,  -- one complaint per order
  user_id         text REFERENCES users(id),
  reason          text NOT NULL CHECK (reason IN ('cacat-cetak', 'salah-produk', 'rusak', 'tidak-sesuai', 'lainnya')),
  description     text NOT NULL,
  photos          text[] NOT NULL DEFAULT '{}' CHECK (cardinality(photos) <= 3),
  status          text NOT NULL DEFAULT 'terbuka' CHECK (status IN ('terbuka', 'dicetak-ulang', 'direfund', 'ditolak')),
  resolution_note text,
  refund_id       bigint REFERENCES refunds(id),
  reprint_order_id text REFERENCES orders(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  resolved_at     timestamptz,
  resolved_by     text REFERENCES users(id),
  CHECK ((status = 'terbuka') = (resolved_at IS NULL))
);
CREATE INDEX complaints_status_idx ON complaints (status, created_at);

ALTER TABLE orders ADD COLUMN reprint_of text REFERENCES orders(id);
CREATE INDEX orders_reprint_of_idx ON orders (reprint_of) WHERE reprint_of IS NOT NULL;

ALTER TABLE royalties ADD COLUMN reversed_amount int NOT NULL DEFAULT 0 CHECK (reversed_amount >= 0);
