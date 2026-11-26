// Command create-admin creates an admin account, or promotes an existing user
// to admin. Production has no demo accounts (APP_ENV=production), so this is how
// the first admin is made.
//
//	docker compose exec -T api /create-admin -email you@example.com -username you -name "Your Name" -password-stdin
//	docker compose exec -e ADMIN_PASSWORD=... api /create-admin -email you@example.com -username you
//
// The password comes from ADMIN_PASSWORD or stdin (-password-stdin), never from a
// flag, so it does not end up in shell history or `ps`. It is hashed with bcrypt
// exactly like signup. Connects with DATABASE_URL; the API must have started once
// so the schema exists.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/db"
)

// Same rules as POST /api/auth/signup (internal/api/auth.go).
var usernameRe = regexp.MustCompile(`^[a-z0-9_.]{3,30}$`)

const (
	minPassword = 8
	maxPassword = 72 // bcrypt input limit (bytes)
)

func main() {
	email := flag.String("email", "", "email of the admin (required)")
	username := flag.String("username", "", "username (required when creating a new account)")
	name := flag.String("name", "", "display name (default: username)")
	passStdin := flag.Bool("password-stdin", false, "read the password from stdin instead of $ADMIN_PASSWORD")
	flag.Parse()

	password := os.Getenv("ADMIN_PASSWORD")
	if *passStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fatalf("read password from stdin: %v", err)
		}
		password = strings.TrimRight(line, "\r\n")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fatalf("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn, 10*time.Second)
	if err != nil {
		fatalf("connect: %v", err)
	}
	defer pool.Close()

	res, err := ensureAdmin(ctx, pool, *email, *username, *name, password)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Println(res)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "create-admin: "+format+"\n", a...)
	os.Exit(1)
}

// ensureAdmin looks the user up by email. Missing: creates a verified admin
// (username + password required). Present: sets role admin and marks the email
// verified; a non-empty password also replaces the old one and revokes sessions.
func ensureAdmin(ctx context.Context, pool *pgxpool.Pool, email, username, name, password string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	username = strings.ToLower(strings.TrimSpace(username))
	name = strings.TrimSpace(name)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return "", errors.New("-email must be a valid email address")
	}
	if password != "" {
		if len(password) < minPassword || len(password) > maxPassword {
			return "", fmt.Errorf("password must be %d-%d characters", minPassword, maxPassword)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1 FOR UPDATE`, email).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if !usernameRe.MatchString(username) {
			return "", errors.New("-username is required for a new account: 3-30 characters, a-z 0-9 _ .")
		}
		if password == "" {
			return "", errors.New("a new account needs a password (ADMIN_PASSWORD or -password-stdin)")
		}
		if name == "" {
			name = username
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return "", err
		}
		buf := make([]byte, 6)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		id = "u-" + hex.EncodeToString(buf)
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, username, email, name, role, password_hash, email_verified)
			VALUES ($1, $2, $3, $4, 'admin', $5, true)`, id, username, email, name, string(hash)); err != nil {
			return "", fmt.Errorf("create user (username or email already taken?): %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return fmt.Sprintf("created admin %q (%s)", username, email), nil
	case err != nil:
		return "", err
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET role = 'admin', email_verified = true WHERE id = $1`, id); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("promoted existing user %s (%s) to admin", id, email)
	if password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, string(hash)); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
			return "", err
		}
		msg += "; password replaced, sessions revoked"
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return msg, nil
}
