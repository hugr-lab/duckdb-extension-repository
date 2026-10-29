package store_test

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
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
		var second string // t0's second distributor's publisher
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
					pp := store.Publisher{TenantID: tn.ID, Name: p.name, CreatedBy: "os:1:t", Distributor: p.distributor}
					if err := tx.InsertPublisher(ctx, &pp); err != nil {
						return err
					}
					*p.id = store.PublisherSource(pp.ID)
				}
				ts = append(ts, x)
			}
			// t0 has a second publisher of the distributor's; t1's is removed (remembered), t2's own is
			// another distributor's
			two := store.Publisher{TenantID: ts[0].id, Name: "enterest-two", CreatedBy: "os:1:t", Distributor: "enterest"}
			if err := tx.InsertPublisher(ctx, &two); err != nil {
				return err
			}
			second = store.PublisherSource(two.ID)
			return tx.DeletePublisher(ctx, ts[1].id, "enterest-ci", "os:1:t")
		})
		// t2's own becomes another distributor's (as an older binary could leave it: spec 0037 marks
		// a publisher at its creation only)
		if err := store.ExecRaw(ctx, s, "UPDATE publishers SET distributor = 'other' WHERE tenant_id = ? AND name = 'own'", ts[2].id); err != nil {
			t.Fatal(err)
		}
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
		add(ts[0], ts[0].prod, second, "geo", "2027-01-02", 20)
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
			want = append(want, store.DistributorPart{Week: weeks[0], Name: "geo", TenantID: x.id, Downloads: int64(i+1) + 10 + map[bool]int64{true: 20}[i == 0], Installers: 2},
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
		if ds, err := s.Distributors(ctx); err != nil || !slices.Equal(ds, []string{"enterest", "other"}) {
			t.Fatalf("the distributors (live and removed publishers'): %v %v", ds, err)
		}
	})
}

// The chunks keep every query under SQL Server's 2,100 parameters: a distributor's query names a
// chunk of sources, a week's bound per week (at most 53, a snapshot one) and three more; a release
// lookup a chunk of ids and the tenant's.
func TestChunkParameters(t *testing.T) {
	if n := store.DistributorChunk + 53 + 3; n >= 2100 {
		t.Fatalf("a distributor's query takes %d parameters", n)
	}
	if n := store.ReleaseSourcesChunk + 1; n >= 2100 {
		t.Fatalf("a release lookup takes %d parameters", n)
	}
}

// A week's snapshots: stored once with the week's mark (a second write leaves them), read back with
// their installer-days left out or not, and pruned with the mark.
func TestDistributorWeekSnapshots(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		if _, _, ok, err := s.DistributorWeeksSpan(ctx); err != nil || ok {
			t.Fatalf("no week stored: %v %v", ok, err)
		}
		five := int64(5)
		rows := []store.DistributorWeek{
			{Distributor: "enterest", PackageType: "duckdb_extension", Week: "2026-10-12", Extension: "geo", Downloads: 9, Public: 2, InstallerDays: &five},
			{Distributor: "enterest", PackageType: "duckdb_extension", Week: "2026-10-12", Extension: "h3", Downloads: 1, Public: 1, Withheld: true},
			{Distributor: "other", PackageType: "duckdb_extension", Week: "2026-10-12", Extension: "geo", Downloads: 3, Public: 3},
		}
		if ok, err := s.PutDistributorWeek(ctx, "2026-10-12", 3, rows); err != nil || !ok {
			t.Fatalf("stored: %v %v", ok, err)
		}
		if ok, err := s.PutDistributorWeek(ctx, "2026-10-12", 3, rows[:1]); err != nil || ok {
			t.Fatalf("a week done is never written again: %v %v", ok, err)
		}
		if _, err := s.PutDistributorWeek(ctx, "2026-10-19", 3, rows[:1]); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a row of another week: %v", err)
		}
		if done, err := s.DistributorWeekDone(ctx, "2026-10-12"); err != nil || !done {
			t.Fatalf("done: %v %v", done, err)
		}
		if lo, hi, ok, err := s.DistributorWeeksSpan(ctx); err != nil || !ok || lo != "2026-10-12" || hi != "2026-10-12" {
			t.Fatalf("the span: %s %s %v %v", lo, hi, ok, err)
		}
		want := slices.Clone(rows[:2])
		for i := range want {
			want[i].MinTenants = 3 // read back with the week's minimum
		}
		got, err := s.DistributorWeeks(ctx, "enterest", "duckdb_extension", "2026-10-05", "2026-10-12")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("read back: %+v %v", got, err)
		}
		if got, _ := s.DistributorWeeks(ctx, "enterest", testType, "2026-10-05", "2026-10-12"); len(got) != 0 {
			t.Fatalf("another type: %+v", got)
		}
		if n, err := s.PruneDistributorWeeks(ctx, "2026-10-19"); err != nil || n != 4 {
			t.Fatalf("pruned (three rows and the mark): %d %v", n, err)
		}
		if done, _ := s.DistributorWeekDone(ctx, "2026-10-12"); done {
			t.Fatal("still done")
		}
	})
}

