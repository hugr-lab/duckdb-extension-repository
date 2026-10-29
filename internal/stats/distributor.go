package stats

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: a distributor's downloads per extension per week, summed over the tenants. A
// public tenant's figures always count; every other tenant's count only in a row with at least the
// minimum of such tenants, downloads and installer-days each on their own. Each week is computed
// once, when it is complete, and stored: answers read only those snapshots.

const (
	// MaxDistributorWeeks is the most weeks one answer covers.
	MaxDistributorWeeks = 53
	// DefaultDistributorWeeks is how many weeks an answer covers when from is not given.
	DefaultDistributorWeeks = 12
	// KeepDistributorWeeks is how many weeks of snapshots are kept, up to the last complete one.
	KeepDistributorWeeks = 106
)

const week = 7 * 24 * time.Hour

// errLeaseLost ends a pass that no longer holds its lease.
var errLeaseLost = errors.New("stats: the daily lease was lost")

// WeeksError is a range of weeks refused; Reason says why. NotComputed marks a range wholly after
// the newest week computed: answerable once its weeks are.
type WeeksError struct {
	Reason      string
	NotComputed bool
}

func (e *WeeksError) Error() string { return "stats: weeks: " + e.Reason }

// monday is the Monday of t's ISO week, at midnight UTC.
func monday(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
}

// LastCompleteWeek is the Monday of the last complete ISO week (UTC) that ended at least two days
// before now: its counts are flushed and its days' installers counted.
func LastCompleteWeek(now time.Time) time.Time {
	// a week ends at its Sunday's end; two days later is its Wednesday after, nine days past Monday
	return monday(now.UTC().AddDate(0, 0, -9))
}

// DistributorWeeks are the Mondays of the weeks from..to (days, "" for the defaults) asks, within
// the weeks oldest..newest (Mondays) an answer may hold, the weeks computed: widened to whole weeks,
// to clamped to newest (the default), from by default the 12 weeks up to to and clamped to oldest;
// at most 53 weeks. A refusal is a *WeeksError.
func DistributorWeeks(oldest, newest time.Time, from, to string) ([]string, error) {
	last := newest
	end := last
	if to != "" {
		t, err := time.Parse(time.DateOnly, to)
		if err != nil {
			return nil, &WeeksError{Reason: "to is a day, YYYY-MM-DD"}
		}
		if end = monday(t); end.After(last) {
			end = last
		}
	}
	start := end.AddDate(0, 0, -7*(DefaultDistributorWeeks-1))
	if from != "" {
		f, err := time.Parse(time.DateOnly, from)
		if err != nil {
			return nil, &WeeksError{Reason: "from is a day, YYYY-MM-DD"}
		}
		if to != "" && from > to {
			return nil, &WeeksError{Reason: "from is after to"}
		}
		start = monday(f)
	}
	if start.After(end) {
		return nil, &WeeksError{Reason: "the newest week computed starts " + store.DayOf(last) + " (a week is computed two days after it ends)", NotComputed: true}
	}
	if start.Before(oldest) {
		if start = oldest; start.After(end) {
			return nil, &WeeksError{Reason: "the oldest week kept starts " + store.DayOf(oldest)}
		}
	}
	n := int(end.Sub(start)/week) + 1
	if n > MaxDistributorWeeks {
		return nil, &WeeksError{Reason: "at most 53 weeks"}
	}
	out := make([]string, n)
	for i := range n {
		out[i] = store.DayOf(start.AddDate(0, 0, 7*i))
	}
	return out, nil
}

