package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("\ufeff# komentar\nKK_A=satu\nKK_B=\"dua tiga\"\nexport KK_C='empat' \nKK_D=lima # catatan\nKK_E=sudah-ada\nKK_F=a=b\n\nrusak\n"), 0o600)
	os.Setenv("KK_E", "dari-env")
	for _, k := range []string{"KK_A", "KK_B", "KK_C", "KK_D", "KK_F"} {
		os.Unsetenv(k)
	}
	n, err := Load(path)
	if err != nil || n != 5 {
		t.Fatalf("Load = %d, %v; want 5, nil", n, err)
	}
	want := map[string]string{"KK_A": "satu", "KK_B": "dua tiga", "KK_C": "empat", "KK_D": "lima", "KK_E": "dari-env", "KK_F": "a=b"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if n, err := Load(filepath.Join(t.TempDir(), "tidak-ada")); n != 0 || err != nil {
		t.Errorf("file tidak ada: %d, %v", n, err)
	}
}
