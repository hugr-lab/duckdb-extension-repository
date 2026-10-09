package stats_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var ctx = context.Background()

func TestInstalls(t *testing.T) {
	s := &stats.Installs{Max: 2}
	if !s.First("p", "r", "c", "d") || s.First("p", "r", "c", "d") {
		t.Fatal("the first, then not")
	}
	if !s.First("p", "r", "c", "d2") || !s.First("q", "r", "c", "d") {
		t.Fatal("another day, another principal")
	}
	if !s.First("p", "r", "c", "d") {
		t.Fatal("the least recently seen is forgotten beyond Max")
	}
	// one principal's events a day are bounded: a flood of clients or releases makes no more
	a := &stats.Installs{MaxPerActor: 3}
	n := 0
	for i := range 10 {
		if a.First("p", "r", string(rune('a'+i)), "d") {
			n++
		}
	}
	if n != 3 || !a.First("q", "r", "c", "d") || !a.First("p", "r", "z", "d2") {
		t.Fatalf("per principal and day: %d", n)
	}
	// a late request of the day before (its counts are gone) never resets the new day's
	if !a.First("p", "r", "y", "d") || !a.First("p", "r", "x", "d2") || !a.First("p", "r", "w", "d2") || a.First("p", "r", "v", "d2") {
		t.Fatal("around midnight")
	}
	b := &stats.Installs{MaxActors: 2}
	if !b.First("p", "r", "c", "d") || !b.First("q", "r", "c", "d") || b.First("x", "r", "c", "d") || !b.First("p", "r2", "c", "d") {
		t.Fatal("principals a day")
	}
}

func TestCounterAndDaily(t *testing.T) {
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn := store.Tenant{Name: "acme", DisplayName: "Acme"}
	ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		return tx.CreateChannel(ctx, &ch)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	c := &stats.Counter{Store: st, Log: slog.New(slog.DiscardHandler)}
	k := store.DownloadKey{TenantID: tn.ID, ChannelID: ch.ID, Name: "acl", ExtVersion: "1.0", Platform: "linux_amd64",
		DuckDBVersion: "v2.0.0", Day: "2026-10-08"}
	for range 3 {
		c.Add(k)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	c.Flush(gone) // the store refuses: the counts are kept
	c.Add(k)
	c.Flush(ctx)
	// installs of 2026-10-08: alice twice, bob once (two releases' worth of keys: one)
	now = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	for _, who := range []string{"principal:acme/i|alice", "principal:acme/i|alice", "principal:acme/i|bob"} {
		e, err := st.NewEvent(ctx, tn.ID, who, "install", audit.OK, "channel:prod/ext:acl/release:r1",
			map[string]any{"release": "r1", "name": "acl", "version": "1.0", "platform": "linux_amd64"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.InsertEvents(ctx, []store.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	// within the hour after the day ends: not yet (the replicas' writers may hold its events)
	now = time.Date(2026, 10, 9, 0, 30, 0, 0, time.UTC)
	d := &stats.Daily{Store: st, Holder: "test", Retention: 365 * 24 * time.Hour, EventRetention: 30 * 24 * time.Hour,
		Log: slog.New(slog.DiscardHandler)}
	d.Once(ctx)
	if done, _ := st.StatisticsDayDone(ctx, "2026-10-08"); done {
		t.Fatal("a day counted within the grace")
	}
	now = time.Date(2026, 10, 9, 1, 30, 0, 0, time.UTC)
	d.Once(ctx)
	rows, err := st.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-10-01", To: "2026-10-09"}, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	var count, installers int64
	for _, r := range rows {
		count, installers = count+r.Count, installers+r.Installers
	}
	if count != 4 || installers != 2 {
		t.Fatalf("counts and installers: %+v", rows)
	}
	if done, _ := st.StatisticsDayDone(ctx, "2026-10-02"); !done {
		t.Fatal("a week of days counted (none had installs)")
	}
	// again: nothing new, the day is done and keeps its installers
	d.Once(ctx)
	rows, _ = st.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-10-01", To: "2026-10-09"}, 100, nil)
	if len(rows) != 2 || rows[1].Installers != 2 {
		t.Fatalf("a second pass: %+v", rows)
	}
	// days whose first events the buffer no longer holds are not counted (nor marked)
	d2 := &stats.Daily{Store: st, Holder: "test", EventRetention: 48 * time.Hour, Days: 10, Log: slog.New(slog.DiscardHandler)}
	d2.Once(ctx)
	if done, _ := st.StatisticsDayDone(ctx, "2026-09-29"); done {
		t.Fatal("a day beyond the events' retention was marked")
	}
	// statistics past their retention go
	now = time.Date(2027, 10, 9, 1, 30, 0, 0, time.UTC)
	d.Once(ctx)
	if rows, _ := st.DownloadRows(ctx, tn.ID, store.StatsFilter{From: "2026-01-01", To: "2027-12-31"}, 100, nil); len(rows) != 0 {
		t.Fatalf("after the retention: %+v", rows)
	}
}
