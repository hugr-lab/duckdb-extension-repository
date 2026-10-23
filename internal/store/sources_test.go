package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0032 (spec 0031) gives each release its source and each channel its selections, as the
// channels served before: a replacement before a mirror, the newest current one first.
func TestMigrationChannelSources(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 31); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
		later := store.TimeArg(s, time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		hash := func(c string) string { return strings.Repeat(c, 64) }
		const tid = "t0000000-0000-0000-0000-000000000000"
		build := func(b, name, h, origin string) []any {
			return []any{"INSERT INTO builds (id, tenant_id, name, ext_version, platform, abi, duckdb_version, body_hash, origin, created_at, created_by, checked) " +
				"VALUES (?, ?, ?, '1.0', 'linux_amd64', 'cpp', 'v2.0.0', ?, ?, ?, 'os:1:t', ?)", id(b), tid, name, hash(h), origin, now, true}
		}
		rel := func(r, ch, b, name, version, state, origin, by string, seq any, at any) []any {
			return []any{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, ext_version, platform, slot, state, visibility, seq, created_at, " +
				"created_by, state_changed_at, state_changed_by, version, origin) VALUES (?, ?, ?, ?, ?, ?, 'linux_amd64', 'v2.0.0', ?, 'public', ?, ?, ?, ?, ?, 1, ?)",
				id(r), tid, id(ch), id(b), name, version, state, seq, at, by, at, by, origin}
		}
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", id("c"), tid, now},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'staging', 'signed', ?, 1, 1)", id("d"), tid, now},
			{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, created_by) " +
				"VALUES (?, ?, 'repo', 'repository', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')", id("u"), tid, id("c"), false, now},
			{"INSERT INTO upstream_cells (upstream_id, duckdb_version, platform, name, body_hash, outcome, fetched_at) VALUES (?, 'v2.0.0', 'linux_amd64', 'acl', ?, 'released', ?)",
				id("u"), hash("1"), now},
			{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, 'ci', ?, 'os:1:t')", id("p"), tid, now},
			build("a", "acl", "1", "upstream"), build("b", "acl", "2", "admin"), build("e", "tresor", "3", "publication"),
			build("f", "gone", "4", "upstream"), build("g", "old", "5", "admin"),
			// prod: acl mirrored, then the tenant's acl (a replacement); tresor published by ci; old yanked
			rel("1", "c", "a", "acl", "1.0", "active", "upstream", "system:upstream:acme/repo", 1, now),
			rel("2", "c", "b", "acl", "2.0", "active", "admin", "os:1:t", nil, later),
			rel("3", "c", "e", "tresor", "1.0", "active", "publication", "publisher:acme/ci", 2, now),
			rel("4", "c", "g", "old", "1.0", "yanked", "admin", "os:1:t", 3, now),
			// staging: acl's mirrored build promoted; gone mirrored by an upstream since removed
			rel("5", "d", "a", "acl", "1.0", "active", "promotion", "os:1:t", 1, later),
			rel("6", "d", "f", "gone", "1.0", "active", "upstream", "system:upstream:acme/removed", 2, now),
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for r, want := range map[string]string{"1": "upstream:" + id("u"), "2": "tenant", "3": "publisher:" + id("p"), "4": "tenant",
			"5": "upstream:" + id("u"), "6": "upstream:"} {
			rs, err := s.ListReleases(ctx, map[string]string{"1": id("c"), "2": id("c"), "3": id("c"), "4": id("c"), "5": id("d"), "6": id("d")}[r], "")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, x := range rs {
				if x.ID == id(r) {
					found = true
					if x.Source != want {
						t.Errorf("release %s: source %q, want %q", r, x.Source, want)
					}
				}
			}
			if !found {
				t.Fatalf("release %s not listed", r)
			}
		}
		prod, err := s.ChannelSources(ctx, id("c"))
		if err != nil {
			t.Fatal(err)
		}
		// the replacement wins over the mirror; a yanked-only name has none
		if len(prod) != 2 || prod["acl"].Source != "tenant" || prod["tresor"].Source != "publisher:"+id("p") || prod["acl"].SetBy != "system:migration" {
			t.Fatalf("prod's selections: %+v", prod)
		}
		staging, err := s.ChannelSources(ctx, id("d"))
		if err != nil || len(staging) != 2 || staging["acl"].Source != "upstream:"+id("u") || staging["gone"].Source != "upstream:" {
			t.Fatalf("staging's selections: %+v %v", staging, err)
		}
	})
}

// Spec 0031: sources as answers name them, resolved from a caller's names, and a removed upstream.
func TestChannelSources(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fullTenant(t, s, "acme", "1")
		labels, err := s.SourceLabels(ctx, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		up := "1" + "0000000-0000-0000-0000-00000000000u"
		if store.Label(labels, "upstream:"+up) != "upstream:core" || store.Label(labels, "upstream:gone") != "upstream:(removed)" ||
			store.Label(labels, "tenant") != "tenant" || store.Label(labels, "publisher:x") != "publisher:(removed)" {
			t.Fatalf("labels %v", labels)
		}
		if src, err := s.ResolveSource(ctx, tn.ID, "upstream:core"); err != nil || src != "upstream:"+up {
			t.Fatalf("resolve: %q %v", src, err)
		}
		for _, bad := range []string{"upstream:nope", "publisher:nope", "core", ""} {
			if _, err := s.ResolveSource(ctx, tn.ID, bad); err != store.ErrNoSource {
				t.Errorf("%q: %v", bad, err)
			}
		}
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetChannelSource(ctx, ch.ID, "acl", "upstream:"+up, "os:1:t")
			if err != nil || before != "" {
				t.Fatalf("set: %q %v", before, err)
			}
			if before, err = tx.SetChannelSource(ctx, ch.ID, "acl", "tenant", "os:1:t"); err != nil || before != "upstream:"+up {
				t.Fatalf("switch: %q %v", before, err)
			}
			if _, err := tx.SetChannelSource(ctx, ch.ID, "acl", "bogus", "os:1:t"); err == nil {
				t.Fatal("a bogus source")
			}
			if removed, err := tx.UpstreamRemoved(ctx, "upstream:"+up); err != nil || removed {
				t.Fatalf("a live upstream removed: %v", err)
			}
			if removed, err := tx.UpstreamRemoved(ctx, "upstream:gone"); err != nil || !removed {
				t.Fatalf("a removed upstream: %v", err)
			}
			if before, err := tx.ClearChannelSource(ctx, ch.ID, "acl"); err != nil || before != "tenant" {
				t.Fatalf("clear: %q %v", before, err)
			}
			return nil
		})
	})
}
