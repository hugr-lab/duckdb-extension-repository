package store_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func TestStatistics(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		k := store.DownloadKey{TenantID: tn.ID, ChannelID: ch.ID, Name: "acl", ExtVersion: "1.0", Platform: "linux_amd64",
			DuckDBVersion: "v2.0.0", Day: "2026-10-09", Authenticated: true}
		anon := k
		anon.Authenticated = false
		old := k
		old.Day, old.ExtVersion = "2026-09-01", "0.9"
		for range 2 { // added, then added to
			if _, err := s.AddDownloadCounts(ctx, map[store.DownloadKey]int64{k: 3, anon: 1, old: 5}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.PutInstallers(ctx, "2026-10-09", map[store.InstallerKey]int64{
			{TenantID: tn.ID, ChannelID: ch.ID, Name: "acl", ExtVersion: "1.0", Platform: "linux_amd64"}: 2}); err != nil {
			t.Fatal(err)
		}
		if done, err := s.StatisticsDayDone(ctx, "2026-10-09"); err != nil || !done {
			t.Fatalf("day done: %v %v", done, err)
		}
		if done, _ := s.StatisticsDayDone(ctx, "2026-10-08"); done {
			t.Fatal("another day done")
		}
		rows, err := s.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-10-01", To: "2026-10-09"}, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		var count, authd, inst int64
		for _, r := range rows {
			count, authd, inst = count+r.Count, authd+r.Authenticated, inst+r.Installers
		}
		if count != 8 || authd != 6 || inst != 2 {
			t.Fatalf("rows: %+v", rows)
		}
		if _, err := s.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-01-01", To: "2026-12-31"}, 1, nil); err == nil {
			t.Fatal("more rows than the limit")
		}
		rs, err := s.ReleaseStats(ctx, tn.ID, "2026-10-09", store.StatsFilter{Name: "acl"})
		if err != nil || len(rs) != 2 || rs[1].Last7 != 8 || rs[1].Last30 != 8 || rs[1].LastDay != "2026-10-09" ||
			rs[0].ExtVersion != "0.9" || rs[0].Last30 != 0 || rs[0].LastDay != "2026-09-01" {
			t.Fatalf("release stats (0.9's last download is older than 30 days): %+v %v", rs, err)
		}
		// keep: a caller's scope; the limit counts kept rows only
		if rows, err := s.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-01-01", To: "2026-12-31"}, 1,
			func(_, name string) bool { return name == "none" }); err != nil || len(rows) != 0 {
			t.Fatalf("nothing kept: %+v %v", rows, err)
		}
		if n, err := s.PruneStatistics(ctx, "2026-10-01"); err != nil || n != 1 {
			t.Fatalf("prune: %d %v", n, err)
		}
		// install events of a day, paged in order
		for i := range 3 {
			now = time.Date(2026, 10, 9, 10, i, 0, 0, time.UTC)
			ev, _ := s.NewEvent(ctx, tn.ID, "principal:x", "install", audit.OK, "channel:prod/ext:acl/release:r",
				map[string]any{"name": "acl"})
			if err := s.InsertEvents(ctx, []store.Event{ev}); err != nil {
				t.Fatal(err)
			}
		}
		n := 0
		if err := s.InstallEvents(ctx, tn.ID, "2026-10-09", func(store.Event) error { n++; return nil }); err != nil || n != 3 {
			t.Fatalf("install events: %d %v", n, err)
		}
		n = 0
		if err := s.InstallEvents(ctx, tn.ID, "2026-10-08", func(store.Event) error { n++; return nil }); err != nil || n != 0 {
			t.Fatalf("another day's: %d %v", n, err)
		}
	})
}

// Replicas flushing the same keys at once neither deadlock nor lose counts (keys go in order).
func TestDownloadCountsConcurrent(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		counts := map[store.DownloadKey]int64{}
		for i := range 50 {
			counts[store.DownloadKey{TenantID: tn.ID, ChannelID: ch.ID, Name: "acl", ExtVersion: "1." + strconv.Itoa(i),
				Platform: "linux_amd64", DuckDBVersion: "v2.0.0", Day: "2026-10-09"}] = 1
		}
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 5 {
					if _, err := s.AddDownloadCounts(ctx, counts); err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		rows, err := s.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-10-09", To: "2026-10-09"}, 100, nil)
		var total int64
		for _, r := range rows {
			total += r.Count
		}
		if err != nil || total != 50*20 {
			t.Fatalf("counts: %d %v", total, err)
		}
	})
}

// The gauges' queries (spec 0010 phase 2b): pending events per sink in one pass, cells per outcome.
func TestMetricsQueries(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		if m, err := s.PendingCounts(ctx, map[string]int{"a": 1 << 3}); err != nil || m["a"] != 0 {
			t.Fatalf("empty pending: %v %v", m, err)
		}
		if m, err := s.CellOutcomes(ctx); err != nil || len(m) != 0 {
			t.Fatalf("empty cells: %v %v", m, err)
		}
		s.EventSinks = func(string) int { return 1<<0 | 1<<5 }
		for i := range 3 {
			ev, err := s.NewEvent(ctx, tn.ID, "os:1:t", "grant.add", audit.OK, "grant:"+string(rune('a'+i)), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.InsertEvents(ctx, []store.Event{ev}); err != nil {
				t.Fatal(err)
			}
		}
		m, err := s.PendingCounts(ctx, map[string]int{"a": 1 << 0, "b": 1 << 5, "c": 1 << 1, "d": 1 << 15})
		if err != nil || m["a"] != 3 || m["b"] != 3 || m["c"] != 0 || m["d"] != 0 {
			t.Fatalf("pending: %v %v", m, err)
		}
		u := store.Upstream{TenantID: tn.ID, Name: "core", Kind: store.UpstreamCore, ChannelID: ch.ID, Mode: store.ModeMirror,
			Visibility: "public", State: store.UpstreamActive}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.InsertUpstream(ctx, &u); err != nil {
				return err
			}
			for i, o := range []string{store.CellReleased, store.CellReleased, store.CellFailed} {
				if err := tx.PutCell(ctx, store.UpstreamCell{UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64",
					Name: string(rune('a' + i)), Outcome: o, FetchedAt: time.Now()}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		m, err = s.CellOutcomes(ctx)
		if err != nil || m[store.CellReleased] != 2 || m[store.CellFailed] != 1 || len(m) != 2 {
			t.Fatalf("cells: %v %v", m, err)
		}
	})
}
