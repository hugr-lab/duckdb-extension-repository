package upstream_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0039 PR 2: discovered names fit the matrix's caps beside the explicit entries; the decision
// is stored, so the run, the tenant's count and the asks follow it, and a name that sorts first does
// not push out one kept.
func TestDiscoveryRoom(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.kista()
		var names []string
		for i := range 600 {
			names = append(names, fmt.Sprintf("n%03d", i))
		}
		en.repo.mu.Lock()
		en.repo.names = names
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"}, store.UpstreamEntry{Name: "aaa"})
		sp.FetchMode = store.FetchLazy
		sp.Platforms = nil
		for i := range 40 { // 40 cells a name: 20,000 / 40 = 500, one of them the explicit entry's
			sp.Platforms = append(sp.Platforms, fmt.Sprintf("p%02d", i))
		}
		en.up.Config.Interval = time.Hour // a run made due runs (lazy: nothing asked, nothing fetched)
		u, err := en.up.Add(ctx, admin, "acme", sp)
		if err != nil {
			t.Fatal(err)
		}
		r := en.runDue(t, u)
		got := en.discovered(t)
		if r.Error != "" || !hasProblem(r, "names") || len(got) != 499 || got[498] != "n498" {
			t.Fatalf("the room: %+v, %d names", r, len(got))
		}
		var cells int
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				cells, err = tx.TenantCells(ctx, en.tenantID(t), "")
				return err
			})
		})
		if cells != store.MaxUpstreamCells {
			t.Fatalf("tenant cells: %d", cells)
		}
		// the tenant's other upstreams are not refused for what "*" left out
		other := en.spec(store.UpstreamEntry{Name: "geo"})
		other.Name, other.Channel = "second", "other"
		if _, err := en.up.Add(ctx, admin, "acme", other); err != nil {
			t.Fatal(err)
		}
		// a name that sorts first takes no kept name's room
		en.repo.mu.Lock()
		en.repo.names = append([]string{"a000"}, names...)
		en.repo.mu.Unlock()
		en.runDue(t, u)
		if got := en.discovered(t); len(got) != 499 || got[0] != "n000" {
			t.Fatalf("after a000: %d names, first %s", len(got), got[0])
		}
		// the explicit entry removed frees a name's room: the next new name by name takes it
		must(t, func() error { return en.up.RemoveEntry(ctx, admin, "acme", "acme-repo", "aaa") })
		en.runDue(t, u)
		if got := en.discovered(t); len(got) != 500 || got[0] != "a000" {
			t.Fatalf("after aaa removed: %d names, first %s", len(got), got[0])
		}
	})
}

// Spec 0039 PR 2: "*" takes no name another source has: a distributor's, an alias, the upstream's
// own entry, a name another "*" mirror of the channel discovered first. A discovered name listed
// later is its entry's, counted once.
func TestDiscoveryRefused(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.kista()
		en.up.Config.Interval = time.Hour // a run made due runs
		tid := en.tenantID(t)
		// a distributor's publisher's release of "dist"
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(signed(t, nil, cpp("1.0", "v2.0.0"), 9, "dist_duckdb_cpp_init")),
			release.AddOptions{Name: "dist"}); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"UPDATE builds SET source = ? WHERE tenant_id = ? AND name = 'dist'",
			"UPDATE releases SET source = ? WHERE tenant_id = ? AND name = 'dist'",
			"UPDATE channel_sources SET source = ? WHERE channel_id IN (SELECT id FROM channels WHERE tenant_id = ?) AND name = 'dist'"} {
			must(t, func() error { return store.ExecRaw(ctx, en.st, q, store.PublisherSource("enterest-ci"), tid) })
		}
		en.repo.mu.Lock()
		en.repo.names = []string{"dist", "http", "own", "geo", "zzz"}
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"}, store.UpstreamEntry{Name: "own"})
		sp.FetchMode = store.FetchLazy
		u, err := en.up.Add(ctx, admin, "acme", sp)
		if err != nil {
			t.Fatal(err)
		}
		en.runDue(t, u)
		if got := en.discovered(t); !slices.Equal(got, []string{"geo", "zzz"}) { // not the distributor's, the alias, its own entry
			t.Fatalf("discovered: %v", got)
		}
		// "*" is one upstream's in a channel: another mirror's is refused, so none discovers a name first
		other := en.spec(store.UpstreamEntry{Name: "*"})
		other.Name = "second"
		if _, err := en.up.Add(ctx, admin, "acme", other); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("a second *: %v", err)
		}
		// "geo" listed explicitly: its discovered row goes, the name counts once
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "geo"}); err != nil {
			t.Fatal(err)
		}
		u, err = en.up.Get(ctx, admin, "acme", "acme-repo")
		if err != nil || !slices.Equal(u.Discovered, []string{"zzz"}) || len(u.Names()) != 3 {
			t.Fatalf("after geo listed: %v %v", u.Names(), err)
		}
		var cells int
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				cells, err = tx.TenantCells(ctx, tid, "")
				return err
			})
		})
		if cells != 3 {
			t.Fatalf("tenant cells: %d", cells)
		}
	})
}

