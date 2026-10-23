package upstream_test

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// lazySetup serves a, b and c on two DuckDB versions of prod, and adds a lazy upstream listing them,
// a eager. Runs are scheduled (an interval), so a mirror never run is due.
func lazySetup(t *testing.T, en *env) store.Upstream {
	t.Helper()
	en.up.Config.Interval = time.Hour
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
		return err
	})
	for _, n := range []string{"a", "b", "c"} {
		for _, v := range []string{"v2.0.0", "v2.1.0"} {
			en.repo.put(v, "linux_amd64", n, signed(t, en.key, cpp("1.0", v), 1, n+"_duckdb_cpp_init"))
		}
	}
	sp := en.spec(store.UpstreamEntry{Name: "a", Eager: true}, store.UpstreamEntry{Name: "b"}, store.UpstreamEntry{Name: "c"})
	sp.FetchMode = store.FetchLazy
	u, err := en.up.Add(ctx, admin, "acme", sp)
	if err != nil {
		t.Fatal(err)
	}
	if u.FetchMode != store.FetchLazy || !u.Entries[0].Eager || u.Entries[1].Eager {
		t.Fatalf("added: %+v", u)
	}
	return u
}

// runDue makes the upstream due and runs it on this replica.
func (en *env) runDue(t *testing.T, u store.Upstream) upstream.Result {
	t.Helper()
	must(t, func() error {
		return en.st.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error { return tx.MakeDue(ctx, u.ID, "") })
	})
	en.run.Once(ctx)
	cur, err := en.up.Get(ctx, admin, "acme", u.Name)
	if err != nil {
		t.Fatal(err)
	}
	var r upstream.Result
	mustJSON(t, cur.LastRun, &r)
	return r
}

func (en *env) ask(t *testing.T, u store.Upstream, v, p, n string) {
	t.Helper()
	must(t, func() error {
		return en.st.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error {
			_, err := tx.Ask(ctx, u.ID, v, p, n, store.AskRefresh)
			return err
		})
	})
}

func (en *env) asks(t *testing.T, u store.Upstream) map[[3]string]time.Time {
	t.Helper()
	var out map[[3]string]time.Time
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			out, err = tx.Asks(ctx, u.ID)
			return err
		})
	})
	return out
}

func (en *env) notAsked(t *testing.T) []string {
	t.Helper()
	cs, err := en.up.Cells(ctx, admin, "acme", "acme-repo", upstream.NotAsked, [3]string{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.DuckDBVersion+"/"+c.Name)
	}
	return out
}

// Spec 0032: a lazy upstream's run fetches its eager entries' cells and the cells asked for, keeps
// them current, and counts the rest not_asked.
func TestLazyRun(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		r := en.runDue(t, u)
		if r.Counts[store.CellReleased] != 2 || r.Counts["not_asked"] != 4 {
			t.Fatalf("the first run: %+v", r.Counts)
		}
		for _, n := range []string{"b", "c"} {
			if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", n)); g != 0 {
				t.Fatalf("%s was fetched unasked", n)
			}
		}
		if got := en.notAsked(t); !slices.Equal(got, []string{"v2.0.0/b", "v2.0.0/c", "v2.1.0/b", "v2.1.0/c"}) {
			t.Fatalf("not asked: %v", got)
		}
		// an ask is a cell: b on v2.0.0 only
		en.ask(t, u, "v2.0.0", "linux_amd64", "b")
		r = en.runDue(t, u)
		if r.Counts[store.CellReleased] != 1 || r.Counts[store.CellUnchanged] != 2 || r.Counts["not_asked"] != 3 {
			t.Fatalf("the run after an ask: %+v", r.Counts)
		}
		if g, _ := en.repo.count(cellPath("v2.1.0", "linux_amd64", "b")); g != 0 {
			t.Fatal("another DuckDB version of an asked cell was fetched")
		}
		// a new version of an asked cell arrives with the next run
		en.repo.put("v2.0.0", "linux_amd64", "b", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "b_duckdb_cpp_init"))
		r = en.runDue(t, u)
		if r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the refresh: %+v", r.Counts)
		}
		if rs := en.releases(t, "b"); len(rs) != 2 {
			t.Fatalf("b's releases: %d", len(rs))
		}
		if got := en.notAsked(t); !slices.Equal(got, []string{"v2.0.0/c", "v2.1.0/b", "v2.1.0/c"}) {
			t.Fatalf("not asked: %v", got)
		}
		// asks go with their entry, platform or DuckDB version
		must(t, func() error { return en.up.AddPlatform(ctx, admin, "acme", "acme-repo", "osx_arm64") })
		en.ask(t, u, "v2.0.0", "osx_arm64", "b")
		must(t, func() error { return en.up.RemovePlatform(ctx, admin, "acme", "acme-repo", "osx_arm64") })
		if _, ok := en.asks(t, u)[[3]string{"v2.0.0", "osx_arm64", "b"}]; ok {
			t.Fatal("an ask outlived its platform")
		}
		en.ask(t, u, "v2.1.0", "linux_amd64", "c")
		must(t, func() error { return en.up.RemoveEntry(ctx, admin, "acme", "acme-repo", "c") })
		if _, ok := en.asks(t, u)[[3]string{"v2.1.0", "linux_amd64", "c"}]; ok {
			t.Fatal("an ask outlived its entry")
		}
		en.ask(t, u, "v2.1.0", "linux_amd64", "b")
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", nil, []string{"v2.1.0"})
			return err
		})
		if got := en.asks(t, u); len(got) != 1 {
			t.Fatalf("asks after the version went: %v", got)
		}
	})
}