// Releases' sources by id, in chunks; a release gone is left out, as another tenant's.
func TestReleaseSources(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		now := store.TimeArg(s, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
		want := map[string]string{}
		for i, src := range []string{"tenant", "publisher:p1", "upstream:u1"} {
			b, r := fmt.Sprintf("b%d", i), fmt.Sprintf("r%d", i)
			for _, q := range [][]any{
				{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
					"created_at, created_by, checked) VALUES (?, ?, ?, 'acl', ?, 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'publication', ?, 'os:1:t', ?)",
					b, tn.ID, src, fmt.Sprintf("1.%d", i), strings.Repeat(fmt.Sprint(i), 64), now, true},
				{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state, visibility, seq, created_at, " +
					"created_by, state_changed_at, state_changed_by, version, origin, source) VALUES (?, ?, ?, ?, 'acl', ?, 'linux_amd64', 'v2.0.0', " +
					"'active', 'public', ?, ?, 'os:1:t', ?, 'os:1:t', 1, 'publication', ?)", r, tn.ID, ch.ID, b, fmt.Sprintf("1.%d", i), i + 1, now, now, src},
			} {
				if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
					t.Fatalf("%s: %v", q[0], err)
				}
			}
			want[r] = src
		}
		for _, chunk := range []int{store.ReleaseSourcesChunk, 1, 2} {
			restore := store.SetReleaseSourcesChunk(chunk)
			got, err := s.ReleaseSources(ctx, tn.ID, []string{"r2", "r0", "gone", "r1", "r0"})
			restore()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("chunks of %d: %v %v", chunk, got, err)
			}
		}
		if got, err := s.ReleaseSources(ctx, "another", []string{"r0"}); err != nil || len(got) != 0 {
			t.Fatalf("another tenant's: %v %v", got, err)
		}
	})
}

// Two passes storing one week at once (each believing it holds the daily lease): the week's lock
// orders them, one stores its rows and the mark, the other leaves the week as it is.
func TestDistributorWeekRace(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		for i := range 8 {
			week := store.DayOf(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*i))
			var stored [2]bool
			var errs [2]error
			var wg sync.WaitGroup
			start := make(chan struct{})
			for g := range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					rows := []store.DistributorWeek{{Distributor: "enterest", PackageType: "duckdb_extension", Week: week, Extension: "geo",
						Downloads: int64(g + 1), Public: int64(g + 1)}}
					stored[g], errs[g] = s.PutDistributorWeek(ctx, week, 3, rows)
				}()
			}
			close(start)
			wg.Wait()
			if errs[0] != nil || errs[1] != nil || stored[0] == stored[1] {
				t.Fatalf("week %s: stored %v, errors %v", week, stored, errs)
			}
			got, err := s.DistributorWeeks(ctx, "enterest", "duckdb_extension", week, week)
			winner := int64(1)
			if stored[1] {
				winner = 2
			}
			if err != nil || len(got) != 1 || got[0].Downloads != winner {
				t.Fatalf("week %s: the winner's rows only: %+v %v", week, got, err)
			}
		}
	})
}
