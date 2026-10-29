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

// DownloadKey is what a download count counts by (spec 0010 phase 2a): no principal, no address.
type DownloadKey struct {
	TenantID, ChannelID, Name, PackageVersion, Platform, DuckDBVersion string
	// Source is whose entry was downloaded (spec 0036 phase 2): the release's source.
	Source        string
	Day           string // YYYY-MM-DD, UTC
	Authenticated bool
}

// DayOf is a time's UTC day as statistics store it.
func DayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// AddDownloadCounts adds counts to the stored ones: an upsert per key, in key order (two replicas
// flushing the same keys lock them in the same order), in transactions of at most 500 keys; on SQL
// Server an update, then an insert where none was updated, under the lock kista/download_counts.
// On an error, the counts of the transactions that committed are not returned: done holds them.
func (s *Store) AddDownloadCounts(ctx context.Context, counts map[DownloadKey]int64) (done []DownloadKey, err error) {
	return addCounts(ctx, s, "kista/download_counts", counts, func(a, b DownloadKey) int {
		return cmp.Or(cmp.Compare(a.TenantID, b.TenantID), cmp.Compare(a.ChannelID, b.ChannelID), cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Name, b.Name), cmp.Compare(a.PackageVersion, b.PackageVersion), cmp.Compare(a.Platform, b.Platform),
			cmp.Compare(a.DuckDBVersion, b.DuckDBVersion), cmp.Compare(a.Day, b.Day), cmp.Compare(boolInt(a.Authenticated), boolInt(b.Authenticated)))
	}, func(k DownloadKey) string { return k.TenantID }, func(tx *Tx, k DownloadKey, n int64) error {
		return tx.upsertCount(ctx, "download_counts", "count",
			[]string{"tenant_id", "channel_id", "source", "name", "package_version", "platform", "duckdb_version", "day", "authenticated"},
			[]any{k.TenantID, k.ChannelID, k.Source, k.Name, k.PackageVersion, k.Platform, k.DuckDBVersion, k.Day, k.Authenticated}, n)
	})
}

