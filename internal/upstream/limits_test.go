package upstream_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// setBytes sets acme's bytes limit (store.LimitUnlimited for none).
func (en *env) setBytes(t *testing.T, v int64) {
	t.Helper()
	if _, _, err := en.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: v}, ""); err != nil {
		t.Fatal(err)
	}
}

// runEvents counts the tenant's events of kind, and the upstream.run events whose error class is
// class.
func (en *env) runEvents(t *testing.T, tenantID, kind, class string) (kinds, runs int) {
	t.Helper()
	evs, err := en.st.ListEvents(ctx, tenantID, store.EventFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == kind {
			kinds++
		}
		if e.Kind == "upstream.run" && strings.Contains(e.Data, `"error":"`+class+`"`) {
			runs++
		}
	}
	return kinds, runs
}

func (en *env) gets(name string) int {
	g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", name))
	return g
}

// Spec 0038: a run that starts at the tenant's bytes limit skips, without a request and without
// touching their records, the cells that hold no body (counted skipped_at_limit); a cell that holds
// a body runs as ever (a 304; a 200 with the body the tenant has, its yank kept). Nothing was
// refused, so the run does not fail. A dry run says the same. With room, the next run brings the
// bodies.
func TestRunAtTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "pg", signed(t, en.key, cpp("1.0", "v2.0.0"), 4, "pg_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "bad", signed(t, nil, cpp("1.0", "v2.0.0"), 6, "bad_duckdb_cpp_init")) // unsigned: rejected
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "pg"},
			store.UpstreamEntry{Name: "acl"}, store.UpstreamEntry{Name: "geo"}, store.UpstreamEntry{Name: "bad"}))
		if err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 || r.Counts[store.CellMissing] != 2 || r.Counts[store.CellRejected] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		if c := en.cell(t, "bad"); c.ETag == "" {
			t.Fatalf("a refusal's record keeps the ETag of a body it did not take: %+v", c)
		}
		used, err := en.st.TenantBytes(ctx, u.TenantID)
		if err != nil || used == 0 {
			t.Fatalf("used: %d %v", used, err)
		}
		en.setBytes(t, used)
		rel := en.releases(t, "tresor")[0].Release
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		en.repo.touch("v2.0.0", "linux_amd64", "tresor") // a 200 with the body the tenant has
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "geo", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "geo_duckdb_cpp_init"))
		before := map[string]int{"acl": en.gets("acl"), "geo": en.gets("geo"), "bad": en.gets("bad")}
		want := map[string]string{"acl": store.CellMissing, "geo": store.CellMissing, "bad": store.CellRejected}

		if r := en.sync(t, true); r.Counts["skipped_at_limit"] != 3 || r.Counts["would_release"] != 0 || r.Error != "" {
			t.Fatalf("a dry run at the limit: %+v", r)
		}
		r := en.sync(t, false)
		if r.Counts["skipped_at_limit"] != 3 || r.Counts[store.CellYanked] != 1 || r.Counts[store.CellUnchanged] != 1 ||
			r.Counts[store.CellFailed] != 0 || r.Error != "" {
			t.Fatalf("the run at the limit: %+v", r)
		}
		for n, outcome := range want {
			if g := en.gets(n); g != before[n] {
				t.Fatalf("%s requested %d times", n, g-before[n])
			}
			if c := en.cell(t, n); c.Outcome != outcome {
				t.Fatalf("%s's record touched: %+v", n, c)
			}
		}
		if rs := en.releases(t, "tresor"); len(rs) != 1 || rs[0].State != store.ReleaseYanked {
			t.Fatalf("the yank: %+v", rs)
		}
		if rejected, failed := en.runEvents(t, u.TenantID, "upstream.rejected", "failed"); rejected != 1 || failed != 0 { // the first run's
			t.Fatalf("events: %d rejected, %d failed runs", rejected, failed)
		}
		en.setBytes(t, store.LimitUnlimited)
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 || r.Error != "" {
			t.Fatalf("with room: %+v", r)
		}
	})
}

