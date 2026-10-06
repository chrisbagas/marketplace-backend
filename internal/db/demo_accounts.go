package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// DemoAccountIDs are the seeded users (one per role) that get a known
// password outside production so the prototype can be explored end-to-end.
var DemoAccountIDs = []string{"u-admin", "u-raka", "u-demo"}

// EnsureDemoPasswords sets password on demo accounts that don't have one yet.
// Never overwrites a password that was already set (e.g. changed by hand).
func EnsureDemoPasswords(ctx context.Context, pool *pgxpool.Pool, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, email_verified = true
		 WHERE id = ANY($1) AND password_hash IS NULL`,
		DemoAccountIDs, string(hash))
	return err
}
