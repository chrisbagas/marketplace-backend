package main

import (
	"log"
	"os"
	"strings"
	"time"

	"karyakita/api/internal/api"
	"karyakita/api/internal/biteship"
)

// newShipping memilih penyedia pengiriman:
//
//	BITESHIP_API_KEY diisi → Biteship (ongkir langsung, resi otomatis, webhook pelacakan)
//	kosong                 → tarif flat + resi manual (dev)
//
// Dengan kunci terisi, asal pengiriman dan rahasia webhook wajib lengkap.
func newShipping() api.ShippingConfig {
	key := os.Getenv("BITESHIP_API_KEY")
	if key == "" {
		log.Printf("pengiriman: tarif FLAT + resi manual (isi BITESHIP_API_KEY untuk Biteship)")
		return api.ShippingConfig{}
	}
	cfg := api.ShippingConfig{
		Biteship:      biteship.New(key),
		WebhookSecret: os.Getenv("BITESHIP_WEBHOOK_SECRET"),
		Couriers:      strings.ReplaceAll(strings.TrimSpace(os.Getenv("BITESHIP_COURIERS")), " ", ""),
		CacheTTL:      10 * time.Minute,
		Origin: api.ShipOrigin{
			AreaID:       os.Getenv("SHIP_ORIGIN_AREA_ID"),
			PostalCode:   os.Getenv("SHIP_ORIGIN_POSTAL"),
			ContactName:  os.Getenv("SHIP_ORIGIN_NAME"),
			ContactPhone: os.Getenv("SHIP_ORIGIN_PHONE"),
			Address:      os.Getenv("SHIP_ORIGIN_ADDRESS"),
			Note:         os.Getenv("SHIP_ORIGIN_NOTE"),
		},
	}
	if cfg.WebhookSecret == "" {
		log.Fatal("BITESHIP_WEBHOOK_SECRET wajib diisi bersama BITESHIP_API_KEY (webhook Biteship tidak bertanda tangan)")
	}
	if cfg.Origin.AreaID == "" || cfg.Origin.ContactName == "" || cfg.Origin.ContactPhone == "" || cfg.Origin.Address == "" {
		log.Fatal("SHIP_ORIGIN_AREA_ID, SHIP_ORIGIN_NAME, SHIP_ORIGIN_PHONE, dan SHIP_ORIGIN_ADDRESS wajib diisi bersama BITESHIP_API_KEY")
	}
	mode := "live"
	if strings.HasPrefix(key, "biteship_test.") {
		mode = "test"
	}
	log.Printf("pengiriman: Biteship (%s) (webhook: POST /api/shipping/biteship/webhook, header X-Webhook-Secret atau ?secret=)", mode)
	return cfg
}
