package db

import (
	"context"
	"embed"
	_ "embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is one schema change. Version is the numeric filename prefix
// (015_search.sql -> 15), never the position in the list, so branches that lack
// some numbered files still record the right versions.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// migrationVersion extracts the numeric prefix of a "NNN_name.sql" filename.
func migrationVersion(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok || prefix == "" {
		return 0, fmt.Errorf("file migrasi %q tidak punya prefiks angka (NNN_nama.sql)", name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil || v < 2 {
		return 0, fmt.Errorf("file migrasi %q: prefiks harus angka >= 2 (versi 1 = schema.sql)", name)
	}
	return v, nil
}

// migrationList returns all migrations sorted by version: schema.sql is
// version 1, then migrations/*.sql by their numeric filename prefix.
func migrationList() ([]migration, error) {
	list := []migration{{Version: 1, Name: "schema.sql", SQL: schemaSQL}}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		v, err := migrationVersion(e.Name())
		if err != nil {
			return nil, err
		}
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		list = append(list, migration{Version: v, Name: e.Name(), SQL: string(b)})
	}
	return list, nil
}

// pendingMigrations returns the migrations whose version is not in applied, in
// ascending version order. It errors when two migrations share a version.
func pendingMigrations(all []migration, applied map[int]bool) ([]migration, error) {
	sorted := append([]migration(nil), all...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Version == sorted[i-1].Version {
			return nil, fmt.Errorf("versi migrasi %d dipakai dua file: %s dan %s",
				sorted[i].Version, sorted[i-1].Name, sorted[i].Name)
		}
	}
	var pending []migration
	for _, m := range sorted {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// Connect waits for Postgres to come up (useful right after `docker compose up`).
func Connect(ctx context.Context, dsn string, wait time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database tidak bisa dihubungi: %w", lastErr)
		}
		time.Sleep(time.Second)
	}
}

// Migrate applies every migration whose version is missing from
// schema_migrations, in ascending order, each in its own transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	all, err := migrationList()
	if err != nil {
		return err
	}
	pending, err := pendingMigrations(all, applied)
	if err != nil {
		return err
	}
	for _, m := range pending {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrasi versi %d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