// Distributor adds tenants' parts up per (week, extension), sorted by week, then extension: a
// public tenant's always count. The other tenants' downloads count only when at least minTenants of
// them have downloads in the row; with fewer, they are left out and the row is withheld (its
// downloads are the public part, its installer-days left out). Their installer-days count only when
// at least minTenants of them have installers in the row; with fewer, the row's installer-days are
// left out, never answered in part. The rows' distributor and type are the caller's.
func Distributor(parts []store.DistributorPart, public map[string]bool, minTenants int) []store.DistributorWeek {
	type acc struct {
		pubD, pubI, npD, npI int64
		nD, nI               int // non-public tenants with downloads, with installers
	}
	by := map[[2]string]*acc{}
	for _, p := range parts {
		k := [2]string{p.Week, p.Name}
		a := by[k]
		if a == nil {
			a = &acc{}
			by[k] = a
		}
		if public[p.TenantID] {
			a.pubD, a.pubI = sum(a.pubD, p.Downloads), sum(a.pubI, p.Installers)
			continue
		}
		a.npD, a.npI = sum(a.npD, p.Downloads), sum(a.npI, p.Installers)
		if p.Downloads > 0 {
			a.nD++
		}
		if p.Installers > 0 {
			a.nI++
		}
	}
	out := make([]store.DistributorWeek, 0, len(by))
	for k, a := range by {
		r := store.DistributorWeek{Week: k[0], Extension: k[1], Downloads: a.pubD, Public: a.pubD}
		if a.nD > 0 && a.nD < minTenants {
			r.Withheld = true // downloads are the public part; installer-days left out
			out = append(out, r)
			continue
		}
		r.Downloads = sum(r.Downloads, a.npD)
		switch {
		case a.nI >= minTenants:
			inst := sum(a.pubI, a.npI)
			r.InstallerDays = &inst
		case a.nI == 0:
			inst := a.pubI
			r.InstallerDays = &inst
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b store.DistributorWeek) int {
		return cmp.Or(cmp.Compare(a.Week, b.Week), cmp.Compare(a.Extension, b.Extension))
	})
	return out
}

// distributorWeeks computes the snapshots of the complete weeks after the newest one stored, oldest
// first, each once. With none stored (the first pass, a one-time backfill), it starts at the oldest
// week within KeepDistributorWeeks of the last complete one whose days the statistics' retention
// still holds whole; it never goes back to older weeks afterwards. Then it deletes the snapshots older
// than KeepDistributorWeeks. held says whether the pass still holds its lease.
func (d *Daily) distributorWeeks(ctx context.Context, now time.Time, held func() bool) error {
	last := LastCompleteWeek(now)
	first := last.AddDate(0, 0, -7*(KeepDistributorWeeks-1))
	_, newest, stored, err := d.Store.DistributorWeeksSpan(ctx)
	if err != nil {
		return err
	}
	if stored {
		n, err := time.Parse(time.DateOnly, newest)
		if err != nil {
			return err
		}
		if n = n.AddDate(0, 0, 7); n.After(first) {
			first = n
		}
	} else if d.Retention > 0 { // the first Monday on or after the retention's first day: its week is held whole
		c := now.UTC().Add(-d.Retention)
		cday := time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, time.UTC)
		m := monday(cday)
		if m.Before(cday) {
			m = m.AddDate(0, 0, 7)
		}
		if m.After(first) {
			first = m
		}
	}
	var dists []string
	public := map[string]bool{}
	read := false // the distributors and public tenants: once a pass, and only when a week is computed
	for w := first; !w.After(last) && ctx.Err() == nil; w = w.AddDate(0, 0, 7) {
		day := store.DayOf(w)
		if !read {
			read = true
			if dists, err = d.Store.Distributors(ctx); err != nil {
				return err
			}
			ids, err := d.Store.TenantIDsByName(ctx, d.PublicTenants)
			if err != nil {
				return err
			}
			for _, id := range ids {
				public[id] = true
			}
		}
		var rows []store.DistributorWeek
		for _, dist := range dists {
			for _, typ := range pkgtype.Types() {
				parts, err := d.Store.DistributorParts(ctx, dist, typ, []string{day})
				if err != nil {
					return err
				}
				for _, r := range Distributor(parts, public, d.minTenants()) {
					r.Distributor, r.PackageType = dist, typ
					rows = append(rows, r)
				}
			}
		}
		if _, err := d.Store.PutDistributorWeek(ctx, day, d.minTenants(), rows); err != nil {
			return err
		}
		if !held() {
			return errLeaseLost
		}
	}
	if n, err := d.Store.PruneDistributorWeeks(ctx, store.DayOf(last.AddDate(0, 0, -7*(KeepDistributorWeeks-1)))); err != nil {
		return err
	} else if n > 0 {
		d.Log.Info("stats: pruned distributor snapshots", "rows", n)
	}
	return nil
}

// minTenants is the minimum the snapshots apply: MinTenants, below 2 the default, 3.
func (d *Daily) minTenants() int {
	if d.MinTenants < 2 {
		return 3
	}
	return d.MinTenants
}