// Spec 0032: sync fetches a lazy upstream's whole matrix and records no ask; a dry run covers what
// a run would.
func TestLazySync(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		en.runDue(t, u)
		r := en.sync(t, true)
		if r.Counts["would_release"] != 0 || r.Counts["not_asked"] != 4 {
			t.Fatalf("a dry sync: %+v", r.Counts)
		}
		r = en.sync(t, false)
		if r.Counts[store.CellReleased] != 4 || r.Counts["not_asked"] != 0 {
			t.Fatalf("a sync: %+v", r.Counts)
		}
		if got := en.asks(t, u); len(got) != 0 {
			t.Fatalf("a sync recorded asks: %v", got)
		}
		if got := en.notAsked(t); len(got) != 0 {
			t.Fatalf("not asked after a sync: %v", got)
		}
		// later runs stay lazy: the synced cells are not refreshed, nor counted not_asked (fetched)
		en.repo.put("v2.0.0", "linux_amd64", "c", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "c_duckdb_cpp_init"))
		r = en.runDue(t, u)
		if r.Counts[store.CellReleased] != 0 || r.Counts["not_asked"] != 0 || len(en.releases(t, "c")) != 2 {
			t.Fatalf("the run after a sync: %+v", r.Counts)
		}
	})
}

// Spec 0032: switching a mirror's fetch mode. Lazy keeps its releases current (an ask for each
// cell a release fills); eager makes it due at once. Only a mirror takes one, never in a passthrough
// channel, and only a mirror's entry is eager.
func TestFetchModes(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Interval = time.Hour
		en.repo.put("v2.0.0", "linux_amd64", "a", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "a_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "a"}, store.UpstreamEntry{Name: "b"}))
		if err != nil {
			t.Fatal(err)
		}
		if u.FetchMode != store.FetchEager {
			t.Fatalf("the default: %q", u.FetchMode)
		}
		en.run.Once(ctx)
		// a's release stays current though its last fetch failed (its record names no body then)
		en.repo.mu.Lock()
		en.repo.busy[cellPath("v2.0.0", "linux_amd64", "a")], en.repo.retryAfter = 1, ""
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 {
			t.Fatalf("a failed fetch: %+v", r.Counts)
		}
		u, _ = en.up.Get(ctx, admin, "acme", "acme-repo")
		if u, err = en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchLazy, u.Version); err != nil {
			t.Fatal(err)
		}
		// b was missing: no release, no ask
		if got := en.asks(t, u); len(got) != 1 || got[[3]string{"v2.0.0", "linux_amd64", "a"}].IsZero() {
			t.Fatalf("asks after turning lazy: %v", got)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchEager, u.Version-1); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		en.run.Once(ctx) // not due
		u, _ = en.up.Get(ctx, admin, "acme", "acme-repo")
		if u, err = en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchEager, 0); err != nil {
			t.Fatal(err)
		}
		if u.NextRunAt.After(time.Now()) {
			t.Fatalf("turning eager did not make it due: %v", u.NextRunAt)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", "sometimes", 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an unknown mode: %v", err)
		}
		// refusals
		pt := en.spec(store.UpstreamEntry{Name: "x"})
		pt.Name, pt.Mode, pt.FetchMode = "pt", store.ModePullThrough, store.FetchLazy
		if _, err := en.up.Add(ctx, admin, "acme", pt); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a lazy pull-through: %v", err)
		}
		pt.FetchMode, pt.Entries = "", []store.UpstreamEntry{{Name: "x", Eager: true}}
		if _, err := en.up.Add(ctx, admin, "acme", pt); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an eager pull-through entry: %v", err)
		}
		pt.Entries = []store.UpstreamEntry{{Name: "x"}}
		if _, err := en.up.Add(ctx, admin, "acme", pt); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "pt", store.FetchLazy, 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a pull-through turning lazy: %v", err)
		}
		core := upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "mirror", Visibility: store.Public, Platforms: []string{"linux_amd64"},
			Entries: []store.UpstreamEntry{{Name: "json"}}, FetchMode: store.FetchLazy}
		if _, err := en.up.Add(ctx, admin, "acme", core); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a lazy mirror in a passthrough channel: %v", err)
		}
		core.FetchMode = ""
		if _, err := en.up.Add(ctx, admin, "acme", core); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "core", store.FetchLazy, 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough mirror turning lazy: %v", err)
		}
	})
}

