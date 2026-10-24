package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A SQLite migration marked foreign_keys_off runs with them off, but one that leaves a broken
// reference is rolled back and not recorded, and the connection has them on again afterwards.
func TestMigrationForeignKeysOff(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		t.Fatal(err)
	}
	broken := migration{version: len(ms) + 1, name: "broken", minReader: ms[len(ms)-1].minReader, foreignKeysOff: true,
		statements: []string{"INSERT INTO release_signatures (release_id, channel_id, key_id, signature, created_at) " +
			"VALUES ('nope', 'nope', 'nope', zeroblob(256), '2026-10-24T00:00:00.000000Z')"}}
	_, err = s.migrateOne(ctx, append(ms, broken))
	if err == nil || !strings.Contains(err.Error(), "release_signatures") {
		t.Fatalf("a broken reference: %v", err)
	}
	if cur, _, err := s.schemaState(ctx, s.db); err != nil || cur != len(ms) {
		t.Fatalf("recorded: %d %v", cur, err)
	}
	// every pooled connection enforces foreign keys again
	for range 4 {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO release_signatures (release_id, channel_id, key_id, signature, created_at) "+
			"VALUES ('nope', 'nope', 'nope', zeroblob(256), '2026-10-24T00:00:00.000000Z')"); err == nil {
			t.Fatal("a broken reference inserted after the migration")
		}
	}
	// a sound one applies
	sound := broken
	sound.statements = []string{"CREATE TABLE probe (id TEXT)"}
	if applied, err := s.migrateOne(ctx, append(ms, sound)); err != nil || !applied {
		t.Fatalf("a sound migration: %v %v", applied, err)
	}
}
