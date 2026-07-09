package db

import (
	"context"
	"embed"
	_ "embed"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationList returns all migrations in order: schema.sql is version 1,
// then migrations/*.sql sorted by filename (002_..., 003_..., dst).
func migrationList() ([]string, error) {
	list := []string{schemaSQL}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := migrationsFS.ReadFile("migrations/" + n)
		if err != nil {
			return nil, err
		}
		list = append(list, string(b))
	}
	return list, nil
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

// Migrate applies pending migrations in order, tracked in schema_migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var current int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	all, err := migrationList()
	if err != nil {
		return err
	}
	for v := current + 1; v <= len(all); v++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, all[v-1]); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrasi versi %d: %w", v, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
