package stats_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: complete ISO weeks that ended at least two days ago, widened, clamped and
// bounded.
func TestDistributorWeeks(t *testing.T) {
	// the week of Monday 2026-10-19 ends on Sunday 25th, and is answered from Wednesday 28th
	for _, c := range []struct {
		now  time.Time
		last string
	}{
		{time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), "2026-10-19"},
		{time.Date(2026, 10, 27, 23, 59, 59, 0, time.UTC), "2026-10-12"},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), "2026-10-19"},
		{time.Date(2026, 11, 4, 1, 0, 0, 0, time.FixedZone("x", 3*3600)), "2026-10-19"}, // 3rd, 22:00 UTC
	} {
		if got := store.DayOf(stats.LastCompleteWeek(c.now)); got != c.last {
			t.Errorf("%v: %s, want %s", c.now, got, c.last)
		}
	}
	now := time.Date(2027, 1, 14, 10, 0, 0, 0, time.UTC) // the last complete week: Monday 2027-01-04
	for name, c := range map[string]struct {
		from, to    string
		first, last string
		n           int
	}{
		"default":             {"", "", "2026-10-19", "2027-01-04", 12},
		"widened":             {"2026-12-31", "2027-01-02", "2026-12-28", "2026-12-28", 1},
		"across a year":       {"2026-12-27", "2027-01-05", "2026-12-21", "2027-01-04", 3},
		"to clamped":          {"2026-12-28", "2027-03-01", "2026-12-28", "2027-01-04", 2},
		"to only":             {"", "2026-12-30", "2026-10-12", "2026-12-28", 12},
		"53 weeks":            {"2026-01-05", "2027-01-04", "2026-01-05", "2027-01-04", 53},
		"from a later Sunday": {"2026-01-11", "2027-01-10", "2026-01-05", "2027-01-04", 53},
	} {
		weeks, err := stats.DistributorWeeks(now, c.from, c.to)
		if err != nil || len(weeks) != c.n || weeks[0] != c.first || weeks[len(weeks)-1] != c.last {
			t.Errorf("%s: %v %v", name, weeks, err)
			continue
		}
		for i := 1; i < len(weeks); i++ {
			a, _ := time.Parse(time.DateOnly, weeks[i-1])
			if b, _ := time.Parse(time.DateOnly, weeks[i]); b.Sub(a) != 7*24*time.Hour || b.Weekday() != time.Monday {
				t.Errorf("%s: not consecutive Mondays: %v", name, weeks)
			}
		}
	}
	for name, c := range map[string][2]string{
		"54 weeks":            {"2025-12-29", "2027-01-04"},
		"from after to":       {"2027-01-05", "2027-01-04"},
		"after the last week": {"2027-01-11", ""},
		"from not a day":      {"2027-1-4", ""},
		"to not a day":        {"", "tomorrow"},
	} {
		if _, err := stats.DistributorWeeks(now, c[0], c[1]); !errors.Is(err, stats.ErrWeeks) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The figures: public tenants always counted (and never toward the minimum), every other tenant's
// downloads only with the minimum of such tenants in the row, and installer-days on their own.
func TestDistributorMinimum(t *testing.T) {
	part := func(week, name, tenant string, d, i int64) store.DistributorPart {
		return store.DistributorPart{Week: week, Name: name, TenantID: tenant, Downloads: d, Installers: i}
	}
	const w1, w2 = "2026-10-12", "2026-10-19"
	parts := []store.DistributorPart{
		// w1 geo: three non-public tenants with downloads, two with installers; a public one
		part(w1, "geo", "a", 5, 1), part(w1, "geo", "b", 6, 0), part(w1, "geo", "c", 7, 2), part(w1, "geo", "pub", 100, 10),
		// w1 h3: one non-public tenant only
		part(w1, "h3", "a", 4, 1),
		// w2 geo: public only (many tenants' worth would not matter)
		part(w2, "geo", "pub", 50, 0), part(w2, "geo", "pub2", 1, 3),
		// w2 h3: three with installers, one public with downloads only
		part(w2, "h3", "a", 1, 1), part(w2, "h3", "b", 1, 1), part(w2, "h3", "c", 1, 1), part(w2, "h3", "pub", 9, 0),
	}
	public := map[string]bool{"pub": true, "pub2": true}
	n := func(v int64) *int64 { return &v }
	want := []stats.DistributorRow{
		{Week: w1, Extension: "geo", Downloads: 118, Public: 100, InstallerDays: n(10), Withheld: true},
		{Week: w1, Extension: "h3", Downloads: 0, Public: 0, Withheld: true},
		{Week: w2, Extension: "geo", Downloads: 51, Public: 51, InstallerDays: n(3)},
		{Week: w2, Extension: "h3", Downloads: 12, Public: 9, InstallerDays: n(3)},
	}
	same := func(a, b stats.DistributorRow) bool {
		return a.Week == b.Week && a.Extension == b.Extension && a.Downloads == b.Downloads && a.Public == b.Public &&
			a.Withheld == b.Withheld && (a.InstallerDays == nil) == (b.InstallerDays == nil) &&
			(a.InstallerDays == nil || *a.InstallerDays == *b.InstallerDays)
	}
	if got := stats.Distributor(parts, public, 3); !slices.EqualFunc(got, want, same) {
		t.Fatalf("minimum 3:\n got %+v\nwant %+v", got, want)
	}
	// at 2, w1 geo's installers count too, and w1 h3 stays withheld
	got := stats.Distributor(parts, public, 2)
	if got[0].Withheld || *got[0].InstallerDays != 13 || got[0].Downloads != 118 || !got[1].Withheld {
		t.Fatalf("minimum 2: %+v", got)
	}
	// no public tenants: every tenant is under the minimum (four in w1 geo, fewer than 5)
	got = stats.Distributor(parts, nil, 5)
	if got[0].Downloads != 0 || !got[0].Withheld || got[0].InstallerDays != nil || got[3].Downloads != 0 || got[3].InstallerDays != nil {
		t.Fatalf("without public tenants, a minimum of 5: %+v", got)
	}
}
