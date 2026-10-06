package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"karyakita/api/internal/api"
	"karyakita/api/internal/db"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://karyakita:karyakita_dev@localhost:5432/karyakita"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081" // frontend (next.config.ts) mem-proxy ke port ini
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn, 30*time.Second)
	if err != nil {
		log.Fatalf("koneksi database gagal: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrasi gagal: %v", err)
	}
	if err := db.SeedIfEmpty(ctx, pool); err != nil {
		log.Fatalf("seed gagal: %v", err)
	}
	production := os.Getenv("APP_ENV") == "production"
	if !production {
		// akun demo (admin / raka / demo) bisa langsung dipakai login di dev
		demoPass := os.Getenv("DEMO_PASSWORD")
		if demoPass == "" {
			demoPass = "karyakita123"
		}
		if err := db.EnsureDemoPasswords(ctx, pool, demoPass); err != nil {
			log.Fatalf("password akun demo: %v", err)
		}
	}

	uploadDir := os.Getenv("UPLOAD_DIR")
	if uploadDir == "" {
		uploadDir = "uploads"
	}
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("folder upload: %v", err)
	}

	authCfg := api.AuthConfig{
		CookieSecure: production || os.Getenv("COOKIE_SECURE") == "true",
		Google: api.GoogleConfig{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
		},
	}
	if authCfg.Google.Enabled() {
		log.Printf("login Google aktif (redirect → %s)", authCfg.Google.RedirectURL)
	}

	log.Printf("KaryaKita API siap di http://localhost:%s (db ok, skema termigrasi, upload → %s)", port, uploadDir)
	if err := http.ListenAndServe(":"+port, api.NewServer(pool, uploadDir, authCfg)); err != nil {
		log.Fatal(err)
	}
}
