// Package files holds the upload rules shared by the API and cmd/migrate-uploads:
// size limits, allowed purposes, object keys, and content inspection.
package files

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders for image.DecodeConfig
	_ "image/png"
	"net/http"
	"regexp"
	"strings"

	_ "golang.org/x/image/webp"
)

const (
	MaxBytes    = 50 << 20 // 50 MB per upload
	MaxSVGBytes = 5 << 20  // SVG is markup, not a print file
	// PrefixBytes is how much of the object the server reads to sniff the type
	// and the pixel size (JPEG metadata can precede the frame header).
	PrefixBytes = 4 << 20

	maxSide   = 30000       // px; rejects absurd canvases
	maxPixels = 200_000_000 // px; decompression-bomb guard for later processing
)

// Purposes maps a key namespace to who may upload into it (nil = any logged-in user).
var Purposes = map[string][]string{
	"designs":    {"designer", "admin"}, // creator Studio
	"custom":     nil,                   // customer Studio (private custom designs)
	"avatars":    nil,
	"complaints": nil,
}

// Extensions by sniffed MIME type.
var Extensions = map[string]string{
	"image/png":     "png",
	"image/jpeg":    "jpg",
	"image/webp":    "webp",
	"image/svg+xml": "svg",
}

// Raster reports whether mime is one of the pixel formats (everyone may upload those).
func Raster(mime string) bool { return mime == "image/png" || mime == "image/jpeg" || mime == "image/webp" }

var keyRe = regexp.MustCompile(`^([a-z]+)/[0-9a-f]{32}\.(png|jpg|webp|svg)$`)

// NewKey returns a random key namespaced by purpose: designs/<32 hex>.png
func NewKey(purpose, mime string) (string, error) {
	ext, ok := Extensions[mime]
	if !ok {
		return "", errors.New("jenis file tidak didukung")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return purpose + "/" + hex.EncodeToString(b) + "." + ext, nil
}

// LegacyPurpose namespaces files copied from the old local uploads/ folder by
// cmd/migrate-uploads. Nobody can presign into it; it exists only so that the
// URLs already stored in the database keep passing isHostedFile.
const LegacyPurpose = "legacy"

var legacyKeyRe = regexp.MustCompile(`^legacy/[A-Za-z0-9][A-Za-z0-9._-]{0,100}\.(png|jpe?g|webp|svg)$`)

// LegacyKey is the bucket key of an old uploads/<name> file; ok is false for
// names that are not plain image file names.
func LegacyKey(name string) (key string, ok bool) {
	key = LegacyPurpose + "/" + name
	return key, legacyKeyRe.MatchString(key) && !strings.Contains(name, "..")
}

// ParseKey validates a key produced by NewKey (or LegacyKey) and returns its purpose.
func ParseKey(key string) (purpose string, ok bool) {
	if strings.HasPrefix(key, LegacyPurpose+"/") {
		if _, ok := LegacyKey(key[len(LegacyPurpose)+1:]); ok {
			return LegacyPurpose, true
		}
		return "", false
	}
	m := keyRe.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	if _, known := Purposes[m[1]]; !known {
		return "", false
	}
	return m[1], true
}

// Info is what Inspect learned from the object's bytes.
type Info struct {
	Mime          string
	Width, Height int // 0 for SVG
}

// Inspect decides what the object really is from its first bytes (never from
// the client's claim) and reads the pixel size. prefix is the start of the
// object, size its full length.
func Inspect(prefix []byte, size int64) (Info, error) {
	if size <= 0 {
		return Info{}, errors.New("file kosong")
	}
	if size > MaxBytes {
		return Info{}, errors.New("file lebih dari 50 MB")
	}
	head := prefix
	if len(head) > 512 {
		head = head[:512]
	}
	switch mime := http.DetectContentType(head); mime {
	case "image/png", "image/jpeg", "image/webp":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(prefix))
		if err != nil {
			return Info{}, fmt.Errorf("gambar rusak atau tidak terbaca: %w", err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxSide || cfg.Height > maxSide ||
			int64(cfg.Width)*int64(cfg.Height) > maxPixels {
			return Info{}, fmt.Errorf("ukuran gambar tidak wajar (%d×%d px)", cfg.Width, cfg.Height)
		}
		return Info{Mime: mime, Width: cfg.Width, Height: cfg.Height}, nil
	default:
		if looksLikeSVG(prefix[:min(len(prefix), 4096)]) {
			if size > MaxSVGBytes {
				return Info{}, errors.New("SVG lebih dari 5 MB")
			}
			return Info{Mime: "image/svg+xml"}, nil
		}
		return Info{}, errors.New("isi file bukan gambar PNG, JPG, WebP, atau SVG")
	}
}

// looksLikeSVG accepts markup whose root element is <svg> (optionally after a
// BOM, an XML declaration, comments or a doctype).
func looksLikeSVG(head []byte) bool {
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	return bytes.Contains(head, []byte("<svg")) && bytes.HasPrefix(bytes.TrimSpace(head), []byte("<"))
}
