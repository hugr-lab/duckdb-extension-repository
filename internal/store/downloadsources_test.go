package store_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0045 (spec 0036 phase 2) on a populated level-44 database: counts and installers are
// kept with the source ”, two sources of one (channel, name, version, platform) are stored apart,
// and an older replica no longer runs (min_reader 45).
func TestMigrationDownloadSources(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 44); err != nil {
			t.Fatal(err)
		}
		const tid, cid = "t0000000-0000-0000-0000-000000000000", "c0000000-0000-0000-0000-000000000000"
		for _, q := range [][]any{
			{"INSERT INTO download_counts (tenant_id, channel_id, name, package_version, platform, duckdb_version, day, authenticated, count) " +
				"VALUES (?, ?, 'acl', '1.0', 'linux_amd64', 'v2.0.0', '2026-10-20', ?, 7)", tid, cid, true},
			{"INSERT INTO download_installers (tenant_id, channel_id, name, package_version, platform, day, installers) " +
				"VALUES (?, ?, 'acl', '1.0', 'linux_amd64', '2026-10-20', 2)", tid, cid},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 45 AND min_reader = 45"); err != nil || n != 1 {
			t.Fatalf("migration 0045 needs a reader at 45: %d %v", n, err)
		}
		for _, table := range []string{"download_counts", "download_installers"} {
			if n, err := store.CountRaw(ctx, s, table, "source = '' AND tenant_id = ?", tid); err != nil || n != 1 {
				t.Fatalf("%s kept with the source '': %d %v", table, n, err)
			}
		}
		k := store.DownloadKey{TenantID: tid, ChannelID: cid, Name: "acl", PackageVersion: "1.0", Platform: "linux_amd64",
			DuckDBVersion: "v2.0.0", Day: "2026-10-20", Authenticated: true}
		other := k
		other.Source = "publisher:p"
		if _, err := s.AddDownloadCounts(ctx, map[store.DownloadKey]int64{k: 1, other: 4}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutDaily(ctx, "2026-10-21", store.Daily{Installers: map[store.InstallerKey]int64{
			{TenantID: tid, ChannelID: cid, Name: "acl", PackageVersion: "1.0", Platform: "linux_amd64", Day: "2026-10-21"}:                        1,
			{TenantID: tid, ChannelID: cid, Source: "publisher:p", Name: "acl", PackageVersion: "1.0", Platform: "linux_amd64", Day: "2026-10-21"}: 3,
		}}); err != nil {
			t.Fatal(err)
		}
		rows, err := s.DownloadRows(ctx, tid, store.StatsFilter{From: "2026-10-01", To: "2026-10-31"}, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, r := range rows {
			got[r.Source] += r.Count + r.Installers
		}
		if len(rows) != 5 || got[""] != 8+2+1 || got["publisher:p"] != 4+3 {
			t.Fatalf("two sources of one release stored apart: %+v", rows)
		}
	})
}