// Spec 0038: a cell refused by the tenant's limit during a run (room for one new body) stops the
// run's new bodies: a later cell holding none is skipped without a request, a cell holding one still
// runs (its conditional 200 with a new body refused, its record keeping the body and ETag it held),
// and the run ends failed, not interrupted, its next run at the usual time.
func TestRunMeetsTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Concurrency = 1 // the cells in the entries' order
		en.up.Config.Interval = time.Hour
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}, store.UpstreamEntry{Name: "geo"},
			store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "pg"}))
		if err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		tresor := en.cell(t, "tresor")
		used, err := en.st.TenantBytes(ctx, u.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		acl := signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "acl_duckdb_cpp_init")
		en.repo.put("v2.0.0", "linux_amd64", "acl", acl)
		en.repo.put("v2.0.0", "linux_amd64", "geo", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "geo_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.1", "v2.0.0"), 5, "tresor_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "pg", signed(t, en.key, cpp("1.0", "v2.0.0"), 4, "pg_duckdb_cpp_init"))
		en.setBytes(t, used+int64(len(acl))*3/2)
		pgBefore := en.gets("pg")
		start := time.Now()
		r := en.sync(t, false)
		if r.Counts[store.CellReleased] != 1 || r.Counts[store.CellFailed] != 2 || r.Counts["skipped_at_limit"] != 1 ||
			!strings.Contains(r.Error, "the run failed") || !strings.Contains(r.Error, "bytes limit") {
			t.Fatalf("the run: %+v", r)
		}
		if g := en.gets("pg"); g != pgBefore {
			t.Fatalf("pg requested %d times after the refusal", g-pgBefore)
		}
		if c := en.cell(t, "geo"); c.Outcome != store.CellFailed || !strings.Contains(c.Detail, "bytes limit") {
			t.Fatalf("geo: %+v", c)
		}
		if c := en.cell(t, "tresor"); c.Outcome != store.CellFailed || c.BodyHash != tresor.BodyHash || c.ETag != tresor.ETag {
			t.Fatalf("tresor's record: %+v, held %+v", c, tresor)
		}
		if rejected, failed := en.runEvents(t, u.TenantID, "upstream.rejected", "failed"); rejected != 0 || failed != 1 {
			t.Fatalf("events: %d rejected, %d failed runs", rejected, failed)
		}
		cur, err := en.up.Get(ctx, admin, "acme", "acme-repo")
		if err != nil || cur.NextRunAt.Before(start.Add(50*time.Minute)) {
			t.Fatalf("the next run (not an interrupted one's): %v %v", cur.NextRunAt, err)
		}
	})
}

// Spec 0038: a cell holding a body whose upstream sends no ETag runs at the limit as ever (fetched
// unconditionally; the body the tenant has passes), and a failed fetch keeps the body it holds: its
// landed build stays its cell's, never collected.
func TestRunAtLimitKeepsBodies(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		path := cellPath("v2.0.0", "linux_amd64", "acl")
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}))
		if err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		v, err := en.rel.Source(ctx, admin, "acme", "prod", "acl")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "acl", v.Source, store.RuleManual); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("2.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		en.repo.noETag[path] = true
		if r := en.sync(t, false); r.Counts[store.CellLanded] != 1 {
			t.Fatalf("2.0 landed: %+v", r)
		}
		landed := en.cell(t, "acl")
		if landed.BodyHash == "" || landed.ETag != "" {
			t.Fatalf("a landed cell without an ETag: %+v", landed)
		}
		used, err := en.st.TenantBytes(ctx, u.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		en.setBytes(t, used)
		before := en.gets("acl")
		if r := en.sync(t, false); r.Counts[store.CellLanded]+r.Counts[store.CellUnchanged] != 1 || r.Error != "" || en.gets("acl") != before+1 {
			t.Fatalf("at the limit: %+v", r)
		}
		en.repo.busy[path], en.repo.retryAfter = 1, "" // a failure, retried by the next run
		if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 {
			t.Fatalf("a failed fetch: %+v", r)
		}
		if c := en.cell(t, "acl"); c.BodyHash != landed.BodyHash {
			t.Fatalf("the failed record lost its body: %+v", c)
		}
		if stale, err := en.st.StaleMirrorLanded(ctx); err != nil || len(stale) != 0 {
			t.Fatalf("the landed build is stale: %+v %v", stale, err)
		}
	})
}

