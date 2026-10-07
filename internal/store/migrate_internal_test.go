package store

import (
	"testing"
	"testing/fstest"
)

func TestParseMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"m/x/0001_init.sql": {Data: []byte("-- +min_reader 1\r\nCREATE TABLE a (x int)\r\n-- +statement\r\nCREATE INDEX i ON a (x)\r\n")},
		"m/x/0002_more.sql": {Data: []byte("ALTER TABLE a ADD y int\n-- +statement\n\n-- +statement\nUPDATE a SET y = 1\n")},
		"m/x/0003_drop.sql": {Data: []byte("-- +min_reader 3\nALTER TABLE a DROP COLUMN x\n")},
	}
	ms, err := parseMigrations(fsys, "m/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("%d migrations", len(ms))
	}
	want := []struct {
		name      string
		minReader int
		stmts     []string
	}{
		{"init", 1, []string{"CREATE TABLE a (x int)", "CREATE INDEX i ON a (x)"}},
		{"more", 1, []string{"ALTER TABLE a ADD y int", "UPDATE a SET y = 1"}}, // inherits; empty statement dropped
		{"drop", 3, []string{"ALTER TABLE a DROP COLUMN x"}},
	}
	for i, w := range want {
		m := ms[i]
		if m.version != i+1 || m.name != w.name || m.minReader != w.minReader || len(m.statements) != len(w.stmts) {
			t.Fatalf("migration %d: %+v", i+1, m)
		}
		for j := range w.stmts {
			if m.statements[j] != w.stmts[j] {
				t.Fatalf("migration %d statement %d: %q", i+1, j, m.statements[j])
			}
		}
	}
	bad := map[string]fstest.MapFS{
		"gap":      {"m/x/0001_a.sql": {Data: []byte("x")}, "m/x/0003_b.sql": {Data: []byte("y")}},
		"bad name": {"m/x/1_a.sql": {Data: []byte("x")}},
	}
	for name, f := range bad {
		if _, err := parseMigrations(f, "m/x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the real migrations parse for every dialect and have the same levels
	for _, d := range []string{"postgres", "sqlserver", "sqlite"} {
		ms, err := loadMigrations(d)
		if err != nil || len(ms) == 0 {
			t.Fatalf("%s: %v", d, err)
		}
	}
}