// Spec 0032: a lazy upstream's run skips, and counts, a cell another fetch holds the lease of.
func TestLazyRunCellLease(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		held, err := en.st.AcquireLease(ctx, upstream.CellLease(u.ID, "v2.0.0", "linux_amd64", "a"), "another", time.Minute)
		if err != nil || !held {
			t.Fatal(held, err)
		}
		r := en.runDue(t, u)
		if r.Counts["busy"] != 1 || r.Counts[store.CellReleased] != 1 {
			t.Fatalf("a run with a held cell: %+v", r.Counts)
		}
		if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", "a")); g != 0 {
			t.Fatal("a held cell was fetched")
		}
		if _, _, found, _ := en.st.LeaseState(ctx, upstream.CellLease(u.ID, "v2.1.0", "linux_amd64", "a")); found {
			t.Fatal("a run's cell lease was left behind")
		}
	})
}

// Spec 0032: kista's fetcher asks again a cell answered 503 with a Retry-After of at most 30
// seconds, three times at most.
func TestFetchRetry(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := cellPath("v2.0.0", "linux_amd64", "a")
		en.repo.put("v2.0.0", "linux_amd64", "a", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "a_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "a"})); err != nil {
			t.Fatal(err)
		}
		en.repo.mu.Lock()
		en.repo.busy[p] = 3
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("three 503s: %+v", r)
		}
		if g, _ := en.repo.count(p); g != 4 {
			t.Fatalf("asked %d times", g)
		}
		en.repo.put("v2.0.0", "linux_amd64", "a", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "a_duckdb_cpp_init"))
		en.repo.mu.Lock()
		en.repo.busy[p] = 4
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 {
			t.Fatalf("four 503s: %+v", r)
		}
		if g, _ := en.repo.count(p); g != 8 {
			t.Fatalf("asked %d times in all", g)
		}
		// 429 alike
		en.repo.put("v2.0.0", "linux_amd64", "a", signed(t, en.key, cpp("1.2", "v2.0.0"), 3, "a_duckdb_cpp_init"))
		en.repo.mu.Lock()
		en.repo.busy[p], en.repo.busyStatus = 1, http.StatusTooManyRequests
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("a 429: %+v", r)
		}
		// no Retry-After, or a long one: not asked again
		for _, ra := range []string{"", "31"} {
			en.repo.mu.Lock()
			en.repo.busy[p], en.repo.retryAfter = 1, ra
			en.repo.mu.Unlock()
			before, _ := en.repo.count(p)
			if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 {
				t.Fatalf("Retry-After %q: %+v", ra, r)
			}
			if g, _ := en.repo.count(p); g != before+1 {
				t.Fatalf("Retry-After %q: asked %d times", ra, g-before)
			}
		}
	})
}

// Spec 0032: bundle export -sync runs a lazy upstream over the cells its plan exports only.
func TestExportSyncLazy(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.repo.token = "T-sub"
			if err := os.WriteFile(g.tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.put(t, "tresor", "1.0", 1)
			g.put(t, "acl", "1.0", 2)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "acl"})
			sp.Credential, sp.FetchMode = "sub", store.FetchLazy
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.air(t, "tresor")
			plan := g.plan(t)
			res, data := g.export(t, plan, upstream.ExportOptions{Private: []string{plan.Sources[0].Prefix}, Sync: true})
			if c := outcomes(res); c[bundle.Fetched] != 1 {
				t.Fatalf("-sync of a lazy upstream: %+v", res)
			}
			if _, cells, _ := g.open(t, data); cells["tresor"].Outcome != bundle.Fetched {
				t.Fatalf("cells: %+v", cells)
			}
			if n, _ := g.repo.count(cellPath("v2.0.0", "linux_amd64", "acl")); n != 0 {
				t.Fatal("a cell outside the plan was fetched")
			}
			u, _ := g.up.Get(ctx, admin, "acme", "acme-repo")
			if got := g.asks(t, u); len(got) != 0 {
				t.Fatalf("an export recorded asks: %v", got)
			}
		})
	}
}

