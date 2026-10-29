package stats

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: a distributor's downloads per extension per week, summed over the tenants. A
// public tenant's figures always count; every other tenant's count only in a row with at least the
// minimum of such tenants, downloads and installer-days each on their own.

const (
	// MaxDistributorWeeks is the most weeks one answer covers.
	MaxDistributorWeeks = 53
	// DefaultDistributorWeeks is how many weeks an answer covers when from is not given.
	DefaultDistributorWeeks = 12
)

// ErrWeeks is a range of weeks refused; its message says why.
var ErrWeeks = errors.New("stats: weeks")

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

// DistributorWeeks are the Mondays of the weeks from..to (days, "" for the defaults) asks: widened
// to whole weeks, to clamped to the last complete week (the default), from by default the 12 weeks
// up to to; at most 53 weeks. A refusal wraps ErrWeeks.
func DistributorWeeks(now time.Time, from, to string) ([]string, error) {
	last := LastCompleteWeek(now)
	end := last
	if to != "" {
		t, err := time.Parse(time.DateOnly, to)
		if err != nil {
			return nil, fmt.Errorf("%w: to is a day, YYYY-MM-DD", ErrWeeks)
		}
		if end = monday(t); end.After(last) {
			end = last
		}
	}
	start := end.AddDate(0, 0, -7*(DefaultDistributorWeeks-1))
	if from != "" {
		f, err := time.Parse(time.DateOnly, from)
		if err != nil {
			return nil, fmt.Errorf("%w: from is a day, YYYY-MM-DD", ErrWeeks)
		}
		if to != "" && from > to {
			return nil, fmt.Errorf("%w: from is after to", ErrWeeks)
		}
		start = monday(f)
	}
	if start.After(end) {
		return nil, fmt.Errorf("%w: the last complete week starts %s (a week is answered two days after it ends)", ErrWeeks, store.DayOf(last))
	}
	n := int(end.Sub(start).Hours()/24)/7 + 1
	if n > MaxDistributorWeeks {
		return nil, fmt.Errorf("%w: at most %d weeks", ErrWeeks, MaxDistributorWeeks)
	}
	out := make([]string, n)
	for i := range n {
		out[i] = store.DayOf(start.AddDate(0, 0, 7*i))
	}
	return out, nil
}

// DistributorRow is one week of one extension: its downloads (public: the public tenants' part),
// and its installer-days, omitted when nothing of them may be answered. Withheld says a non-public
// part, of the downloads or of the installer-days, was left out.
type DistributorRow struct {
	Week          string `json:"week"`
	Extension     string `json:"extension"`
	Downloads     int64  `json:"downloads"`
	Public        int64  `json:"public"`
	InstallerDays *int64 `json:"installer_days,omitempty"`
	Withheld      bool   `json:"withheld"`
}

// Distributor adds tenants' parts up per (week, extension): a public tenant's always count; the
// other tenants' downloads count only when at least minTenants of them have downloads in the row,
// and their installer-days only when at least minTenants of them have installers there. Rows are
// sorted by week, then extension.
func Distributor(parts []store.DistributorPart, public map[string]bool, minTenants int) []DistributorRow {
	type acc struct {
		pubD, pubI, npD, npI int64
		nD, nI               int  // non-public tenants with downloads, with installers
		pubInstallers        bool // a public tenant has installers in the row
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
			a.pubInstallers = a.pubInstallers || p.Installers > 0
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
	out := make([]DistributorRow, 0, len(by))
	for k, a := range by {
		r := DistributorRow{Week: k[0], Extension: k[1], Downloads: a.pubD, Public: a.pubD}
		switch {
		case a.nD >= minTenants:
			r.Downloads = sum(r.Downloads, a.npD)
		case a.nD > 0:
			r.Withheld = true
		}
		inst := a.pubI
		switch {
		case a.nI >= minTenants:
			inst = sum(inst, a.npI)
			r.InstallerDays = &inst
		case a.nI > 0:
			r.Withheld = true
			if a.pubInstallers {
				r.InstallerDays = &inst
			}
		default:
			r.InstallerDays = &inst
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b DistributorRow) int {
		return cmp.Or(cmp.Compare(a.Week, b.Week), cmp.Compare(a.Extension, b.Extension))
	})
	return out
}
