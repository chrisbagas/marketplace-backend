-- Print spec: the vendor must learn the print size of every order item.
--
-- Studio lets a creator pick the print width and a vertical offset in cm.
-- They are now kept on the submission, copied to the listing on approval and
-- snapshotted onto every order item (like title/price/design_uri), so the
-- vendor sees the exact size even if the listing changes later.
--
--   width_cm     physical print width, 0 < width <= product_types.print_max_w_cm
--   offset_y_cm  vertical shift from the CENTRE of the print area, in cm:
--                positive = lower (towards the hem), negative = higher
--                (towards the collar). Same meaning as user_designs.offset_y_cm
--                and the Mockup component.
--
-- Existing rows get the Studio default for their product type, round(80% of
-- the max print width) with no offset (kaos 24, hoodie 22, mug 7, totebag 20 cm).
-- A flat 24 cm would exceed the mug's 9 cm print area. These are defaults, not
-- measurements: pre-016 designs never recorded a size.

ALTER TABLE design_submissions ADD COLUMN width_cm numeric(5,1);
ALTER TABLE design_submissions ADD COLUMN offset_y_cm numeric(5,1);
ALTER TABLE listings ADD COLUMN width_cm numeric(5,1);
ALTER TABLE listings ADD COLUMN offset_y_cm numeric(5,1);
ALTER TABLE order_items ADD COLUMN width_cm numeric(5,1);
ALTER TABLE order_items ADD COLUMN offset_y_cm numeric(5,1);

UPDATE design_submissions ds
SET width_cm = round(pt.print_max_w_cm * 0.8), offset_y_cm = 0
FROM product_types pt WHERE pt.id = ds.product_type_id;

UPDATE listings l
SET width_cm = round(pt.print_max_w_cm * 0.8), offset_y_cm = 0
FROM product_types pt WHERE pt.id = l.product_type_id;

-- catalog items follow their listing; custom designs (no listing) and items
-- whose listing is gone use the product-type default
UPDATE order_items oi
SET width_cm = l.width_cm, offset_y_cm = l.offset_y_cm
FROM listings l WHERE l.id = oi.listing_id;

UPDATE order_items oi
SET width_cm = round(pt.print_max_w_cm * 0.8), offset_y_cm = 0
FROM product_types pt WHERE pt.id = oi.type AND oi.width_cm IS NULL;

ALTER TABLE design_submissions
  ALTER COLUMN width_cm SET NOT NULL, ALTER COLUMN width_cm SET DEFAULT 24,
  ALTER COLUMN offset_y_cm SET NOT NULL, ALTER COLUMN offset_y_cm SET DEFAULT 0,
  ADD CONSTRAINT design_submissions_width_positive CHECK (width_cm > 0);

ALTER TABLE listings
  ALTER COLUMN width_cm SET NOT NULL, ALTER COLUMN width_cm SET DEFAULT 24,
  ALTER COLUMN offset_y_cm SET NOT NULL, ALTER COLUMN offset_y_cm SET DEFAULT 0,
  ADD CONSTRAINT listings_width_positive CHECK (width_cm > 0);

-- no default here: every order-item insert must state the spec explicitly
ALTER TABLE order_items
  ALTER COLUMN width_cm SET NOT NULL,
  ALTER COLUMN offset_y_cm SET NOT NULL,
  ADD CONSTRAINT order_items_width_positive CHECK (width_cm > 0);