// addCounts adds counts in key order, in transactions of at most 500 keys, skipping the keys of
// tenants being deleted (spec 0019); on SQL Server under lock. done holds the keys of the
// transactions that committed.
func addCounts[K comparable](ctx context.Context, s *Store, lock string, counts map[K]int64, order func(a, b K) int,
	tenant func(K) string, add func(tx *Tx, k K, n int64) error) (done []K, err error) {
	keys := make([]K, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, order)
	if s.d.Name != "sqlserver" {
		lock = ""
	}
	for len(keys) > 0 {
		batch := keys[:min(len(keys), 500)]
		if err := s.InTx(ctx, lock, func(tx *Tx) error {
			ids := make([]string, 0, len(batch))
			for _, k := range batch {
				ids = append(ids, tenant(k))
			}
			closed, err := tx.closedTenants(ctx, ids)
			if err != nil {
				return err
			}
			for _, k := range batch {
				if closed[tenant(k)] {
					continue // spec 0019: done, so the counter does not keep it
				}
				if err := add(tx, k, counts[k]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return done, err
		}
		done = append(done, batch...)
		keys = keys[len(batch):]
	}
	return done, nil
}

// MaxCount is the most a stored count holds: additions saturate there (2^53-1, exact in JSON), so
// that the sum of clients' claims never overflows a bigint.
const MaxCount = 1<<53 - 1

// upsertCount adds n to the row of table keyed by cols = args, saturating at MaxCount: an insert
// that adds on conflict, or on SQL Server an update, then an insert where none was updated.
func (tx *Tx) upsertCount(ctx context.Context, table, col string, cols []string, args []any, n int64) error {
	list := strings.Join(cols, ", ")
	marks := strings.Repeat("?, ", len(cols)) + "?"
	if tx.s.d.Name != "sqlserver" {
		cur := table + "." + col
		_, err := tx.exec(ctx, "INSERT INTO "+table+" ("+list+", "+col+") VALUES ("+marks+") ON CONFLICT ("+list+") DO UPDATE SET "+
			col+" = CASE WHEN "+cur+" > "+maxCount+" - excluded."+col+" THEN "+maxCount+" ELSE "+cur+" + excluded."+col+" END",
			append(args, min(n, MaxCount))...)
		return err
	}
	n = min(n, MaxCount)
	res, err := tx.exec(ctx, "UPDATE "+table+" SET "+col+" = CASE WHEN "+col+" > "+maxCount+" - ? THEN "+maxCount+" ELSE "+col+" + ? END WHERE "+
		strings.Join(cols, " = ? AND ")+" = ?", append([]any{n, n}, args...)...)
	if err != nil {
		return err
	}
	m, err := res.RowsAffected()
	if err != nil || m == 1 {
		return err
	}
	_, err = tx.exec(ctx, "INSERT INTO "+table+" ("+list+", "+col+") VALUES ("+marks+")", append(args, n)...)
	return err
}

var maxCount = strconv.FormatInt(MaxCount, 10)

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InstallerKey is what daily installers are counted by; Source is the installed release's (spec 0036
// phase 2: "" when the release is gone).
type InstallerKey struct {
	TenantID, ChannelID, Source, Name, PackageVersion, Platform, Day string
}

// ReleaseSources are the sources of a tenant's releases by id (spec 0036 phase 2: an install
// event written before events carried the source); a release gone is left out.
func (s *Store) ReleaseSources(ctx context.Context, tenantID string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		args := []any{tenantID}
		for _, id := range chunk {
			args = append(args, id)
		}
		if err := s.eachRow(ctx, "SELECT id, source FROM releases WHERE tenant_id = ? AND id IN ("+
			strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")+")", args, func(sc func(...any) error) error {
			var id, src string
			if err := sc(&id, &src); err != nil {
				return err
			}
			out[id] = src
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// StatisticsDayDone reports whether a day's installers are stored.
func (s *Store) StatisticsDayDone(ctx context.Context, day string) (bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT day FROM statistics_days WHERE day = ?"), day).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Daily is a finished day's distinct principals: installers per release (spec 0010) and users of
// usage reports per (channel, name, version) (spec 0012 phase 1b).
type Daily struct {
	Installers map[InstallerKey]int64
	Users      map[UsageUserKey]int64
}

// PutDaily stores a day's installers and users (replacing what a pass interrupted before it left)
// and marks the day done, in one transaction; the day's lock orders two passes that both believe
// they hold the daily lease (one lost it mid-pass).
func (s *Store) PutDaily(ctx context.Context, day string, d Daily) error {
	return s.InTx(ctx, "kista/statistics/"+day, func(tx *Tx) error {
		for _, t := range []string{"download_installers", "usage_users"} {
			if _, err := tx.exec(ctx, "DELETE FROM "+t+" WHERE day = ?", day); err != nil {
				return err
			}
		}
		ids := make([]string, 0, len(d.Installers)+len(d.Users))
		for k := range d.Installers {
			ids = append(ids, k.TenantID)
		}
		for k := range d.Users {
			ids = append(ids, k.TenantID)
		}
		closed, err := tx.closedTenants(ctx, ids)
		if err != nil {
			return err
		}
		for k, n := range d.Installers {
			if closed[k.TenantID] {
				continue
			}
			if _, err := tx.exec(ctx, "INSERT INTO download_installers (tenant_id, channel_id, source, name, package_version, platform, day, "+
				"installers) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", k.TenantID, k.ChannelID, k.Source, k.Name, k.PackageVersion, k.Platform, day, n); err != nil {
				return err
			}
		}
		for k, n := range d.Users {
			if closed[k.TenantID] {
				continue
			}
			if _, err := tx.exec(ctx, "INSERT INTO usage_users (tenant_id, channel_id, name, package_version, day, users) "+
				"VALUES (?, ?, ?, ?, ?, ?)", k.TenantID, k.ChannelID, k.Name, k.PackageVersion, day, n); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, "DELETE FROM statistics_days WHERE day = ?", day); err != nil {
			return err
		}
		_, err = tx.exec(ctx, "INSERT INTO statistics_days (day, done_at) VALUES (?, ?)", day, tx.s.d.timeArg(tx.Now()))
		return err
	})
}

// PruneStatistics deletes counts, installers, usage and day marks before a day.
func (s *Store) PruneStatistics(ctx context.Context, before string) (int, error) {
	total := 0
	for _, t := range []string{"download_counts", "download_installers", "usage_counts", "usage_users", "usage_received", "statistics_days"} {
		res, err := s.db.ExecContext(ctx, s.d.rebind("DELETE FROM "+t+" WHERE day < ?"), before)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// StatsFilter selects a tenant's statistics: days From..To (inclusive), a channel, an extension.
type StatsFilter struct {
	From, To  string
	ChannelID string
	// PackageType keeps the rows of channels of a type (spec 0034: the extension dimension is (type,
	// name), and a tenant's totals are per type); "": every type.
	PackageType string
	Name        string
	Names       []string // at most these extensions (a caller's scope); nil: any
}

// StatsRow is a download count with its installers, at the dimensions a query keeps (the others
// empty). Installers are summed over the kept rows (a principal installing on two days or two
// platforms counts twice). Source is the downloaded release's (spec 0036 phase 2; "" before it).
type StatsRow struct {
	ChannelID, Source, Name, PackageVersion, Platform, DuckDBVersion, Day string
	Count, Authenticated, Installers                                      int64
}

// DownloadRows reads a tenant's counts and installers per (channel, name, version, platform,
// DuckDB version, day) within a filter, keeping the rows keep takes (a caller's scope); more than
// limit rows kept is ErrInvalid (the caller narrows the range).
func (s *Store) DownloadRows(ctx context.Context, tenantID string, f StatsFilter, limit int, keep func(channelID, name string) bool) ([]StatsRow, error) {
	where, args := statsWhere(tenantID, f.From, f.To, f)
	var out []StatsRow
	add := func(r StatsRow) error {
		if keep != nil && !keep(r.ChannelID, r.Name) {
			return nil
		}
		out = append(out, r)
		if len(out) > limit {
			return fmt.Errorf("%w: more than %d rows; narrow the range", ErrInvalid, limit)
		}
		return nil
	}
	err := s.eachRow(ctx, "SELECT channel_id, source, name, package_version, platform, duckdb_version, day, authenticated, count "+
		"FROM download_counts WHERE "+where, args, func(sc func(...any) error) error {
		var r StatsRow
		var auth bool
		if err := sc(&r.ChannelID, &r.Source, &r.Name, &r.PackageVersion, &r.Platform, &r.DuckDBVersion, &r.Day, &auth, &r.Count); err != nil {
			return err
		}
		if auth {
			r.Authenticated = r.Count
		}
		return add(r)
	})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, "SELECT channel_id, source, name, package_version, platform, day, installers FROM download_installers WHERE "+where,
		args, func(sc func(...any) error) error {
			var r StatsRow
			if err := sc(&r.ChannelID, &r.Source, &r.Name, &r.PackageVersion, &r.Platform, &r.Day, &r.Installers); err != nil {
				return err
			}
			return add(r)
		})
	return out, err
}

func statsWhere(tenantID, from, to string, f StatsFilter) (string, []any) {
	where, args := "tenant_id = ? AND day >= ? AND day <= ?", []any{tenantID, from, to}
	if f.ChannelID != "" {
		where += " AND channel_id = ?"
		args = append(args, f.ChannelID)
	}
	if f.PackageType != "" {
		where += " AND channel_id IN (SELECT id FROM channels WHERE tenant_id = ? AND package_type = ?)"
		args = append(args, tenantID, f.PackageType)
	}
	if f.Name != "" {
		where += " AND name = ?"
		args = append(args, f.Name)
	}
	if len(f.Names) > 0 {
		where += " AND name IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(f.Names)), ", ") + ")"
		for _, n := range f.Names {
			args = append(args, n)
		}
	}
	return where, args
}

// eachRow runs a query and calls fn with each row's scanner.
func (s *Store) eachRow(ctx context.Context, q string, args []any, fn func(scan func(...any) error) error) error {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ReleaseDownloads is a release's downloads in the last 7 and 30 days and its last download day.
type ReleaseDownloads struct {
	ChannelID, Name, PackageVersion, Platform string
	Last7, Last30                             int64
	LastDay                                   string
}

// ReleaseStats reads per (channel, name, version, platform) with a download kept in statistics its
// downloads of the 7 and 30 days to today and its last download day.
func (s *Store) ReleaseStats(ctx context.Context, tenantID, today string, f StatsFilter) ([]ReleaseDownloads, error) {
	t, err := time.Parse(time.DateOnly, today)
	if err != nil {
		return nil, fmt.Errorf("%w: a day", ErrInvalid)
	}
	from30, from7 := DayOf(t.AddDate(0, 0, -29)), DayOf(t.AddDate(0, 0, -6))
	where, args := statsWhere(tenantID, "0000-00-00", today, f)
	by := map[[4]string]*ReleaseDownloads{}
	var order [][4]string
	// ordered by the release: the primary key holds the source since spec 0036, so a grouping no
	// longer comes out in this order by itself
	err = s.eachRow(ctx, "SELECT channel_id, name, package_version, platform, MAX(day) FROM download_counts WHERE "+where+
		" GROUP BY channel_id, name, package_version, platform ORDER BY channel_id, name, package_version, platform", args,
		func(sc func(...any) error) error {
			var k [4]string
			var last string
			if err := sc(&k[0], &k[1], &k[2], &k[3], &last); err != nil {
				return err
			}
			by[k] = &ReleaseDownloads{ChannelID: k[0], Name: k[1], PackageVersion: k[2], Platform: k[3], LastDay: last}
			order = append(order, k)
			return nil
		})
	if err != nil {
		return nil, err
	}
	where, args = statsWhere(tenantID, from30, today, f)
	err = s.eachRow(ctx, "SELECT channel_id, name, package_version, platform, day, SUM(count) FROM download_counts WHERE "+where+
		" GROUP BY channel_id, name, package_version, platform, day", args, func(sc func(...any) error) error {
		var k [4]string
		var day string
		var n int64
		if err := sc(&k[0], &k[1], &k[2], &k[3], &day, &n); err != nil {
			return err
		}
		if r := by[k]; r != nil {
			r.Last30 += n
			if day >= from7 {
				r.Last7 += n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReleaseDownloads, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out, nil
}

// InstallEvents calls fn with every install event of a tenant's day, in pages, oldest first.
func (s *Store) InstallEvents(ctx context.Context, tenantID, day string, fn func(Event) error) error {
	return s.DayEvents(ctx, tenantID, "install", day, fn)
}

// DayEvents calls fn with every event of a kind in a tenant's day, in pages, oldest first (the
// index on tenant_id, kind, at).
func (s *Store) DayEvents(ctx context.Context, tenantID, kind, day string, fn func(Event) error) error {
	from, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return fmt.Errorf("%w: a day", ErrInvalid)
	}
	to := from.AddDate(0, 0, 1)
	var afterAt time.Time
	afterID := ""
	for {
		q := "SELECT " + eventCols + " FROM events WHERE tenant_id = ? AND kind = ? AND at >= ? AND at < ?"
		args := []any{tenantID, kind, s.d.timeArg(from), s.d.timeArg(to)}
		if afterID != "" {
			q += " AND (at > ? OR at = ? AND id > ?)"
			args = append(args, s.d.timeArg(afterAt), s.d.timeArg(afterAt), afterID)
		}
		q += " ORDER BY at, id"
		const page = 1000
		if s.d.Name == "sqlserver" {
			q += fmt.Sprintf(" OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", page)
		} else {
			q += fmt.Sprintf(" LIMIT %d", page)
		}
		rows, err := s.db.QueryContext(ctx, s.d.rebind(q), args...)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return err
			}
			n++
			afterAt, afterID = e.At, e.ID
			if err := fn(e); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if n < page {
			return nil
		}
	}
}

// closedTenants are those of ids whose deletion started (spec 0019): no statistics are written for
// them.
func (tx *Tx) closedTenants(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := "SELECT id FROM tenants WHERE deleting_at IS NOT NULL AND id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ") + ")"
		if err := tx.each(ctx, q, args, func(r *sql.Rows) error {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			out[id] = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}
