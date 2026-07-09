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
		port = "8080"
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

	log.Printf("KaryaKita API siap di http://localhost:%s (db ok, skema termigrasi)", port)
	if err := http.ListenAndServe(":"+port, api.NewServer(pool)); err != nil {
		log.Fatal(err)
	}
}
