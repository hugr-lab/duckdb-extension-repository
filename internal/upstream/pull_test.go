package upstream_test

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

func (en *env) puller() *upstream.Puller {
	return &upstream.Puller{Service: en.up, Holder: "test", Log: slog.New(slog.DiscardHandler)}
}

func (en *env) pullSpec(entries ...store.UpstreamEntry) upstream.Spec {
	sp := en.spec(entries...)
	sp.Name, sp.Mode = "pull", store.ModePullThrough
	return sp
}

func (en *env) snapshot(t *testing.T, channel string) *release.Snapshot {
	t.Helper()
	c, err := en.st.GetChannel(ctx, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&release.Snapshots{Store: en.st}).Get(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestPullThrough(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "anyext", signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "anyext_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "*"}))
		if err != nil {
			t.Fatal(err)
		}
		// a mirror upstream of the channel lists "listed": "*" does not take it
		mirror := en.spec(store.UpstreamEntry{Name: "listed"})
		mirror.Name = "mirror"
		if _, err := en.up.Add(ctx, admin, "acme", mirror); err != nil {
			t.Fatal(err)
		}
		// a pull-through upstream is not run
		if _, err := en.up.Sync(ctx, admin, "acme", "pull", false); err == nil {
			t.Fatal("a sync of a pull-through upstream")
		}
		// the channel's snapshot says which cells a miss may offer, without the database
		snap := en.snapshot(t, "prod")
		for _, c := range []struct {
			version, platform, name string
			want                    bool
		}{
			{"v2.0.0", "linux_amd64", "tresor", true},
			{"v2.0.0", "linux_amd64", "anyext", true},  // "*"
			{"v2.0.0", "linux_amd64", "json", false},   // a core name: never "*"
			{"v2.0.0", "linux_amd64", "listed", false}, // another upstream lists it
			{"v2.0.0", "osx_arm64", "tresor", false},   // a platform it does not fetch
			{"v9.9.9", "linux_amd64", "tresor", false}, // a version the channel does not serve
		} {
			if got := snap.PullThroughFor(c.version, c.platform, c.name) == u.ID; got != c.want {
				t.Errorf("%+v: %v", c, got)
			}
		}
		p := en.puller()
		miss := func(name string) release.Miss {
			return release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: name}
		}
		p.Offer(miss("tresor"))
		p.Offer(miss("tresor")) // queued once
		p.Offer(miss("anyext"))
		p.Offer(miss("gone"))
		p.Drain(ctx)
		for _, n := range []string{"tresor", "anyext"} {
			if rs := en.releases(t, n); len(rs) != 1 || rs[0].Origin != store.OriginUpstream {
				t.Fatalf("%s pulled: %+v", n, rs)
			}
		}
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "tresor")); g != 1 {
			t.Fatalf("tresor fetched %d times", g)
		}
		// cells: the listed name's and released "*" names'; a "*" miss leaves none
		cells, _ := en.up.Cells(ctx, admin, "acme", "pull", "", [3]string{}, 10)
		if len(cells) != 2 {
			t.Fatalf("cells: %+v", cells)
		}
		// a miss is remembered: it is not fetched again within negative_ttl
		p.Offer(miss("gone"))
		p.Drain(ctx)
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "gone")); g != 1 {
			t.Fatalf("a remembered miss was fetched %d times", g)
		}
		// a yanked pulled release still misses: it is not fetched again
		rel := en.releases(t, "anyext")[0].Release
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "anyext", rel.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		en.repo.touch("v2.0.0", "linux_amd64", "anyext")
		p.Offer(miss("anyext"))
		p.Drain(ctx)
		p.Offer(miss("anyext"))
		p.Drain(ctx)
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "anyext")); g != 2 {
			t.Fatalf("a yanked cell was fetched %d times", g)
		}
		// "*" takes no name a mirror of the channel lists since (the worker checks again)
		en.repo.put("v2.0.0", "linux_amd64", "late", signed(t, en.key, cpp("1.0", "v2.0.0"), 4, "late_duckdb_cpp_init"))
		p.Offer(miss("late"))
		if _, err := en.up.PutEntry(ctx, admin, "acme", "mirror", store.UpstreamEntry{Name: "late"}); err != nil {
			t.Fatal(err)
		}
		p.Drain(ctx)
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "late")); g != 0 {
			t.Fatal("\"*\" took a name a mirror lists")
		}
		// the cell's lease row is gone after the fetch
		var leases int
		if err := store.QueryRowRaw(ctx, en.st, "SELECT COUNT(*) FROM leases WHERE name LIKE 'kista/upstream/%/%'", &leases); err != nil || leases != 0 {
			t.Fatalf("cell leases left: %d %v", leases, err)
		}
		// a listed name whose body a pull refuses is an upstream.rejected event, as in a run (spec 0010)
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		en.repo.put("v2.0.0", "linux_amd64", "forged", signed(t, other, cpp("1.0", "v2.0.0"), 5, "forged_duckdb_cpp_init"))
		if _, err := en.up.PutEntry(ctx, admin, "acme", "pull", store.UpstreamEntry{Name: "forged"}); err != nil {
			t.Fatal(err)
		}
		p.Offer(miss("forged"))
		p.Drain(ctx)
		evs, _ := en.st.ListEvents(ctx, u.TenantID, store.EventFilter{Kind: "upstream.rejected"})
		if len(evs) != 1 || !strings.Contains(evs[0].Data, `"name":"forged"`) {
			t.Fatalf("upstream.rejected of a pull: %+v", evs)
		}
		// a paused upstream does not fetch
		cur, _ := en.up.Get(ctx, admin, "acme", "pull")
		if _, err := en.up.Set(ctx, admin, "acme", "pull", "", store.UpstreamPaused, cur.Version); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "later", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "later_duckdb_cpp_init"))
		p.Offer(miss("later"))
		p.Drain(ctx)
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "later")); g != 0 {
			t.Fatal("a paused upstream fetched")
		}
		if snap := en.snapshot(t, "prod"); snap.PullThroughFor("v2.0.0", "linux_amd64", "later") != "" {
			t.Fatal("a paused upstream is offered misses")
		}
	})
}

// Two replicas missing the same cell fetch it once.
func TestPullThroughOnce(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p0 := cellPath("v2.0.0", "linux_amd64", "tresor")
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "tresor"}))
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		en.repo.mu.Lock()
		en.repo.gate[p0] = gate
		en.repo.mu.Unlock()
		m := release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "tresor"}
		a, b := en.puller(), en.puller()
		b.Holder = "other"
		a.Offer(m)
		b.Offer(m)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); a.Drain(ctx) }()
		<-en.repo.hit
		b.Drain(ctx) // the cell's lease is held
		close(gate)
		wg.Wait()
		if g, _ := en.repo.count(p0); g != 1 {
			t.Fatalf("fetched %d times", g)
		}
		if rs := en.releases(t, "tresor"); len(rs) != 1 {
			t.Fatalf("releases: %+v", rs)
		}
	})
}
