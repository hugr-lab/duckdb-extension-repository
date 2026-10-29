package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Spec 0036 phase 2: a distributor's downloads, summed over the tenants into weekly snapshots.

// DistributorPart is one tenant's part of a week's figures for one extension of a distributor: its
// downloads and its installer-days. The tenant is never answered; the caller counts tenants by it.
type DistributorPart struct {
	Week                  string // the week's Monday, YYYY-MM-DD
	Name, TenantID        string
	Downloads, Installers int64
}

// DistributorChunk is the most sources one distributor query names: with a week's bound per week
// (at most 53) and three more, under SQL Server's 2,100 parameters. A test makes it small.
const DistributorChunk = 500

var distributorChunk = DistributorChunk

// DistributorParts reads a distributor's downloads and installers per (week, extension, tenant) in
// weeks given by their Mondays (ascending and consecutive), of channels of a package type, in no
// order. The distributor's sources are every publisher marked with its name, live or removed (the
// mark read now); a tenant whose deletion started counts nothing.
func (s *Store) DistributorParts(ctx context.Context, distributor, packageType string, weeks []string) ([]DistributorPart, error) {
	if len(weeks) == 0 {
		return nil, nil
	}
	sundays := make([]string, len(weeks))
	for i, w := range weeks {
		m, err := time.Parse(time.DateOnly, w)
		if err != nil {
			return nil, fmt.Errorf("%w: a week is its Monday, YYYY-MM-DD", ErrInvalid)
		}
		sundays[i] = DayOf(m.AddDate(0, 0, 6))
	}
	var srcs []string
	for _, q := range []string{"SELECT id FROM publishers WHERE distributor = ?",
		"SELECT publisher_id FROM removed_distributor_publishers WHERE distributor = ?"} {
		if err := s.eachRow(ctx, q, []any{distributor}, func(sc func(...any) error) error {
			var id string
			if err := sc(&id); err != nil {
				return err
			}
			srcs = append(srcs, PublisherSource(id))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.Sort(srcs)
	srcs = slices.Compact(srcs) // a publisher id is the server's: its source is in one tenant only
	// a day maps to its week by a CASE of plain YYYY-MM-DD comparisons, the same in every dialect;
	// the derived table lets every dialect group by it
	week := "CASE"
	var weekArgs []any
	for i, sun := range sundays {
		week += " WHEN day <= ? THEN " + strconv.Itoa(i)
		weekArgs = append(weekArgs, sun)
	}
	week += " END"
	by := map[[3]string]*DistributorPart{}
	for len(srcs) > 0 {
		chunk := srcs[:min(len(srcs), distributorChunk)]
		srcs = srcs[len(chunk):]
		where := "source IN (" + marks(len(chunk)) + ") AND day >= ? AND day <= ? AND channel_id IN (SELECT id FROM channels WHERE package_type = ?) " +
			"AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL)"
		args := slices.Clone(weekArgs)
		for _, src := range chunk {
			args = append(args, src)
		}
		args = append(args, weeks[0], sundays[len(sundays)-1], packageType)
		for _, q := range []struct {
			table, col string
			installers bool
		}{{"download_counts", "count", false}, {"download_installers", "installers", true}} {
			err := s.eachRow(ctx, "SELECT wk, name, tenant_id, SUM(n) FROM (SELECT "+week+" AS wk, name, tenant_id, "+q.col+" AS n FROM "+
				q.table+" WHERE "+where+") x GROUP BY wk, name, tenant_id", args, func(sc func(...any) error) error {
				var i int
				var name, tenant string
				var n int64
				if err := sc(&i, &name, &tenant, &n); err != nil {
					return err
				}
				if i < 0 || i >= len(weeks) {
					return fmt.Errorf("store: a week out of range: %d", i)
				}
				k := [3]string{weeks[i], name, tenant}
				p := by[k]
				if p == nil {
					p = &DistributorPart{Week: weeks[i], Name: name, TenantID: tenant}
					by[k] = p
				}
				if q.installers {
					p.Installers = min(p.Installers+n, MaxCount)
				} else {
					p.Downloads = min(p.Downloads+n, MaxCount)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	out := make([]DistributorPart, 0, len(by))
	for _, p := range by {
		out = append(out, *p)
	}
	return out, nil
}

// Distributors are the distinct distributors' names of the server's marked publishers, live and
// removed, sorted.
func (s *Store) Distributors(ctx context.Context) ([]string, error) {
	var out []string
	for _, q := range []string{"SELECT DISTINCT distributor FROM publishers WHERE distributor IS NOT NULL",
		"SELECT DISTINCT distributor FROM removed_distributor_publishers"} {
		if err := s.eachRow(ctx, q, nil, func(sc func(...any) error) error {
			var d string
			if err := sc(&d); err != nil {
				return err
			}
			out = append(out, d)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// TenantIDsByName are the ids of the tenants of names, by name: a name without a tenant, or of a
// tenant whose deletion started, is left out.
func (s *Store) TenantIDsByName(ctx context.Context, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		var id string
		err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT id FROM tenants WHERE name = ? AND deleting_at IS NULL"), n).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[n] = id
	}
	return out, nil
}

// DistributorWeek is a distributor's snapshot of one complete week of one extension of a package
// type: its downloads (Public: the public tenants' part) and installer-days (nil: left out), and
// whether the downloads' non-public part was left out (Withheld). MinTenants is the minimum the week
// was computed with (read back only).
type DistributorWeek struct {
	Distributor, PackageType, Week, Extension string
	Downloads, Public                         int64
	InstallerDays                             *int64
	Withheld                                  bool
	MinTenants                                int
}

// DistributorWeekDone reports whether a week's snapshots are stored (the week by its Monday).
func (s *Store) DistributorWeekDone(ctx context.Context, week string) (bool, error) {
	var w string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT week FROM distributor_weeks_done WHERE week = ?"), week).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DistributorWeeksSpan is the oldest and the newest week (Mondays) whose snapshots are stored; ok is
// false when none is. Weeks are computed in order and pruned oldest first, so every week between
// them is stored.
func (s *Store) DistributorWeeksSpan(ctx context.Context) (oldest, newest string, ok bool, err error) {
	var lo, hi sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(week), MAX(week) FROM distributor_weeks_done").Scan(&lo, &hi); err != nil {
		return "", "", false, err
	}
	return lo.String, hi.String, hi.Valid, nil
}

// PutDistributorWeek stores a week's snapshots, of every distributor and type, and marks the week
// done, in one transaction under the week's lock; a week done already is left as it is (false): a
// snapshot is never computed again.
func (s *Store) PutDistributorWeek(ctx context.Context, week string, minTenants int, rows []DistributorWeek) (bool, error) {
	stored := false
	err := s.InTx(ctx, "kista/statistics/week/"+week, func(tx *Tx) error {
		var n int
		if err := tx.queryRow(ctx, "SELECT COUNT(*) FROM distributor_weeks_done WHERE week = ?", week).Scan(&n); err != nil || n > 0 {
			return err
		}
		for _, r := range rows {
			if r.Week != week {
				return fmt.Errorf("%w: a row of week %s with week %s", ErrInvalid, r.Week, week)
			}
			var inst any
			if r.InstallerDays != nil {
				inst = *r.InstallerDays
			}
			if _, err := tx.exec(ctx, "INSERT INTO distributor_weeks (distributor, package_type, week, name, downloads, public_downloads, "+
				"installer_days, withheld) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", r.Distributor, r.PackageType, week, r.Extension, r.Downloads, r.Public,
				inst, r.Withheld); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, "INSERT INTO distributor_weeks_done (week, min_tenants, done_at) VALUES (?, ?, ?)", week, minTenants,
			tx.s.d.timeArg(tx.Now())); err != nil {
			return err
		}
		stored = true
		return nil
	})
	return stored, err
}

// DistributorWeeks reads a distributor's snapshots of a package type in weeks from..to (Mondays),
// by week and extension.
func (s *Store) DistributorWeeks(ctx context.Context, distributor, packageType, from, to string) ([]DistributorWeek, error) {
	var out []DistributorWeek
	err := s.eachRow(ctx, "SELECT w.week, w.name, w.downloads, w.public_downloads, w.installer_days, w.withheld, d.min_tenants FROM distributor_weeks w "+
		"JOIN distributor_weeks_done d ON d.week = w.week WHERE w.distributor = ? AND w.package_type = ? AND w.week >= ? AND w.week <= ? "+
		"ORDER BY w.week, w.name",
		[]any{distributor, packageType, from, to}, func(sc func(...any) error) error {
			r := DistributorWeek{Distributor: distributor, PackageType: packageType}
			var inst sql.NullInt64
			if err := sc(&r.Week, &r.Extension, &r.Downloads, &r.Public, &inst, &r.Withheld, &r.MinTenants); err != nil {
				return err
			}
			if inst.Valid {
				r.InstallerDays = &inst.Int64
			}
			out = append(out, r)
			return nil
		})
	return out, err
}

// PruneDistributorWeeks deletes the snapshots, and the marks, of weeks before a Monday.
func (s *Store) PruneDistributorWeeks(ctx context.Context, before string) (int, error) {
	total := 0
	for _, t := range []string{"distributor_weeks", "distributor_weeks_done"} {
		res, err := s.db.ExecContext(ctx, s.d.rebind("DELETE FROM "+t+" WHERE week < ?"), before)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}
