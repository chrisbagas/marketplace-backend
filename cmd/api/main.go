package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"karyakita/api/internal/api"
	"karyakita/api/internal/db"
	"karyakita/api/internal/envfile"
	"karyakita/api/internal/mail"
	"karyakita/api/internal/midtrans"
	"karyakita/api/internal/s3"
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
	// rebuild the search index: cheap, and it picks up anything changed outside the API
	if err := db.ReindexSearch(ctx, pool, nil); err != nil {
		log.Fatalf("indeks pencarian: %v", err)
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

	store, err := s3.FromEnv(production)
	if err != nil {
		log.Fatalf("penyimpanan file: %v", err)
	}
	if store == nil {
		log.Printf("PERINGATAN: S3_* belum diisi — unggah file dinonaktifkan")
	}

	log.Printf("KaryaKita API siap di http://localhost:%s (db ok, skema termigrasi)", port)
	srv := api.NewServer(pool, authCfg)
	srv.SetFiles(store)
	srv.SetPayments(newPayments(production))
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

// newPayments memilih penyedia pembayaran:
//
//	MIDTRANS_SERVER_KEY diisi → Midtrans (MIDTRANS_ENV=production untuk produksi, selain itu sandbox)
//	kosong, bukan produksi    → mock (pembayaran tiruan untuk dev)
//	kosong, produksi          → none (pembayaran dinonaktifkan; admin bisa tandai lunas manual)
func newPayments(production bool) api.PaymentsConfig {
	serverKey := os.Getenv("MIDTRANS_SERVER_KEY")
	if serverKey == "" {
		if production {
			log.Printf("PERINGATAN: MIDTRANS_SERVER_KEY kosong di produksi — pembayaran online nonaktif")
			return api.PaymentsConfig{Provider: "none"}
		}
		log.Printf("pembayaran: MOCK (isi MIDTRANS_SERVER_KEY & MIDTRANS_CLIENT_KEY untuk Midtrans sandbox)")
		return api.PaymentsConfig{Provider: "mock"}
	}
	clientKey := os.Getenv("MIDTRANS_CLIENT_KEY")
	if clientKey == "" {
		log.Fatal("MIDTRANS_CLIENT_KEY wajib diisi bersama MIDTRANS_SERVER_KEY")
	}
	prod := os.Getenv("MIDTRANS_ENV") == "production"
	var enabled []string
	for _, p := range strings.Split(os.Getenv("MIDTRANS_ENABLED_PAYMENTS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			enabled = append(enabled, p)
		}
	}
	env := "sandbox"
	if prod {
		env = "PRODUKSI"
	}
	client := midtrans.New(serverKey, clientKey, prod)
	// Tanya Midtrans langsung apakah kunci ini diterima di lingkungan yang dipilih
	// (lebih andal daripada menebak dari awalan kunci). Kunci ditolak → berhenti.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.CheckCredentials(ctx); err != nil {
		var apiErr *midtrans.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			log.Fatalf("Midtrans (%s) menolak MIDTRANS_SERVER_KEY — kunci salah, atau kunci sandbox/produksi tertukar dengan MIDTRANS_ENV=%s", env, os.Getenv("MIDTRANS_ENV"))
		}
		log.Printf("PERINGATAN: kunci Midtrans belum bisa diverifikasi (%v) — lanjut, akan dicoba lagi saat pembayaran", err)
	} else {
		log.Printf("pembayaran: kunci Midtrans %s diterima", env)
	}
	log.Printf("pembayaran: Midtrans %s (webhook: POST /api/payments/midtrans/notification)", env)
	return api.PaymentsConfig{Provider: "midtrans", Midtrans: client, EnabledPayments: enabled}
}
