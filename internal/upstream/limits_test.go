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

// runEvents counts acme's events of kind, and the upstream.run events whose error class is class.
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
		if e.Kind == "upstream.run" && strings.Contains(string(e.Data), `"error":"`+class+`"`) {
			runs++
		}
	}
	return kinds, runs
}

// Spec 0038: a run at the tenant's bytes limit records a cell that would bring a new body failed
// without downloading it (no upstream.rejected event), still fetches a cell with a body
// conditionally (a body the tenant has passes), and ends failed: not interrupted, its next run at
// the usual time. With room again, the next run brings the bodies.
func TestRunAtTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Interval = time.Hour // scheduled: an interrupted run would be due again in minutes
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "acl"},
			store.UpstreamEntry{Name: "geo"}))
		if err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		used, err := en.st.TenantBytes(ctx, u.TenantID)
		if err != nil || used == 0 {
			t.Fatalf("used: %d %v", used, err)
		}
		en.setBytes(t, used)
		// tresor's yank and a changed ETag: its 200 brings the body the tenant has, which passes
		rel := en.releases(t, "tresor")[0].Release
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		en.repo.touch("v2.0.0", "linux_amd64", "tresor")
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "geo", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "geo_duckdb_cpp_init"))
		gets := map[string]int{}
		for _, n := range []string{"acl", "geo"} {
			gets[n], _ = en.repo.count(cellPath("v2.0.0", "linux_amd64", n)) // the first run's 404s
		}
		start := time.Now()
		r := en.sync(t, false)
		if r.Counts[store.CellYanked] != 1 || r.Counts[store.CellFailed] != 2 || !strings.Contains(r.Error, "the run failed") ||
			!strings.Contains(r.Error, "bytes limit") {
			t.Fatalf("the run at the limit: %+v", r)
		}
		for _, n := range []string{"acl", "geo"} {
			if g, _ := en.repo.count(cellPath("v2.0.0", "linux_amd64", n)); g != gets[n] {
				t.Fatalf("%s downloaded %d times", n, g-gets[n])
			}
			if c := en.cell(t, n); c.Outcome != store.CellFailed || !strings.Contains(c.Detail, "bytes limit") {
				t.Fatalf("%s: %+v", n, c)
			}
		}
		if rejected, failed := en.runEvents(t, u.TenantID, "upstream.rejected", "failed"); rejected != 0 || failed != 1 {
			t.Fatalf("events: %d rejected, %d failed runs", rejected, failed)
		}
		cur, err := en.up.Get(ctx, admin, "acme", "acme-repo")
		if err != nil || cur.NextRunAt.Before(start.Add(50*time.Minute)) {
			t.Fatalf("the next run (not an interrupted one's): %v %v", cur.NextRunAt, err)
		}
		// room again: the next run brings both
		en.setBytes(t, store.LimitUnlimited)
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 || r.Error != "" {
			t.Fatalf("with room: %+v", r)
		}
	})
}

// Spec 0038: a cell refused by the tenant's limit after its download (room for one body of two)
// stops the run's new downloads; the other is released.
func TestRunMeetsTenantLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		acl := signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "acl_duckdb_cpp_init")
		en.repo.put("v2.0.0", "linux_amd64", "acl", acl)
		en.repo.put("v2.0.0", "linux_amd64", "geo", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "geo_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}, store.UpstreamEntry{Name: "geo"}))
		if err != nil {
			t.Fatal(err)
		}
		en.setBytes(t, int64(len(acl))*3/2)
		r := en.sync(t, false)
		if r.Counts[store.CellReleased] != 1 || r.Counts[store.CellFailed] != 1 || !strings.Contains(r.Error, "bytes limit") {
			t.Fatalf("the run: %+v", r)
		}
		if rejected, failed := en.runEvents(t, u.TenantID, "upstream.rejected", "failed"); rejected != 0 || failed != 1 {
			t.Fatalf("events: %d rejected, %d failed runs", rejected, failed)
		}
	})
}

// Spec 0038: a source's full catalogue (spec 0033) in a run is the same: the cell (fetched
// conditionally: its build landed) failed, not rejected, and the next run downloads no new body while
// the catalogue is full.
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
		en.rel.MaxEntries = 1 // one build placed nowhere: the source's catalogue is full
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("3.0", "v2.0.0"), 3, "acl_duckdb_cpp_init"))
		r := en.sync(t, false)
		if r.Counts[store.CellFailed] != 1 || !strings.Contains(r.Error, "the run failed") {
			t.Fatalf("the run: %+v", r)
		}
		if c := en.cell(t, "acl"); c.Outcome != store.CellFailed || !strings.Contains(c.Detail, "catalogue is full") {
			t.Fatalf("the cell: %+v", c)
		}
		path := cellPath("v2.0.0", "linux_amd64", "acl")
		before, _ := en.repo.count(path)
		if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 || !strings.Contains(r.Error, "catalogue is full") {
			t.Fatalf("the next run: %+v", r)
		}
		if g, _ := en.repo.count(path); g != before {
			t.Fatalf("downloaded %d times with the catalogue full", g-before)
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

// Spec 0038: a pull-through fetch past the tenant's bytes limit is refused before its download and
// remembered for NegativeTTL (here a nanosecond: offered again once there is room, it is fetched).
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
		path := cellPath("v2.0.0", "linux_amd64", "tresor")
		if g, _ := en.repo.count(path); g != 0 || len(en.releases(t, "tresor")) != 0 {
			t.Fatalf("fetched past the limit: %d", g)
		}
		cs, err := en.up.Cells(ctx, admin, "acme", "pull", "", [3]string{}, 10)
		if err != nil || len(cs) != 1 || cs[0].Outcome != store.CellFailed || !strings.Contains(cs[0].Detail, "bytes limit") {
			t.Fatalf("the cell: %+v %v", cs, err)
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
