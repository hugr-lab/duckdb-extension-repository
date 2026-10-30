package upstream_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

func (en *env) discovered(t *testing.T) []string {
	t.Helper()
	u, err := en.up.Get(ctx, admin, "acme", "acme-repo")
	if err != nil {
		t.Fatal(err)
	}
	return u.Discovered
}

func (en *env) provides(t *testing.T, name string) bool {
	t.Helper()
	ok, err := en.st.UpstreamProvides(ctx, en.tenantID(t), pkgtype.DuckDBExtension, name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Spec 0039 PR 2: "*" on a mirror of a kista channel takes the names the remote's -/names lists,
// except what a pull-through "*" never takes (a reserved name, a bad name), names another upstream
// of the channel lists, and names a source of the tenant other than an upstream holds; they join
// the matrix as listed names and are fetched. A name reserves the tenant's name only once fetched.
// A failed or cut listing deletes nothing; a complete one removes a name that left, its releases
// kept. "*" is refused on a mirror whose remote is not a kista, and its removal forgets the names.
func TestDiscovery(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		// not a kista: "*" refused
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "*"})); !errors.Is(err, store.ErrInvalid) ||
			!strings.Contains(err.Error(), "not a kista") {
			t.Fatalf("a mirror of no kista with *: %v", err)
		}
		en.kista()
		for _, n := range []string{"tresor", "geo"} {
			en.repo.put("v2.0.0", "linux_amd64", n, signed(t, en.key, cpp("1.0", "v2.0.0"), len(n), n+"_duckdb_cpp_init"))
		}
		// the tenant's own build of "own"; another upstream of the channel lists "other"
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(signed(t, nil, cpp("1.0", "v2.0.0"), 9, "own_duckdb_cpp_init")),
			release.AddOptions{Name: "own"}); err != nil {
			t.Fatal(err)
		}
		other := en.spec(store.UpstreamEntry{Name: "other"})
		other.Name = "second"
		if _, err := en.up.Add(ctx, admin, "acme", other); err != nil {
			t.Fatal(err)
		}
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesPage = []string{"tresor", "geo", "zzz", "json", "own", "other", "Bad"}, 2
		en.repo.mu.Unlock()
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "*"}))
		if err != nil || !u.RemoteKista || !u.AnyMirror() {
			t.Fatalf("add: %+v %v", u, err)
		}
		r := en.sync(t, false)
		if r.Counts["discovered"] != 3 || r.Counts[store.CellReleased] != 2 || r.Counts[store.CellMissing] != 1 {
			t.Fatalf("run: %+v", r)
		}
		if got := en.discovered(t); !slices.Equal(got, []string{"geo", "tresor", "zzz"}) {
			t.Fatalf("discovered: %v", got)
		}
		if len(en.releases(t, "geo")) != 1 || len(en.releases(t, "tresor")) != 1 {
			t.Fatalf("releases: %v %v", en.releases(t, "geo"), en.releases(t, "tresor"))
		}
		// a name reserves the tenant's only once fetched
		if !en.provides(t, "geo") || en.provides(t, "zzz") {
			t.Fatalf("provides: geo %v zzz %v", en.provides(t, "geo"), en.provides(t, "zzz"))
		}
		// the matrix's cells count the discovered names
		var cells int
		if err := en.st.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			cells, err = tx.TenantCells(ctx, en.tenantID(t), "")
			return err
		}); err != nil || cells != 3+1 {
			t.Fatalf("tenant cells: %d %v", cells, err)
		}
		// another upstream of the channel listing a discovered name is a collision
		if _, err := en.up.PutEntry(ctx, admin, "acme", "second", store.UpstreamEntry{Name: "geo"}); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("listing a discovered name: %v", err)
		}

		// a failed listing, and a cut one, delete nothing
		en.repo.mu.Lock()
		en.repo.namesStatus = http.StatusBadGateway
		en.repo.mu.Unlock()
		if r := en.sync(t, false); !hasProblem(r, "names") || !slices.Equal(en.discovered(t), []string{"geo", "tresor", "zzz"}) {
			t.Fatalf("a failed listing: %+v %v", r, en.discovered(t))
		}
		// a complete listing without geo removes it; its release stays
		en.repo.mu.Lock()
		en.repo.namesStatus, en.repo.names = 0, []string{"tresor", "zzz"}
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts["discovered"] != 2 || !slices.Equal(en.discovered(t), []string{"tresor", "zzz"}) {
			t.Fatalf("geo left: %+v %v", r, en.discovered(t))
		}
		if len(en.releases(t, "geo")) != 1 || en.provides(t, "geo") {
			t.Fatalf("geo's release kept, not provided: %v %v", en.releases(t, "geo"), en.provides(t, "geo"))
		}
		// the API names them; "*" removed forgets them
		if _, err := en.up.Get(ctx, admin, "acme", "acme-repo"); err != nil {
			t.Fatal(err)
		}
		if err := en.up.RemoveEntry(ctx, admin, "acme", "acme-repo", "*"); err != nil {
			t.Fatal(err)
		}
		if got := en.discovered(t); len(got) != 0 {
			t.Fatalf("after * removed: %v", got)
		}
	})
}

// Spec 0039 PR 2: "*" keeps at most 1,000 names, counted after the checks: a reserved name past the
// thousandth costs nothing, one more is a problem and the listing deletes nothing.
func TestDiscoveryCap(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.kista()
		names := []string{"json"} // reserved: skipped before the count
		for i := range store.MaxDiscoveredNames {
			names = append(names, fmt.Sprintf("x%04d", i))
		}
		en.repo.mu.Lock()
		en.repo.names = names
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"})
		sp.FetchMode = store.FetchLazy // nothing asked: nothing fetched
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts["discovered"] != store.MaxDiscoveredNames || hasProblem(r, "names") {
			t.Fatalf("1,000 after the checks: %+v", r)
		}
		// x0000 leaves, two new names come: 1,001 after the checks, one over the cap; a listing over
		// the cap deletes nothing, so x0000 stays
		en.repo.mu.Lock()
		en.repo.names = append(slices.Clone(names[2:]), "y0000", "y0001")
		en.repo.mu.Unlock()
		r := en.sync(t, false)
		if !hasProblem(r, "names") || r.Counts["discovered"] != store.MaxDiscoveredNames {
			t.Fatalf("over the cap: %+v", r)
		}
		if got := en.discovered(t); !slices.Contains(got, "x0000") {
			t.Fatalf("x0000 deleted by a listing over the cap")
		}
	})
}

func hasProblem(r upstream.Result, outcome string) bool {
	return slices.ContainsFunc(r.Problems, func(p upstream.CellNote) bool { return p.Outcome == outcome })
}
