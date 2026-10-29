package stats_test

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: the daily pass computes each complete week's distributor snapshots once (on its
// first pass every complete week the retention still holds); a later late flush, re-mark of a
// publisher's distributor, tenant deletion, another minimum or a retention prune never changes an
// answered week; and snapshots older than the weeks kept go.
func TestDistributorSnapshots(t *testing.T) {
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) // a Friday: the last complete week is 10-12's
	st.Now = func() time.Time { return now }
	type tenant struct{ id, ch, src string }
	ts := map[string]tenant{}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		for _, name := range []string{"t1", "t2", "t3", "free"} {
			tn := store.Tenant{Name: name}
			if err := tx.CreateTenant(ctx, &tn); err != nil {
				return err
			}
			ch := store.Channel{TenantID: tn.ID, Name: "prod", Kind: store.ChannelSigned}
			if err := tx.CreateChannel(ctx, &ch); err != nil {
				return err
			}
			p := store.Publisher{TenantID: tn.ID, Name: "ent-ci", CreatedBy: "os:1:t"}
			if err := tx.InsertPublisher(ctx, &p); err != nil {
				return err
			}
			if _, err := tx.SetPublisherDistributor(ctx, tn.ID, "ent-ci", "enterest"); err != nil {
				return err
			}
			ts[name] = tenant{tn.ID, ch.ID, store.PublisherSource(p.ID)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	add := func(name, day string, n int64) {
		t.Helper()
		x := ts[name]
		if _, err := st.AddDownloadCounts(ctx, map[store.DownloadKey]int64{{TenantID: x.id, ChannelID: x.ch, Source: x.src, Name: "geo",
			PackageVersion: "1.0", Platform: "linux_amd64", DuckDBVersion: "v2.0.0", Day: day}: n}); err != nil {
			t.Fatal(err)
		}
	}
	// week 10-05: three non-public tenants and the public one; week 10-12: one non-public tenant
	add("t1", "2026-10-06", 10)
	add("t2", "2026-10-07", 20)
	add("t3", "2026-10-11", 30)
	add("free", "2026-10-05", 5)
	add("t1", "2026-10-13", 7)
	add("free", "2026-10-18", 4)
	inst := map[store.InstallerKey]int64{}
	for _, name := range []string{"t1", "t2", "t3"} {
		x := ts[name]
		inst[store.InstallerKey{TenantID: x.id, ChannelID: x.ch, Source: x.src, Name: "geo", PackageVersion: "1.0", Platform: "linux_amd64"}] = 1
	}
	if err := st.PutDaily(ctx, "2026-10-06", store.Daily{Installers: inst}); err != nil {
		t.Fatal(err)
	}
	daily := func(minTenants int) {
		(&stats.Daily{Store: st, Holder: "test", Retention: 30 * 24 * time.Hour, EventRetention: 30 * 24 * time.Hour,
			Log: slog.New(slog.DiscardHandler), PublicTenants: []string{"free"}, MinTenants: minTenants}).Once(ctx)
	}
	read := func(dist, from, to string) []store.DistributorWeek {
		t.Helper()
		rows, err := st.DistributorWeeks(ctx, dist, "duckdb_extension", from, to)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	daily(3)
	// the first pass: every complete week the retention (from 09-23) holds whole, 09-28 to 10-12
	for day, want := range map[string]bool{"2026-09-21": false, "2026-09-28": true, "2026-10-05": true, "2026-10-12": true, "2026-10-19": false} {
		if done, err := st.DistributorWeekDone(ctx, day); err != nil || done != want {
			t.Fatalf("week %s done: %v %v", day, done, err)
		}
	}
	three := int64(3)
	want := []store.DistributorWeek{
		{Distributor: "enterest", PackageType: "duckdb_extension", Week: "2026-10-05", Extension: "geo", Downloads: 65, Public: 5, InstallerDays: &three},
		{Distributor: "enterest", PackageType: "duckdb_extension", Week: "2026-10-12", Extension: "geo", Downloads: 4, Public: 4, Withheld: true},
	}
	if got := read("enterest", "2026-09-28", "2026-10-12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the snapshots:\n got %+v\nwant %+v", got, want)
	}

	// a late flush, a re-mark, a tenant's deletion and another minimum: the answered weeks stay
	add("t1", "2026-10-08", 100)
	add("t2", "2026-10-20", 9) // week 10-19: the re-marked publisher's from then on
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		_, err := tx.SetPublisherDistributor(ctx, ts["t2"].id, "ent-ci", "other")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(ctx, st, "UPDATE tenants SET deleting_at = ? WHERE id = ?", store.TimeArg(st, now), ts["t3"].id); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) // week 10-19 complete
	daily(2)
	if got := read("enterest", "2026-09-28", "2026-10-12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("an answered week changed: %+v", got)
	}
	if rows := read("other", "2026-10-19", "2026-10-19"); len(rows) != 1 || rows[0].Downloads != 0 || !rows[0].Withheld {
		t.Fatalf("the new week, the re-marked publisher's: %+v", rows)
	}
	// the counts of week 10-05 pruned by the retention: its snapshot stays
	now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	daily(3)
	if n, _ := store.CountRaw(ctx, st, "download_counts", "day < ?", "2026-10-13"); n != 0 {
		t.Fatalf("the old counts are pruned: %d", n)
	}
	if got := read("enterest", "2026-09-28", "2026-10-12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("a pruned week's snapshot changed: %+v", got)
	}
	// beyond the weeks kept, snapshots go: 10-12 is the oldest week kept
	now = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*(stats.KeepDistributorWeeks-1)+9)
	daily(3)
	if got := read("enterest", "2026-09-28", "2026-10-12"); len(got) != 1 || got[0].Week != "2026-10-12" {
		t.Fatalf("snapshots past the weeks kept: %+v", got)
	}
	if done, _ := st.DistributorWeekDone(ctx, "2026-10-05"); done {
		t.Fatal("a pruned week is still marked done")
	}
}