// Spec 0038: a source's full catalogue (spec 0033) in a run: the cell (its build landed, fetched
// conditionally) is failed, not rejected, keeping the body it held, and the run ends failed.
func TestRunAtFullCatalogue(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}))
		if err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		v, err := en.rel.Source(ctx, admin, "acme", "prod", "acl")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "acl", v.Source, store.RuleManual); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("2.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellLanded] != 1 {
			t.Fatalf("2.0 landed: %+v", r)
		}
		landed := en.cell(t, "acl")
		en.rel.MaxEntries = 1 // one build placed nowhere: the source's catalogue is full
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("3.0", "v2.0.0"), 3, "acl_duckdb_cpp_init"))
		r := en.sync(t, false)
		if r.Counts[store.CellFailed] != 1 || !strings.Contains(r.Error, "the run failed") {
			t.Fatalf("the run: %+v", r)
		}
		if c := en.cell(t, "acl"); c.Outcome != store.CellFailed || !strings.Contains(c.Detail, "catalogue is full") || c.BodyHash != landed.BodyHash {
			t.Fatalf("the cell: %+v", c)
		}
		if rejected, _ := en.runEvents(t, u.TenantID, "upstream.rejected", "failed"); rejected != 0 {
			t.Fatalf("%d upstream.rejected events", rejected)
		}
		en.rel.MaxEntries = 0
		if r := en.sync(t, false); r.Counts[store.CellLanded] != 1 || r.Error != "" {
			t.Fatalf("with room: %+v", r)
		}
	})
}

// Spec 0038 with spec 0032: a lazy mirror's first request at the tenant's bytes limit is not
// fetched, and its cell is recorded failed, so the request is answered 404 (not coming) rather than
// 503 forever, past the ask's window too.
func TestLazyAskAtTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		en.setBytes(t, 0)
		miss := release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "b",
			Principal: "user:alice", Lazy: true}
		p := en.puller()
		if coming, err := p.Ask(ctx, true, miss); err != nil || !coming {
			t.Fatalf("a first ask: %v %v", coming, err)
		}
		before := en.gets("b")
		p.Offer(miss)
		p.Drain(ctx)
		if g := en.gets("b"); g != before {
			t.Fatalf("fetched past the limit: %d", g-before)
		}
		must(t, func() error {
			return store.ExecRaw(ctx, en.st, "UPDATE upstream_asks SET asked_at = ? WHERE upstream_id = ?", store.TimeArg(en.st, time.Now().Add(-time.Hour)), u.ID)
		})
		if coming, err := p.Ask(ctx, true, miss); err != nil || coming {
			t.Fatalf("an ask past the window at the limit: %v %v", coming, err)
		}
		cs, err := en.up.Cells(ctx, admin, "acme", u.Name, "", [3]string{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		recorded := false
		for _, c := range cs {
			if c.Name == "b" && c.DuckDBVersion == "v2.0.0" {
				recorded = c.Outcome == store.CellFailed && strings.Contains(c.Detail, "bytes limit")
			}
		}
		if !recorded {
			t.Fatalf("b's cell: %+v", cs)
		}
	})
}

// Spec 0038: a pull-through fetch of a cell holding no body, at the tenant's bytes limit, is not
// requested nor recorded, and is remembered for NegativeTTL (here a nanosecond: offered again once
// there is room, it is fetched).
func TestPullAtTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "tresor"}))
		if err != nil {
			t.Fatal(err)
		}
		en.setBytes(t, 0)
		p := en.puller()
		p.NegativeTTL = time.Nanosecond
		miss := release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "tresor"}
		p.Offer(miss)
		p.Drain(ctx)
		if g := en.gets("tresor"); g != 0 || len(en.releases(t, "tresor")) != 0 {
			t.Fatalf("fetched past the limit: %d", g)
		}
		if cs, err := en.up.Cells(ctx, admin, "acme", "pull", "", [3]string{}, 10); err != nil || len(cs) != 0 {
			t.Fatalf("recorded: %+v %v", cs, err)
		}
		en.setBytes(t, store.LimitUnlimited)
		time.Sleep(time.Millisecond)
		p.Offer(miss)
		p.Drain(ctx)
		if rs := en.releases(t, "tresor"); len(rs) != 1 {
			t.Fatalf("pulled with room: %+v", rs)
		}
	})
}
