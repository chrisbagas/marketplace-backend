// Command migrate-uploads is a one-off: it copies the files that the old API kept
// in its local uploads/ folder into the S3/R2 bucket and rewrites every database
// reference from "/uploads/<name>" to the public URL
// (FILES_PUBLIC_URL/legacy/<name>).
//
// Safe to re-run: objects already in the bucket with the same size are skipped,
// `files` rows use ON CONFLICT DO NOTHING, and rows that no longer say
// "/uploads/..." are not touched. Data-URIs (dev seed) are left as they are.
//
//	go run ./cmd/migrate-uploads -dry-run            # report only, writes nothing
//	go run ./cmd/migrate-uploads -dir uploads        # do it
//
// Uses the same env as the API: DATABASE_URL and S3_* / FILES_PUBLIC_URL
// (outside APP_ENV=production the dev defaults apply, see internal/s3).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"karyakita/api/internal/envfile"
	"karyakita/api/internal/files"
	"karyakita/api/internal/s3"
)

// Every column that can hold an upload URL. Listings have none of their own:
// they show designs.uri.
var refColumns = []struct{ table, column string }{
	{"designs", "uri"},
	{"design_submissions", "uri"},
	{"user_designs", "uri"},
	{"order_items", "design_uri"},
	{"designers", "avatar_uri"},
}

const legacyURLPrefix = "/uploads/"

func main() {
	dir := flag.String("dir", envOr("UPLOAD_DIR", "uploads"), "folder with the old local uploads (env UPLOAD_DIR)")
	dryRun := flag.Bool("dry-run", false, "report what would change without uploading or writing to the database")
	flag.Parse()

	if _, err := envfile.Load(".env"); err != nil {
		log.Fatalf("baca .env: %v", err)
	}
	dsn := envOr("DATABASE_URL", "postgres://karyakita:karyakita_dev@localhost:5432/karyakita")
	production := os.Getenv("APP_ENV") == "production"
	store, err := s3.FromEnv(production)
	if err != nil {
		log.Fatalf("penyimpanan file: %v", err)
	}
	if store == nil {
		log.Fatal("S3_* belum diisi")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("database tidak bisa dihubungi: %v", err)
	}

	if err := run(ctx, pool, store, *dir, *dryRun); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, pool *pgxpool.Pool, store *s3.Client, dir string, dryRun bool) error {
	mode := ""
	if dryRun {
		mode = " (DRY RUN)"
	}

	// 1. Names referenced by the database
	referenced := map[string]bool{}
	for _, rc := range refColumns {
		rows, err := pool.Query(ctx, fmt.Sprintf(
			`SELECT DISTINCT %[2]s FROM %[1]s WHERE %[2]s LIKE '/uploads/%%'`, rc.table, rc.column))
		if err != nil {
			return fmt.Errorf("%s.%s: %w", rc.table, rc.column, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			referenced[strings.TrimPrefix(v, legacyURLPrefix)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	// 2. Files on disk (everything in the folder is copied, referenced or not)
	onDisk := map[string]bool{}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		log.Printf("folder %q tidak ada — tidak ada file lokal untuk disalin", dir)
	case err != nil:
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			onDisk[e.Name()] = true
		}
	}

	names := map[string]bool{}
	for n := range referenced {
		names[n] = true
	}
	for n := range onDisk {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	// 3. Copy to the bucket
	copied, skipped, bad := 0, 0, 0
	ready := map[string]bool{} // names now in the bucket (or that would be, in a dry run)
	for _, name := range sorted {
		key, ok := files.LegacyKey(name)
		if !ok {
			log.Printf("LEWATI %s: nama file tidak valid", name)
			bad++
			continue
		}
		if !onDisk[name] {
			// maybe a previous run already moved it
			if _, err := store.Head(ctx, key); err == nil {
				ready[name] = true
				skipped++
				continue
			}
			log.Printf("HILANG %s: dirujuk database tapi tidak ada di %s maupun di bucket", name, dir)
			bad++
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		info, err := files.Inspect(data[:min(len(data), files.PrefixBytes)], int64(len(data)))
		if err != nil {
			log.Printf("LEWATI %s: %v", name, err)
			bad++
			continue
		}
		if head, err := store.Head(ctx, key); err == nil && head.Size == int64(len(data)) {
			skipped++
		} else if dryRun {
			log.Printf("SALIN %s → %s (%s, %d byte)", name, key, info.Mime, len(data))
			copied++
		} else {
			if err := store.Put(ctx, key, info.Mime, data); err != nil {
				return fmt.Errorf("unggah %s: %w", name, err)
			}
			copied++
		}
		if !dryRun {
			if _, err := pool.Exec(ctx, `
				INSERT INTO files (key, mime, bytes, width_px, height_px, owner_id, purpose, created_at)
				VALUES ($1,$2,$3,NULLIF($4,0),NULLIF($5,0),NULL,$6,$7) ON CONFLICT (key) DO NOTHING`,
				key, info.Mime, len(data), info.Width, info.Height, files.LegacyPurpose, modTime(dir, name)); err != nil {
				return fmt.Errorf("simpan baris files %s: %w", key, err)
			}
		}
		ready[name] = true
	}

	// 4. Rewrite references (only for files that are in the bucket)
	rewritten := 0
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, rc := range refColumns {
		for name := range referenced {
			if !ready[name] {
				continue
			}
			key, _ := files.LegacyKey(name)
			if dryRun {
				var n int
				if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, rc.table, rc.column),
					legacyURLPrefix+name).Scan(&n); err != nil {
					return err
				}
				rewritten += n
				continue
			}
			tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s SET %[2]s = $1 WHERE %[2]s = $2`, rc.table, rc.column),
				store.PublicURL(key), legacyURLPrefix+name)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", rc.table, rc.column, err)
			}
			rewritten += int(tag.RowsAffected())
		}
	}
	if dryRun {
		tx.Rollback(ctx)
	} else if err := tx.Commit(ctx); err != nil {
		return err
	}

	// 5. What is still pointing at the old folder
	left := 0
	for _, rc := range refColumns {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s LIKE '/uploads/%%'`, rc.table, rc.column)).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			log.Printf("MASIH %d baris %s.%s menunjuk /uploads/", n, rc.table, rc.column)
		}
		left += n
	}

	log.Printf("selesai%s: %d disalin, %d sudah ada, %d bermasalah, %d referensi database %s, %d masih /uploads/",
		mode, copied, skipped, bad, rewritten, map[bool]string{true: "akan ditulis ulang", false: "ditulis ulang"}[dryRun], left)
	if bad > 0 || (!dryRun && left > 0) {
		return fmt.Errorf("ada %d file bermasalah dan %d referensi tersisa — periksa log di atas", bad, left)
	}
	return nil
}

func modTime(dir, name string) time.Time {
	if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return st.ModTime()
	}
	return time.Now()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
