package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"karyakita/api/internal/api"
	"karyakita/api/internal/db"
	"karyakita/api/internal/envfile"
	"karyakita/api/internal/mail"
)

func main() {
	// dev: baca backend/.env bila ada (tidak menimpa variabel yang sudah di-set)
	if n, err := envfile.Load(".env"); err != nil {
		log.Fatalf("baca .env: %v", err)
	} else if n > 0 {
		log.Printf(".env dimuat (%d variabel)", n)
	}
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
		AppURL:       envOr("APP_URL", "http://localhost:3000"),
		Mailer:       newMailer(production),
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
	srv := api.NewServer(pool, uploadDir, authCfg)
	// pesanan "tiba" selesai otomatis 2 hari setelah paket sampai (dicek tiap 10 menit)
	go srv.RunOrderJobs(ctx, 10*time.Minute)
	if err := http.ListenAndServe(":"+port, srv); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newMailer: SMTP_HOST diisi → kirim lewat SMTP. Di dev bawaannya Mailpit
// (localhost:1025, lihat docker-compose.yml). Di produksi tanpa SMTP, email
// tidak dikirim dan isinya (berisi token) tidak ditulis ke log.
func newMailer(production bool) mail.Mailer {
	host := os.Getenv("SMTP_HOST")
	if host == "" && !production {
		host = "localhost"
	}
	if host == "" {
		log.Printf("PERINGATAN: SMTP_HOST kosong — email verifikasi & reset password tidak terkirim")
		return mail.Log{ShowBody: false}
	}
	m := mail.SMTP{
		Host:     host,
		Port:     envOr("SMTP_PORT", "1025"),
		Username: os.Getenv("SMTP_USERNAME"),
		Password: os.Getenv("SMTP_PASSWORD"),
		From:     envOr("MAIL_FROM", "KaryaKita <no-reply@karyakita.id>"),
	}
	log.Printf("email dikirim lewat SMTP %s:%s", m.Host, m.Port)
	return m
}