// Spec 0039 PR 2: a cut listing (a page failing after the first, a next that is no cursor, an empty
// page paging on, a source over 10,000 names) adds what it read and removes nothing.
func TestDiscoveryCut(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.kista()
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesPage = []string{"geo", "tresor", "zzz"}, 2
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"})
		sp.FetchMode = store.FetchLazy
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		for _, cut := range []string{"error", "cursor", "empty"} {
			en.repo.mu.Lock()
			en.repo.names, en.repo.namesCut = []string{"geo", "new", "zzz"}, cut
			en.repo.mu.Unlock()
			r := en.sync(t, false)
			if got := en.discovered(t); !hasProblem(r, "names") || !slices.Equal(got, []string{"geo", "new", "tresor", "zzz"}) {
				t.Fatalf("%s: %+v %v", cut, r, got)
			}
		}
		// a page over 1 MiB is no page
		var long []string
		for i := range 500 {
			long = append(long, fmt.Sprintf("l%03d", i)+strings.Repeat("x", 2200))
		}
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesPage, en.repo.namesCut = long, 500, ""
		en.repo.mu.Unlock()
		if r := en.sync(t, false); !hasProblem(r, "names") || len(en.discovered(t)) != 4 {
			t.Fatalf("a page over 1 MiB: %+v %v", r, en.discovered(t))
		}
		var many []string
		for i := range 10001 {
			many = append(many, fmt.Sprintf("m%05d", i))
		}
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesPage, en.repo.namesCut = many, 500, ""
		en.repo.mu.Unlock()
		r := en.sync(t, false)
		if got := en.discovered(t); !hasProblem(r, "names") || !slices.Contains(got, "tresor") || len(got) != store.MaxDiscoveredNames {
			t.Fatalf("over 10,000: %+v, %d names", r, len(got))
		}
	})
}

// Spec 0039 PR 2: "*" put on a mirror reads its remote first: refused while it is no kista, taken
// (the upstream marked a kista's) once it is.
func TestDiscoveryPutProbes(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "geo"})); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "*"}); !errors.Is(err, store.ErrInvalid) ||
			!strings.Contains(err.Error(), "not a kista") {
			t.Fatalf("no kista: %v", err)
		}
		en.kista()
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "*"}); err != nil {
			t.Fatal(err)
		}
		u, err := en.up.Get(ctx, admin, "acme", "acme-repo")
		if err != nil || !u.RemoteKista || !u.AnyMirror() {
			t.Fatalf("after *: %+v %v", u, err)
		}
	})
}

// Spec 0039 PR 2: a lazy "*" mirror takes an ask for a discovered name; an intake (an ask's, a
// run's) refuses a name reserved, or held by the tenant's own source, since it was discovered.
func TestDiscoveredIntake(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.kista()
		en.up.Config.Interval = time.Hour // a run made due runs
		for _, n := range []string{"geo", "tresor", "zed"} {
			en.repo.put("v2.0.0", "linux_amd64", n, signed(t, en.key, cpp("1.0", "v2.0.0"), len(n), n+"_duckdb_cpp_init"))
		}
		en.repo.mu.Lock()
		en.repo.names = []string{"geo", "tresor", "zed"}
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"})
		sp.FetchMode = store.FetchLazy
		u, err := en.up.Add(ctx, admin, "acme", sp)
		if err != nil {
			t.Fatal(err)
		}
		if r := en.runDue(t, u); r.Counts["discovered"] != 3 || r.Counts[store.CellReleased] != 0 { // lazy: nothing asked
			t.Fatalf("discovery: %+v", r)
		}
		offer := func(name string) {
			p := en.puller()
			p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: name,
				Principal: "user:alice", Lazy: true})
			p.Drain(ctx)
		}
		offer("geo")
		if len(en.releases(t, "geo")) != 1 {
			t.Fatal("an ask for a discovered name was not fetched")
		}
		if _, err := en.ten.Reserve(ctx, admin, "", []string{"tresor"}, store.ReservedCommunity, true); err != nil {
			t.Fatal(err)
		}
		// the server's list outlives a test on a shared database, and the process's view follows it
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		t.Cleanup(func() { _ = en.ten.Unreserve(ctx, admin, "", "tresor") })
		offer("tresor")
		if len(en.releases(t, "tresor")) != 0 {
			t.Fatal("an ask took a name reserved since")
		}
		// the tenant's own zed since, in another channel: a run's intake refuses it too (the listing
		// failing, the run keeps the names it has)
		if _, _, err := en.rel.Add(ctx, admin, "acme", "other", bytes.NewReader(signed(t, nil, cpp("1.0", "v2.0.0"), 9, "zed_duckdb_cpp_init")),
			release.AddOptions{Name: "zed"}); err != nil {
			t.Fatal(err)
		}
		en.repo.mu.Lock()
		en.repo.namesStatus = http.StatusBadGateway
		en.repo.mu.Unlock()
		en.sync(t, false)
		for _, n := range []string{"tresor", "zed"} {
			if c := en.cell(t, n); c.Outcome != store.CellShadowed {
				t.Fatalf("%s at the run: %+v", n, c)
			}
		}
		if rs := en.releases(t, "zed"); len(rs) != 0 {
			t.Fatalf("zed: %+v", rs)
		}
	})
}
