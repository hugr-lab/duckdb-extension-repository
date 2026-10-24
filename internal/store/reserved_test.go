package store_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// TestReservedNames is spec 0030's list: migration 0030's seed, a list that only grows (community
// may become core), a version counting its changes, and only administrators' names removable.
func TestReservedNames(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		names, v0, err := s.ReservedNames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(names, reserved.Seed()) || v0 < 1 {
			t.Fatalf("the seed: %d names at version %d, the compiled lists hold %d", len(names), v0, len(reserved.Seed()))
		}
		list, err := s.ListReservedNames(ctx, "legacy_parser")
		if err != nil || len(list) != 1 || list[0].Kind != store.ReservedCore || !slices.Equal(list[0].Sources, []string{"builtin"}) {
			t.Fatalf("legacy_parser: %+v %v", list, err)
		}
		reserve := func(name, kind, source string) bool {
			t.Helper()
			var changed bool
			inTx(t, s, func(tx *store.Tx) (err error) {
				if changed, err = tx.ReserveName(ctx, name, kind, source); err != nil || !changed {
					return err
				}
				return tx.BumpReservedVersion(ctx)
			})
			return changed
		}
		version := func() int64 {
			t.Helper()
			v, err := s.ReservedVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		// a new name, then the same name from another source: one change
		if !reserve("kista_x", store.ReservedCommunity, "upstream:acme/community") || version() != v0+1 {
			t.Fatalf("a new name: version %d", version())
		}
		if reserve("kista_x", store.ReservedCommunity, "community-sync@0123456") || version() != v0+1 {
			t.Fatal("a second source changed the list")
		}
		// community becomes core, never the other way
		if !reserve("kista_x", store.ReservedCore, "version:v2.1.0@abcdef0") || version() != v0+2 {
			t.Fatal("community did not become core")
		}
		if reserve("kista_x", store.ReservedCommunity, "admin:os:1:t") || version() != v0+2 {
			t.Fatal("core became community")
		}
		inTx(t, s, func(tx *store.Tx) error {
			if k, err := tx.ReservedKind(ctx, "kista_x"); err != nil || k != store.ReservedCore {
				t.Errorf("kind %q %v", k, err)
			}
			if k, err := tx.ReservedKind(ctx, "kista_none"); err != nil || k != "" {
				t.Errorf("an unreserved name: %q %v", k, err)
			}
			return nil
		})
		list, _ = s.ListReservedNames(ctx, "kista_x")
		if want := []string{"admin:os:1:t", "community-sync@0123456", "upstream:acme/community", "version:v2.1.0@abcdef0"}; len(list) != 1 ||
			!slices.Equal(list[0].Sources, want) {
			t.Fatalf("sources: %+v", list)
		}
		for _, bad := range [][2]string{{"core", "nobody:x"}, {"core", "admin:"}, {"core", "admin@x"}, {"core", "community-sync@main"}, {"core", "admin:a b"}, {"tenant", "admin:x"},
			{"core", "admin:" + strings.Repeat("a", 381)}} {
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.ReserveName(ctx, "kista_y", bad[0], bad[1]); return err }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%v: %v", bad, err)
			}
		}
		// only a name administrators alone reserved goes
		remove := func(name string) ([]string, error) {
			var sources []string
			err := s.InTx(ctx, "", func(tx *store.Tx) (err error) { sources, err = tx.RemoveReservedName(ctx, name); return })
			return sources, err
		}
		for _, n := range []string{"kista_x", "legacy_parser"} {
			if _, err := remove(n); !errors.Is(err, store.ErrConflict) {
				t.Errorf("%s removed: %v", n, err)
			}
		}
		if _, err := remove("kista_none"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an unreserved name: %v", err)
		}
		reserve("kista_admin", store.ReservedCommunity, "admin:os:1:t")
		reserve("kista_admin", store.ReservedCommunity, "admin:os:2:u")
		before := version()
		if sources, err := remove("kista_admin"); err != nil || len(sources) != 2 || version() != before+1 {
			t.Fatalf("an administrator's name: %v %v", sources, err)
		}
		if names, _, _ := s.ReservedNames(ctx); names["kista_admin"] != "" || names["kista_x"] != store.ReservedCore {
			t.Fatal("the list after a removal")
		}
		// the tenants with releases under a name
		tn, ch := fixture(t, s)
		b := cppBuild(tn, "kista_x", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: b.PackageVersion,
			Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r, "os:1:t") })
		if ts, err := s.TenantsWithReleases(ctx, "kista_x"); err != nil || !slices.Equal(ts, []string{"acme"}) {
			t.Fatalf("tenants: %v %v", ts, err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r, store.ReleaseYanked, "os:1:t") })
		if ts, err := s.TenantsWithReleases(ctx, "kista_x"); err != nil || len(ts) != 0 {
			t.Fatalf("a yanked release counted: %v %v", ts, err)
		}
	})
}

