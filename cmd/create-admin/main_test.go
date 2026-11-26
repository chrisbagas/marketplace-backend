package main

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/db"
)

// Needs a migrated database: TEST_DATABASE_URL=postgres://... go test ./cmd/create-admin
func TestEnsureAdmin(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const email = "create-admin-test@example.com"
	cleanup := func() {
		pool.Exec(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE email = $1)`, email)
		pool.Exec(ctx, `DELETE FROM users WHERE email = $1`, email)
	}
	cleanup()
	defer cleanup()

	// validation: new account needs username and password
	if _, err := ensureAdmin(ctx, pool, email, "", "", "longenough1"); err == nil {
		t.Fatal("expected error without username")
	}
	if _, err := ensureAdmin(ctx, pool, email, "adm_test", "", ""); err == nil {
		t.Fatal("expected error without password")
	}
	if _, err := ensureAdmin(ctx, pool, email, "adm_test", "", "short"); err == nil {
		t.Fatal("expected error for short password")
	}

	// create
	if _, err := ensureAdmin(ctx, pool, email, "adm_test", "Admin Test", "longenough1"); err != nil {
		t.Fatal(err)
	}
	var role, hash string
	var verified bool
	q := `SELECT role::text, password_hash, email_verified FROM users WHERE email = $1`
	if err := pool.QueryRow(ctx, q, email).Scan(&role, &hash, &verified); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || !verified || bcrypt.CompareHashAndPassword([]byte(hash), []byte("longenough1")) != nil {
		t.Fatalf("created user wrong: role=%s verified=%v", role, verified)
	}

	// promote an existing customer (no password given: password untouched)
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'customer', email_verified = false WHERE email = $1`, email); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAdmin(ctx, pool, email, "", "", ""); err != nil {
		t.Fatal(err)
	}
	var hash2 string
	if err := pool.QueryRow(ctx, q, email).Scan(&role, &hash2, &verified); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || !verified || hash2 != hash {
		t.Fatalf("promote wrong: role=%s verified=%v samehash=%v", role, verified, hash2 == hash)
	}

	// password replacement
	if _, err := ensureAdmin(ctx, pool, email, "", "", "another-pass-2"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, q, email).Scan(&role, &hash2, &verified); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash2), []byte("another-pass-2")) != nil {
		t.Fatal("password not replaced")
	}
}
