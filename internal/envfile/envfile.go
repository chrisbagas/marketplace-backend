// Package envfile memuat file .env sederhana (KEY=VALUE) ke environment proses.
// Hanya untuk dev: variabel yang sudah di-set di environment TIDAK ditimpa,
// jadi di produksi nilai asli dari server/hosting selalu menang.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// Load membaca path; file tidak ada = bukan galat. Mengembalikan jumlah variabel yang di-set.
func Load(path string) (int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || key == "" {
			continue
		}
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 { // komentar di akhir baris
			val = strings.TrimSpace(val[:i])
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}