// TestTenantReservedNames is spec 0030 part 2's table and the one lookup: DuckDB's names, then the
// tenant's, then its upstreams'.
func TestTenantReservedNames(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		s := e.Open(t)
		tn, ch := fixture(t, s)
		r := store.TenantReservedName{TenantID: tn.ID, Name: "acme_tool", Holder: "enterest", AddedBy: "os:1:t"}
		reserve := func(r store.TenantReservedName) (prev string, changed bool) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) (err error) { prev, changed, err = tx.ReserveTenantName(ctx, &r); return })
			return
		}
		if prev, changed := reserve(r); prev != "" || !changed {
			t.Fatalf("new: %q %v", prev, changed)
		}
		if prev, changed := reserve(r); prev != "enterest" || changed {
			t.Fatalf("again: %q %v", prev, changed)
		}
		r.Holder = "other"
		if prev, changed := reserve(r); prev != "enterest" || !changed {
			t.Fatalf("another holder: %q %v", prev, changed)
		}
		for _, bad := range []string{"", "a\nb", "a\tb", "\x1b[31m", strings.Repeat("h", store.MaxHolder+1)} {
			r.Holder = bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { _, _, err := tx.ReserveTenantName(ctx, &r); return err }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("holder %q: %v", bad, err)
			}
		}
		if l, err := s.ListTenantReservedNames(ctx, tn.ID, ""); err != nil || len(l) != 1 || l[0].Holder != "other" || l[0].AddedBy != "os:1:t" {
			t.Fatalf("list: %+v %v", l, err)
		}
		set, _ := s.TenantReservedSet(ctx, tn.ID)
		inTx(t, s, func(tx *store.Tx) error {
			if held, err := tx.TenantReserves(ctx, tn.ID, "acme_tool"); err != nil || !held || !set["acme_tool"] {
				t.Errorf("reserves: %v %v %v", held, set, err)
			}
			return nil
		})
		// the lookup, in precedence
		u := store.Upstream{TenantID: tn.ID, Name: "vendor", Kind: store.UpstreamRepo, ChannelID: ch.ID, Mode: store.ModeMirror,
			Visibility: "public", State: store.UpstreamActive, Entries: []store.UpstreamEntry{{Name: "vendor_ext"}, {Name: "acme_tool"}, {Name: "json"}}}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
		for name, want := range map[string]string{"json": store.ReservedCore, "acme_tool": store.ReservedTenant, "vendor_ext": store.ReservedUpstream,
			"free_ext": "", "legacy_parser": store.ReservedCore} {
			if got, err := s.ReservedFor(ctx, "acme", name); err != nil || got != want {
				t.Errorf("%s: %q, want %q (%v)", name, got, want, err)
			}
		}
		if got, _ := s.ReservedFor(ctx, "other-tenant", "acme_tool"); got != "" {
			t.Errorf("another tenant's name: %q", got)
		}
		// the name's live releases in the tenant
		b := cppBuild(tn, "acme_tool", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		rel := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: b.PackageVersion,
			Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &rel, "os:1:t") })
		if l, err := s.TenantLiveReleases(ctx, tn.ID, "acme_tool"); err != nil || len(l) != 1 || l[0].Channel != "prod" ||
			l[0].Version != "1.0" || l[0].Origin != store.OriginAdmin {
			t.Fatalf("live releases: %+v %v", l, err)
		}
		// released, and once more
		inTx(t, s, func(tx *store.Tx) error {
			if got, err := tx.ReleaseTenantName(ctx, tn.ID, "acme_tool"); err != nil || got.Holder != "other" {
				t.Errorf("release: %+v %v", got, err)
			}
			return nil
		})
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.ReleaseTenantName(ctx, tn.ID, "acme_tool"); return err }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("released twice: %v", err)
		}
		if got, _ := s.ReservedFor(ctx, "acme", "acme_tool"); got != store.ReservedUpstream {
			t.Fatalf("after its release, the upstream's: %q", got)
		}
	})
}
