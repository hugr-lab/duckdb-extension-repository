package upstream_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// TestIntakeReservesNames is spec 0030's learning: a build mirrored from DuckDB's core repository
// reserves its name as core, in the release's transaction, with the upstream as its source.
func TestIntakeReservesNames(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0)) // the process's view follows this test's store
		en.repo.put("v2.0.0", "linux_amd64", "kista_newcore", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "kista_newcore_duckdb_cpp_init"))
		en.passthrough(t, "kista_newcore")
		before, err := en.st.ReservedVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r := en.syncCore(t); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the run: %+v", r)
		}
		list, err := en.st.ListReservedNames(ctx, "kista_newcore")
		if err != nil || len(list) != 1 || list[0].Kind != store.ReservedCore || !slices.Equal(list[0].Sources, []string{"upstream:acme/core"}) {
			t.Fatalf("learned: %+v %v", list, err)
		}
		if v, _ := en.st.ReservedVersion(ctx); v != before+1 {
			t.Fatalf("version %d after %d", v, before)
		}
		if err := reserved.Sync(ctx, en.st); err != nil || reserved.Kind("kista_newcore") != reserved.Core {
			t.Fatalf("the view: %q %v", reserved.Kind("kista_newcore"), err)
		}
	})
}

// TestPullThroughFollowsReservations: a name reserved after a replica last read the list is out of
// "*" of a repository upstream once the view follows the server's list (the community case:
// release.TestAnyTakes).
func TestPullThroughFollowsReservations(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "*"}))
		if err != nil {
			t.Fatal(err)
		}
		if ptID(en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_later")) != u.ID {
			t.Fatal("an unreserved name is not taken by *")
		}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if _, err := tx.ReserveName(ctx, "kista_later", store.ReservedCommunity, "admin:os:1:t"); err != nil {
					return err
				}
				return tx.BumpReservedVersion(ctx)
			})
		})
		if err := reserved.Sync(ctx, en.st); err != nil {
			t.Fatal(err)
		}
		if ptID(en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_later")) != "" {
			t.Fatal("a reserved name is taken by * of a repository upstream")
		}
	})
}

// TestTenantReservedUpstreams: a name reserved in the tenant (spec 0030 part 2) is never taken by
// "*", and an entry for it needs allow_reserved.
func TestTenantReservedUpstreams(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "*"}))
		if err != nil {
			t.Fatal(err)
		}
		if ptID(en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_held")) != u.ID {
			t.Fatal("a free name is not taken by *")
		}
		if _, err := en.ten.ReserveInTenant(ctx, admin, "acme", "kista_held", "enterest"); err != nil {
			t.Fatal(err)
		}
		if ptID(en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_held")) != "" {
			t.Fatal("a tenant-reserved name is taken by *")
		}
		// a miss queued, the name reserved before the fetch: the insert refuses it under its locks
		en.repo.put("v2.0.0", "linux_amd64", "kista_queued", signed(t, en.key, cpp("1.0", "v2.0.0"), 9, "kista_queued_duckdb_cpp_init"))
		p := en.puller()
		p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "kista_queued"})
		if _, err := en.ten.ReserveInTenant(ctx, admin, "acme", "kista_queued", "enterest"); err != nil {
			t.Fatal(err)
		}
		p.Drain(ctx)
		if rs := en.releases(t, "kista_queued"); len(rs) != 0 {
			t.Fatalf("a name reserved while its miss was queued was pulled: %+v", rs)
		}
		mirror := en.spec(store.UpstreamEntry{Name: "kista_held"})
		mirror.Name = "mirror"
		if _, err := en.up.Add(ctx, admin, "acme", mirror); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an entry for a tenant-reserved name: %v", err)
		}
		mirror.Entries[0].AllowReserved = true
		if _, err := en.up.Add(ctx, admin, "acme", mirror); err != nil {
			t.Fatalf("with allow_reserved: %v", err)
		}
		if _, err := en.up.PutEntry(ctx, admin, "acme", "mirror", store.UpstreamEntry{Name: "kista_held"}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an entry put without allow_reserved: %v", err)
		}
	})
}