// Spec 0032: a cell waiting out a Retry-After frees its host's slot: two cells of one host, one
// slot, each answered 503 once with Retry-After: 2, wait at once (about 2s, not 4s).
func TestFetchRetryFreesHost(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.PerHost = 1
		for _, n := range []string{"a", "b"} {
			en.repo.put("v2.0.0", "linux_amd64", n, signed(t, en.key, cpp("1.0", "v2.0.0"), 1, n+"_duckdb_cpp_init"))
		}
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "a"}, store.UpstreamEntry{Name: "b"})); err != nil {
			t.Fatal(err)
		}
		en.repo.mu.Lock()
		en.repo.busy[cellPath("v2.0.0", "linux_amd64", "a")], en.repo.busy[cellPath("v2.0.0", "linux_amd64", "b")] = 1, 1
		en.repo.retryAfter = "2"
		en.repo.mu.Unlock()
		start := time.Now()
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 {
			t.Fatalf("the run: %+v", r)
		}
		if d := time.Since(start); d > 3500*time.Millisecond {
			t.Fatalf("the waits held the host's slot: %v", d)
		}
	})
}

// Spec 0032: a first request asks for a lazy mirror's cell: its build is coming (503) while the cell
// was never fetched or its lease is held, and every replica answers alike; the puller fetches it
// (trigger ask), and the cell is then answered as any (404 when the upstream has none).
func TestLazyAsk(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		snap := en.snapshot(t, "prod")
		pt := snap.PullThroughFor("v2.0.0", "linux_amd64", "b")
		if pt == nil || !pt.Lazy || pt.UpstreamID != u.ID {
			t.Fatalf("the snapshot's lazy mirror: %+v", pt)
		}
		miss := func(name string) release.Miss {
			return release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: name,
				Principal: "user:alice", Lazy: true}
		}
		a, b := en.puller(), en.puller()
		b.Holder = "other"
		for _, p := range []*upstream.Puller{a, b} {
			if coming, err := p.Ask(ctx, true, miss("b")); err != nil || !coming {
				t.Fatalf("a first ask: %v %v", coming, err)
			}
		}
		if got := en.asks(t, u); len(got) != 1 {
			t.Fatalf("asks: %v", got)
		}
		// a fetch holding the cell's lease: coming, on every replica
		lease := upstream.CellLease(u.ID, "v2.0.0", "linux_amd64", "b")
		must(t, func() error {
			_, err := en.st.AcquireLease(ctx, lease, "fetching", time.Minute)
			return err
		})
		must(t, func() error {
			return store.ExecRaw(ctx, en.st, "INSERT INTO upstream_cells (upstream_id, duckdb_version, platform, name, outcome, fetched_at) VALUES (?, 'v2.0.0', 'linux_amd64', 'b', 'failed', ?)",
				u.ID, store.TimeArg(en.st, time.Now()))
		})
		if coming, err := b.Ask(ctx, true, miss("b")); err != nil || !coming {
			t.Fatalf("an ask while the lease is held: %v %v", coming, err)
		}
		must(t, func() error { return en.st.DeleteLease(ctx, lease, "fetching") })
		if coming, err := b.Ask(ctx, true, miss("b")); err != nil || coming {
			t.Fatalf("an ask of a failed cell: %v %v", coming, err)
		}
		must(t, func() error { return store.ExecRaw(ctx, en.st, "DELETE FROM upstream_cells WHERE name = 'b'") })
		// the puller fetches an asked cell of a lazy mirror
		a.Offer(miss("b"))
		a.Drain(ctx)
		if rs := en.releases(t, "b"); len(rs) != 1 {
			t.Fatalf("b's releases: %+v", rs)
		}
		evs, _ := en.st.ListEvents(ctx, u.TenantID, store.EventFilter{Kind: "upstream.release"})
		found := false
		for _, e := range evs {
			found = found || strings.Contains(e.Data, `"name":"b"`) && strings.Contains(e.Data, `"trigger":"ask"`)
		}
		if !found {
			t.Fatalf("upstream.release of an ask: %+v", evs)
		}
		if coming, err := a.Ask(ctx, true, miss("b")); err != nil || coming {
			t.Fatalf("an ask of a fetched cell: %v %v", coming, err)
		}
		// a cell the upstream has none of: coming until fetched, then answered as any (404)
		if coming, _ := a.Ask(ctx, true, miss("c")); !coming {
			t.Fatal("an ask of a cell never fetched")
		}
		must(t, func() error {
			en.repo.mu.Lock()
			defer en.repo.mu.Unlock()
			delete(en.repo.files, cellPath("v2.0.0", "linux_amd64", "c"))
			return nil
		})
		a.Offer(miss("c"))
		a.Drain(ctx)
		if coming, _ := a.Ask(ctx, true, miss("c")); coming {
			t.Fatal("an ask of a missing cell")
		}
		asks, _ := a.Asks(ctx)
		if asks[upstream.AskRecorded] != 2 || asks[upstream.AskFetched] != 1 {
			t.Fatalf("ask metrics: %v", asks)
		}
		// a cell nothing fetched is coming for two minutes after its ask; a plain request after them
		// opens them again (it is offered again), a versioned one does not
		b21 := release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.1.0", Platform: "linux_amd64", Name: "b",
			Principal: "user:alice", Lazy: true}
		if coming, _ := a.Ask(ctx, false, b21); coming {
			t.Fatal("a versioned ask is never coming")
		}
		if coming, _ := a.Ask(ctx, true, b21); !coming {
			t.Fatal("a fresh ask")
		}
		later := time.Now().Add(3 * time.Minute)
		en.st.Now = func() time.Time { return later }
		if coming, _ := a.Ask(ctx, false, b21); coming {
			t.Fatal("a versioned ask past the window")
		}
		if coming, _ := a.Ask(ctx, true, b21); !coming {
			t.Fatal("a plain ask past the window did not open it again")
		}
		if at := en.asks(t, u)[[3]string{"v2.1.0", "linux_amd64", "b"}]; !at.Equal(later.UTC().Truncate(time.Microsecond)) && !at.Equal(later) {
			t.Fatalf("the reopened ask's time: %v", at)
		}
		en.st.Now = func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
		// a fetch that cannot start (the repository's keys unreadable) records the cell failed: 404
		en.repo.mu.Lock()
		keys := en.repo.keys
		en.repo.keys = nil
		en.repo.mu.Unlock()
		c := en.puller()
		c.Offer(b21)
		c.Drain(ctx)
		if coming, _ := c.Ask(ctx, true, b21); coming {
			t.Fatal("an ask of a cell whose fetch could not start")
		}
		en.repo.mu.Lock()
		en.repo.keys = keys
		en.repo.mu.Unlock()
		// the run fetches the asked cells besides the eager entries
		r := en.runDue(t, u)
		if r.Counts[store.CellReleased] != 3 || r.Counts[store.CellUnchanged] != 1 || r.Counts[store.CellMissing] != 1 {
			t.Fatalf("the run after asks: %+v", r.Counts)
		}
	})
}

