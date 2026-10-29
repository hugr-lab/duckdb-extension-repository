package store_test

import (
	"cmp"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// A distributor's downloads per (week, extension, tenant): its publishers' in every tenant, live and
// removed, never another source's (a tenant's own build of the name), another distributor's or a
// deleting tenant's; a suspended tenant's counted; weeks across a year's end; the type filter; and
// the same answer however the sources are chunked.
func TestDistributorParts(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		defer pkgtype.RegisterForTest(testType)()
		type tenant struct {
			id, prod, drv, pub, own string
		}
		var ts []tenant
		inTx(t, s, func(tx *store.Tx) error {
			for i := range 5 {
				tn := store.Tenant{Name: fmt.Sprintf("t%d", i)}
				if err := tx.CreateTenant(ctx, &tn); err != nil {
					return err
				}
				x := tenant{id: tn.ID}
				for _, c := range []struct {
					name, typ string
					id        *string
				}{{"prod", "duckdb_extension", &x.prod}, {"drv", testType, &x.drv}} {
					ch := store.Channel{TenantID: tn.ID, Name: c.name, Kind: store.ChannelSigned, PackageType: c.typ}
					if err := tx.CreateChannel(ctx, &ch); err != nil {
						return err
					}
					*c.id = ch.ID
				}
				for _, p := range []struct {
					name, distributor string
					id                *string
				}{{"enterest-ci", "enterest", &x.pub}, {"own", "", &x.own}} {
					pp := store.Publisher{TenantID: tn.ID, Name: p.name, CreatedBy: "os:1:t"}
					if err := tx.InsertPublisher(ctx, &pp); err != nil {
						return err
					}
					if p.distributor != "" {
						if _, err := tx.SetPublisherDistributor(ctx, tn.ID, p.name, p.distributor); err != nil {
							return err
						}
					}
					*p.id = store.PublisherSource(pp.ID)
				}
				ts = append(ts, x)
			}
			// t1's distributor's publisher is removed (remembered), t2's own is another distributor's
			if err := tx.DeletePublisher(ctx, ts[1].id, "enterest-ci", "os:1:t"); err != nil {
				return err
			}
			_, err := tx.SetPublisherDistributor(ctx, ts[2].id, "own", "other")
			return err
		})
		// t3 suspended, t4 being deleted
		if err := store.ExecRaw(ctx, s, "UPDATE tenants SET state = 'suspended' WHERE id = ?", ts[3].id); err != nil {
			t.Fatal(err)
		}
		counts := map[store.DownloadKey]int64{}
		add := func(x tenant, ch, src, name, day string, n int64) {
			counts[store.DownloadKey{TenantID: x.id, ChannelID: ch, Source: src, Name: name, PackageVersion: "1.0", Platform: "linux_amd64",
				DuckDBVersion: "v2.0.0", Day: day, Authenticated: n%2 == 0}] += n
		}
		for i, x := range ts {
			add(x, x.prod, x.pub, "geo", "2026-12-31", int64(i+1)) // week 2026-12-28
			add(x, x.prod, x.pub, "geo", "2027-01-03", 10)         // the same week, its Sunday
			add(x, x.prod, x.pub, "geo", "2027-01-04", 100)        // week 2027-01-04, across the year's end
			add(x, x.prod, x.pub, "geo", "2026-12-27", 1000)       // before the weeks asked
			add(x, x.prod, x.own, "geo", "2026-12-31", 5000)       // a tenant's own build of the name
			add(x, x.prod, store.SourceTenant, "geo", "2026-12-31", 7000)
			add(x, x.drv, x.pub, "drv", "2026-12-31", 3)  // another type
			add(x, x.prod, "", "geo", "2026-12-31", 9000) // counted before the source was
		}
		if _, err := s.AddDownloadCounts(ctx, counts); err != nil {
			t.Fatal(err)
		}
		installers := map[store.InstallerKey]int64{}
		for _, x := range ts {
			installers[store.InstallerKey{TenantID: x.id, ChannelID: x.prod, Source: x.pub, Name: "geo", PackageVersion: "1.0",
				Platform: "linux_amd64", Day: "2027-01-01"}] = 2
		}
		if err := s.PutDaily(ctx, "2027-01-01", store.Daily{Installers: installers}); err != nil {
			t.Fatal(err)
		}
		// deleting after the counts: its statistics no longer count
		if err := store.ExecRaw(ctx, s, "UPDATE tenants SET deleting_at = ? WHERE id = ?", store.TimeArg(s, time.Now()), ts[4].id); err != nil {
			t.Fatal(err)
		}
		weeks := []string{"2026-12-28", "2027-01-04"}
		want := []store.DistributorPart{}
		for i, x := range ts[:4] {
			want = append(want, store.DistributorPart{Week: weeks[0], Name: "geo", TenantID: x.id, Downloads: int64(i+1) + 10, Installers: 2},
				store.DistributorPart{Week: weeks[1], Name: "geo", TenantID: x.id, Downloads: 100})
		}
		sorted := func(ps []store.DistributorPart) []store.DistributorPart {
			slices.SortFunc(ps, func(a, b store.DistributorPart) int {
				return cmp.Or(cmp.Compare(a.Week, b.Week), cmp.Compare(a.Name, b.Name), cmp.Compare(a.TenantID, b.TenantID))
			})
			return ps
		}
		want = sorted(want)
		for _, chunk := range []int{500, 1, 3} {
			restore := store.SetDistributorChunk(chunk)
			got, err := s.DistributorParts(ctx, "enterest", "duckdb_extension", weeks)
			restore()
			if err != nil || !slices.Equal(sorted(got), want) {
				t.Fatalf("chunks of %d: %+v %v\nwant %+v", chunk, got, err, want)
			}
		}
		got, err := s.DistributorParts(ctx, "enterest", testType, weeks[:1])
		if err != nil || len(got) != 4 || got[0].Name != "drv" || got[0].Downloads != 3 {
			t.Fatalf("the type filter: %+v %v", got, err)
		}
		if got, err := s.DistributorParts(ctx, "other", "duckdb_extension", weeks); err != nil || len(got) != 1 || got[0].Downloads != 5000 ||
			got[0].TenantID != ts[2].id {
			t.Fatalf("another distributor's: %+v %v", got, err)
		}
		if got, err := s.DistributorParts(ctx, "nobody", "duckdb_extension", weeks); err != nil || len(got) != 0 {
			t.Fatalf("no such distributor: %+v %v", got, err)
		}
		ids, err := s.TenantIDsByName(ctx, []string{"t0", "t3", "t4", "none"})
		if err != nil || len(ids) != 2 || ids["t0"] != ts[0].id || ids["t3"] != ts[3].id {
			t.Fatalf("tenants by name (a deleting one and an unknown one left out): %v %v", ids, err)
		}
	})
}
