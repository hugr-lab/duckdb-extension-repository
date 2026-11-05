package upstream_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// retire retires a release in prod as the retention pass does (spec 0042).
func (en *env) retire(t *testing.T, id string) {
	t.Helper()
	ch, err := en.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := en.st.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
		r, err := tx.GetRelease(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		if err := tx.RetireRelease(ctx, &r, "system:release-retention"); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	}); err != nil {
		t.Fatal(err)
	}
}

// Spec 0042: a mirror's run finding its cell's release retired records the outcome retired and
// leaves it, as a yanked one.
func TestMirrorRetired(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		rels := en.releases(t, "tresor")
		en.retire(t, rels[0].ID)
		en.repo.touch("v2.0.0", "linux_amd64", "tresor")
		if r := en.sync(t, false); r.Counts[store.CellRetired] != 1 {
			t.Fatalf("a retired release: %+v", r)
		}
		if c := en.cell(t, "tresor"); c.Outcome != store.CellRetired {
			t.Fatalf("the cell: %+v", c)
		}
		if rs := en.releases(t, "tresor"); len(rs) != 1 || rs[0].State != store.ReleaseRetired {
			t.Fatalf("the run changed the retired release: %+v", rs)
		}
	})
}

// Spec 0042: an upstream's yank of a retired release keeps retired_from and retired_at through the
// yank (prior_state retired), and its lift returns it to retired with a new grace (retired_at the lift).
func TestRevocationRetired(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		k := en.kista()
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		rel := en.releases(t, "tresor")[0]
		en.retire(t, rel.ID)
		retiredAt := en.releases(t, "tresor")[0].RetiredAt
		if _, err := en.up.SetRevocations(ctx, admin, "acme", "acme-repo", true, 0); err != nil {
			t.Fatal(err)
		}
		k.set(row(rel, store.RevokeYanked, ""))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 {
			t.Fatalf("the yank: %+v", r)
		}
		got := en.releases(t, "tresor")[0]
		if got.State != store.ReleaseYanked || got.RetiredFrom != store.ReleaseActive || !got.RetiredAt.Equal(retiredAt) {
			t.Fatalf("yanked: %+v", got)
		}
		if n, err := en.up.LiftRevocation(ctx, admin, "acme", "acme-repo", rel.BuildID, 0); err != nil || n != 1 {
			t.Fatalf("lift: %d %v", n, err)
		}
		got = en.releases(t, "tresor")[0]
		if got.State != store.ReleaseRetired || got.RetiredFrom != store.ReleaseActive || !got.RetiredAt.After(retiredAt) {
			t.Fatalf("lifted: %+v (retired at %v before)", got, retiredAt)
		}
	})
}

// Spec 0042: a bundle's cell whose release is retired records the outcome retired and leaves it.
func TestImportRetired(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		if tr := target(t, en.importB(t, bu, data, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first import: %+v", tr)
		}
		rs := en.releases(t, "x")
		en.retire(t, rs[0].ID)
		again, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		if tr := target(t, en.importB(t, bu, again, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellRetired] != 1 {
			t.Fatalf("a retired release: %+v", tr)
		}
		if c := en.cellOf(t, "air", "x", "v2.0.0"); c.Outcome != store.CellRetired {
			t.Fatalf("the cell: %+v", c)
		}
		if rs := en.releases(t, "x"); len(rs) != 1 || rs[0].State != store.ReleaseRetired {
			t.Fatalf("the import changed the retired release: %+v", rs)
		}
	})
}