// Spec 0032: one caller has at most 8 misses waiting, so one downstream kista's run does not starve
// the tenant's other callers; a dropped lazy offer keeps its ask for the next run.
func TestCallerShare(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		p := en.puller()
		for i := range 10 {
			p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64",
				Name: fmt.Sprintf("n%d", i), Principal: "client:downstream", Lazy: true})
		}
		if q := p.Queued(); q != 8 {
			t.Fatalf("queued: %d", q)
		}
		p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "b",
			Principal: "user:alice", Lazy: true})
		if q := p.Queued(); q != 9 {
			t.Fatalf("another caller's miss: %d queued", q)
		}
		if asks, _ := p.Asks(ctx); asks[upstream.AskDropped] != 2 {
			t.Fatalf("dropped: %v", asks)
		}
		// pull-through misses take the share too (not counted as asks)
		for i := range 9 {
			p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64",
				Name: fmt.Sprintf("p%d", i), Principal: "client:other"})
		}
		if q := p.Queued(); q != 17 {
			t.Fatalf("pull-through misses: %d queued", q)
		}
		if asks, _ := p.Asks(ctx); asks[upstream.AskDropped] != 2 {
			t.Fatalf("dropped after pull-through misses: %v", asks)
		}
		// the queue drains: the shares free (nothing here is fetched but b)
		p.Drain(ctx)
		if q := p.Queued(); q != 0 {
			t.Fatalf("queued after draining: %d", q)
		}
	})
}
