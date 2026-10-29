package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Spec 0036 phase 2: a distributor's downloads, summed over the tenants.

// DistributorPart is one tenant's part of a week's figures for one extension of a distributor: its
// downloads and its installer-days. The tenant is never answered; the caller counts tenants by it.
type DistributorPart struct {
	Week                  string // the week's Monday, YYYY-MM-DD
	Name, TenantID        string
	Downloads, Installers int64
}

// distributorChunk is the most sources one query names (under SQL Server's parameter limit, with
// the weeks' bounds beside them); a test makes it small.
var distributorChunk = 500

// DistributorParts reads a distributor's downloads and installers per (week, extension, tenant) in
// weeks given by their Mondays (ascending and consecutive), of channels of a package type. The
// distributor's sources are every publisher marked with its name, live or removed (the mark read
// now); a tenant whose deletion started counts nothing.
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
	type src struct{ tenant, source string }
	var srcs []src
	for _, q := range []string{"SELECT tenant_id, id FROM publishers WHERE distributor = ?",
		"SELECT tenant_id, publisher_id FROM removed_distributor_publishers WHERE distributor = ?"} {
		if err := s.eachRow(ctx, q, []any{distributor}, func(sc func(...any) error) error {
			var x src
			if err := sc(&x.tenant, &x.source); err != nil {
				return err
			}
			x.source = PublisherSource(x.source)
			srcs = append(srcs, x)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(srcs, func(a, b src) int { return cmp.Or(cmp.Compare(a.tenant, b.tenant), cmp.Compare(a.source, b.source)) })
	srcs = slices.Compact(srcs)
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
	var order [][3]string
	for len(srcs) > 0 {
		chunk := srcs[:min(len(srcs), distributorChunk)]
		srcs = srcs[len(chunk):]
		in := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		where := "source IN (" + in + ") AND day >= ? AND day <= ? AND channel_id IN (SELECT id FROM channels WHERE package_type = ?) " +
			"AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL)"
		args := slices.Clone(weekArgs)
		for _, x := range chunk {
			args = append(args, x.source)
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
					order = append(order, k)
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
	out := make([]DistributorPart, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out, nil
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
