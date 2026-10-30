package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0049 (spec 0039 PR 2) needs a reader at 49: a level-48 binary would fetch no
// discovered name, and count none in the tenant's cells.
func TestMigrationUpstreamNames(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 48); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 49 AND min_reader = 49"); err != nil || n != 1 {
			t.Fatalf("migration 0049 needs a reader at 49: %d %v", n, err)
		}
		if err := store.CheckAs(ctx, s, 48); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("a level-48 binary on a level-49 database: %v", err)
		}
	})
}

// Spec 0039 PR 2: a "*" mirror's names on every dialect: kept within the room, a cut listing
// removing none, a complete one removing those that left; a name listed later is its entry's, counted
// once; the upstream's removal takes them.
func TestUpstreamNames(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		at := store.TimeArg(s, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC))
		const tid, cid = "t0000000-0000-0000-0000-000000000000", "c0000000-0000-0000-0000-000000000000"
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, at},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, at},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatal(err)
			}
		}
		u := store.Upstream{TenantID: tid, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: cid,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchLazy,
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "*"}}}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
		set := func(names []string, complete bool, room int) int {
			t.Helper()
			var over int
			inTx(t, s, func(tx *store.Tx) error {
				cur, err := tx.GetUpstreamByID(ctx, u.ID)
				if err != nil {
					return err
				}
				_, over, err = tx.SetUpstreamNames(ctx, cur, names, complete, room)
				return err
			})
			return over
		}
		get := func() store.Upstream {
			t.Helper()
			var cur store.Upstream
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				cur, err = tx.GetUpstreamByID(ctx, u.ID)
				return err
			})
			return cur
		}
		if over := set([]string{"a", "b", "c"}, true, 2); over != 1 || !slices.Equal(get().Discovered, []string{"a", "b"}) {
			t.Fatalf("within the room: %d %v", over, get().Discovered)
		}
		// a cut listing removes nothing and adds within the room
		if over := set([]string{"c", "d"}, false, 3); over != 1 || !slices.Equal(get().Discovered, []string{"a", "b", "c"}) {
			t.Fatalf("a cut listing: %d %v", over, get().Discovered)
		}
		// a complete one removes those that left
		if set([]string{"b", "c"}, true, 3); !slices.Equal(get().Discovered, []string{"b", "c"}) {
			t.Fatalf("a complete listing: %v", get().Discovered)
		}
		// b listed: its entry's, counted once
		inTx(t, s, func(tx *store.Tx) error { return tx.PutUpstreamEntry(ctx, u.ID, store.UpstreamEntry{Name: "b"}) })
		if cur := get(); !slices.Equal(cur.Discovered, []string{"c"}) || len(cur.Names()) != 2 {
			t.Fatalf("b listed: %v", cur.Names())
		}
		cur := get()
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteUpstream(ctx, cur, 0) })
		if n, err := store.CountRaw(ctx, s, "upstream_names", "1 = 1"); err != nil || n != 0 {
			t.Fatalf("names left: %d %v", n, err)
		}
	})
}

// Spec 0039 PR 2: the tenant's own release of a name holds it from "*" only in its package type: a
// driver named geo takes no DuckDB extension geo.
func TestOtherSourcesByType(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		unregister := pkgtype.RegisterForTest(testType)
		defer func() { unregister() }()
		s := e.Open(t)
		at := store.TimeArg(s, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC))
		tn := store.Tenant{Name: "acme"}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateTenant(ctx, &tn) })
		drivers := store.Channel{TenantID: tn.ID, Name: "drivers", Kind: store.ChannelSigned, PackageType: testType}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &drivers) })
		const bid, rid = "b0000000-0000-0000-0000-000000000000", "r0000000-0000-0000-0000-000000000000"
		for _, q := range [][]any{
			{"INSERT INTO builds (id, tenant_id, package_type, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked) VALUES (?, ?, ?, 'tenant', 'geo', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'admin', ?, 'os:1:t', ?)",
				bid, tn.ID, testType, strings.Repeat("d", 64), at, true},
			{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state, visibility, seq, created_at, " +
				"created_by, state_changed_at, state_changed_by, version, origin, source) VALUES (?, ?, ?, ?, 'geo', '1.0', 'linux_amd64', 'v2.0.0', 'active', " +
				"'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'admin', 'tenant')", rid, tn.ID, drivers.ID, bid, at, at},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		for typ, want := range map[string]bool{pkgtype.DuckDBExtension: false, testType: true} {
			inTx(t, s, func(tx *store.Tx) error {
				names, err := tx.TenantNamesOfOtherSources(ctx, tn.ID, typ)
				if err != nil {
					return err
				}
				held, err := tx.OtherSourceHolds(ctx, tn.ID, typ, "geo")
				if err == nil && (names["geo"] != want || held != want) {
					t.Errorf("%s: names %v, holds %v", typ, names, held)
				}
				return err
			})
		}
	})
}
