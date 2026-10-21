package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Migration 0020 rebuilds SQLite's upstreams to admit mode bundle (spec 0014); the rows of its five
// child tables, which cascade on delete, must survive the rebuild.
func TestMigration20KeepsUpstreamChildren(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		t.Fatal(err)
	}
	for {
		next, err := s.migrateOne(ctx, ms[:19])
		if err != nil {
			t.Fatal(err)
		}
		if !next {
			break
		}
	}
	// the tenant as the schema of the time has it (no holder_id: migration 0029)
	tn := Tenant{ID: NewID(), Name: "acme"}
	ch := Channel{Name: "prod", Kind: ChannelSigned}
	if err := s.InTx(ctx, "", func(tx *Tx) error {
		if _, err := tx.exec(ctx, "INSERT INTO tenants (id, name, display_name, state, storage_domain, created_at, version) VALUES (?, ?, '', 'active', 'default', ?, 1)",
			tn.ID, tn.Name, s.d.timeArg(s.Now())); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		return tx.CreateChannel(ctx, &ch)
	}); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO upstreams (id, tenant_id, name, kind, prefix, channel_id, mode, visibility, state, version, request_dry_run,
			created_at, created_by, credential) VALUES ('u1', ?, 'acme-src', 'repository', 'https://src.example.org/a/b', ?,
			'mirror', 'private', 'active', 3, 0, '2026-10-01T00:00:00.000000Z', 'os:1:t', 'cred')`,
		`INSERT INTO upstream_keys (upstream_id, fingerprint) VALUES ('u1', 'sha256:aa'), ('u1', 'sha256:bb')`,
		`INSERT INTO upstream_platforms (upstream_id, platform) VALUES ('u1', 'linux_amd64')`,
		`INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES ('u1', 'x', 0), ('u1', 'y', 1)`,
		`INSERT INTO upstream_versions (upstream_id, name, ext_version) VALUES ('u1', 'x', '1.0.0')`,
		`INSERT INTO upstream_cells (upstream_id, duckdb_version, platform, name, etag, body_hash, outcome, fetched_at)
			VALUES ('u1', 'v2.0.0', 'linux_amd64', 'x', '"e"', 'h', 'released', '2026-10-01T00:00:00.000000Z')`,
	}
	for i, q := range seed {
		args := []any{}
		if i == 0 {
			args = []any{tn.ID, ch.ID}
		}
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"upstreams": 1, "upstream_keys": 2, "upstream_platforms": 1, "upstream_entries": 2,
		"upstream_versions": 1, "upstream_cells": 1, "bundle_imports": 0} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows (%v), want %d", table, n, err, want)
		}
	}
	var leftovers int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'm20_%' OR name = 'upstreams_new'").Scan(&leftovers); err != nil || leftovers != 0 {
		t.Errorf("temporary tables left: %d (%v)", leftovers, err)
	}
	var u Upstream
	if err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		u, err = tx.GetUpstreamByID(ctx, "u1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if u.Credential != "cred" || u.Version != 3 || u.Prefix != "https://src.example.org/a/b" || u.BundleID != "" {
		t.Errorf("the upstream after the rebuild: %+v", u)
	}
	// the foreign keys still point at the rebuilt table: deleting the upstream cascades
	if _, err := s.db.ExecContext(ctx, "DELETE FROM upstreams WHERE id = 'u1'"); err != nil {
		t.Fatal(err)
	}
	var cells int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM upstream_cells").Scan(&cells); err != nil || cells != 0 {
		t.Errorf("cells after deleting their upstream: %d (%v)", cells, err)
	}
	b := Upstream{TenantID: tn.ID, Name: "air", Kind: UpstreamCore, ChannelID: ch.ID, Mode: ModeBundle, Visibility: Public,
		State: UpstreamActive, CreatedBy: "os:1:t"}
	if err := s.InTx(ctx, "", func(tx *Tx) error { return tx.InsertUpstream(ctx, &b) }); err != nil {
		t.Fatalf("a bundle-mode upstream: %v", err)
	}
}
