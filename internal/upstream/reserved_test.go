package upstream_test

import (
	"slices"
	"testing"

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
		if en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_later") != u.ID {
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
		if en.snapshot(t, "prod").PullThroughFor("v2.0.0", "linux_amd64", "kista_later") != "" {
			t.Fatal("a reserved name is taken by * of a repository upstream")
		}
	})
}
