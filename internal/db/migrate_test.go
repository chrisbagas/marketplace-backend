package db

import "testing"

func mig(v int, name string) migration { return migration{Version: v, Name: name} }

func versions(ms []migration) []int {
	out := []int{}
	for _, m := range ms {
		out = append(out, m.Version)
	}
	return out
}

func TestPendingMigrationsGapGetsApplied(t *testing.T) {
	all := []migration{mig(1, "schema.sql"), mig(12, "012_a.sql"), mig(13, "013_b.sql"), mig(15, "015_search.sql")}
	// 15 was applied on a branch lacking 12-13; a later merge must still run them.
	got, err := pendingMigrations(all, map[int]bool{1: true, 15: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := versions(got); len(v) != 2 || v[0] != 12 || v[1] != 13 {
		t.Fatalf("pending = %v, want [12 13]", v)
	}
}

func TestPendingMigrationsSortsAscending(t *testing.T) {
	all := []migration{mig(3, "003.sql"), mig(1, "schema.sql"), mig(2, "002.sql")}
	got, err := pendingMigrations(all, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if v := versions(got); len(v) != 3 || v[0] != 1 || v[1] != 2 || v[2] != 3 {
		t.Fatalf("pending = %v, want [1 2 3]", v)
	}
}

func TestPendingMigrationsAllApplied(t *testing.T) {
	all := []migration{mig(1, "schema.sql"), mig(2, "002_a.sql")}
	got, err := pendingMigrations(all, map[int]bool{1: true, 2: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("pending = %v, want none", versions(got))
	}
}

func TestPendingMigrationsDuplicatePrefix(t *testing.T) {
	all := []migration{mig(1, "schema.sql"), mig(5, "005_a.sql"), mig(5, "005_b.sql")}
	if _, err := pendingMigrations(all, map[int]bool{}); err == nil {
		t.Fatal("expected error for duplicate version")
	}
}

func TestMigrationVersion(t *testing.T) {
	if v, err := migrationVersion("015_search.sql"); err != nil || v != 15 {
		t.Fatalf("015_search.sql = %d, %v; want 15", v, err)
	}
	for _, bad := range []string{"search.sql", "_x.sql", "abc_x.sql", "001_x.sql", "000_x.sql"} {
		if _, err := migrationVersion(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestEmbeddedMigrationsValid(t *testing.T) {
	all, err := migrationList()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pendingMigrations(all, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
}
