package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Field weights of the search document: A = title, B = tags + attribute
// labels, C = product type + category, D = creator name.
const reindexSQL = `
	INSERT INTO listing_search (listing_id, doc)
	SELECT l.id,
	       setweight(to_tsvector('simple', COALESCE(l.title_override, '') || ' ' || d.title), 'A') ||
	       setweight(to_tsvector('simple', array_to_string(d.tags, ' ') || ' ' || COALESCE((
	           SELECT string_agg(av.label, ' ')
	           FROM design_attributes da JOIN attribute_values av ON av.id = da.value_id
	           WHERE da.design_id = d.id), '')), 'B') ||
	       setweight(to_tsvector('simple', pt.label || ' ' || COALESCE(c.label, '')), 'C') ||
	       setweight(to_tsvector('simple', dr.name), 'D')
	FROM listings l
	JOIN designs d ON d.id = l.design_id
	JOIN designers dr ON dr.id = d.designer_id
	JOIN product_types pt ON pt.id = l.product_type_id
	LEFT JOIN categories c ON c.id = pt.category_id
	WHERE $1::text[] IS NULL OR l.id = ANY($1)
	ON CONFLICT (listing_id) DO UPDATE SET doc = EXCLUDED.doc`

// ReindexSearch rebuilds the search document of the given listings (nil = all)
// and the vocabulary used for typo correction. Call it after anything that
// changes what a listing says: publish, rename, (de)activate, attribute edits.
// It also runs at startup, so a missed call heals on the next restart.
func ReindexSearch(ctx context.Context, pool *pgxpool.Pool, listingIDs []string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// one rebuild at a time: the vocabulary is replaced wholesale
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(815015)`); err != nil {
		return err
	}
	var ids any
	if listingIDs != nil {
		ids = listingIDs
	}
	if _, err := tx.Exec(ctx, reindexSQL, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM search_words`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO search_words (word, ndoc)
		SELECT word, ndoc FROM ts_stat(
			'SELECT ls.doc FROM listing_search ls JOIN listings l ON l.id = ls.listing_id WHERE l.active')
		WHERE length(word) BETWEEN 2 AND 40`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
