package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DownloadKey is what a download count counts by (spec 0010 phase 2a): no principal, no address.
type DownloadKey struct {
	TenantID, ChannelID, Name, ExtVersion, Platform, DuckDBVersion string
	Day                                                            string // YYYY-MM-DD, UTC
	Authenticated                                                  bool
}

// DayOf is a time's UTC day as statistics store it.
func DayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// AddDownloadCounts adds counts to the stored ones: an upsert per key, in key order (two replicas
// flushing the same keys lock them in the same order), in transactions of at most 500 keys; on SQL
// Server an update, then an insert where none was updated, under the lock kista/download_counts.
// On an error, the counts of the transactions that committed are not returned: done holds them.
func (s *Store) AddDownloadCounts(ctx context.Context, counts map[DownloadKey]int64) (done []DownloadKey, err error) {
	keys := make([]DownloadKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b DownloadKey) int {
		return cmp.Or(cmp.Compare(a.TenantID, b.TenantID), cmp.Compare(a.ChannelID, b.ChannelID), cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.ExtVersion, b.ExtVersion), cmp.Compare(a.Platform, b.Platform), cmp.Compare(a.DuckDBVersion, b.DuckDBVersion),
			cmp.Compare(a.Day, b.Day), cmp.Compare(boolInt(a.Authenticated), boolInt(b.Authenticated)))
	})
	lock := ""
	if s.d.Name == "sqlserver" {
		lock = "kista/download_counts"
	}
	for len(keys) > 0 {
		batch := keys[:min(len(keys), 500)]
		if err := s.InTx(ctx, lock, func(tx *Tx) error {
			for _, k := range batch {
				if err := tx.addDownloadCount(ctx, k, counts[k]); err != nil {
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (tx *Tx) addDownloadCount(ctx context.Context, k DownloadKey, n int64) error {
	args := []any{k.TenantID, k.ChannelID, k.Name, k.ExtVersion, k.Platform, k.DuckDBVersion, k.Day, k.Authenticated}
	if tx.s.d.Name != "sqlserver" {
		_, err := tx.exec(ctx, "INSERT INTO download_counts (tenant_id, channel_id, name, ext_version, platform, duckdb_version, "+
			"day, authenticated, count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, channel_id, name, ext_version, "+
			"platform, duckdb_version, day, authenticated) DO UPDATE SET count = download_counts.count + excluded.count",
			append(args, n)...)
		return err
	}
	res, err := tx.exec(ctx, "UPDATE download_counts SET count = count + ? WHERE tenant_id = ? AND channel_id = ? AND name = ? "+
		"AND ext_version = ? AND platform = ? AND duckdb_version = ? AND day = ? AND authenticated = ?", append([]any{n}, args...)...)
	if err != nil {
		return err
	}
	m, err := res.RowsAffected()
	if err != nil || m == 1 {
		return err
	}
	_, err = tx.exec(ctx, "INSERT INTO download_counts (tenant_id, channel_id, name, ext_version, platform, duckdb_version, "+
		"day, authenticated, count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", append(args, n)...)
	return err
}

// InstallerKey is what daily installers are counted by.
type InstallerKey struct {
	TenantID, ChannelID, Name, ExtVersion, Platform, Day string
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

// PutInstallers stores a day's installers (replacing what a pass interrupted before it left) and
// marks the day done, in one transaction; the day's lock orders two passes that both believe they
// hold the daily lease (one lost it mid-pass).
func (s *Store) PutInstallers(ctx context.Context, day string, rows map[InstallerKey]int64) error {
	return s.InTx(ctx, "kista/statistics/"+day, func(tx *Tx) error {
		if _, err := tx.exec(ctx, "DELETE FROM download_installers WHERE day = ?", day); err != nil {
			return err
		}
		for k, n := range rows {
			if _, err := tx.exec(ctx, "INSERT INTO download_installers (tenant_id, channel_id, name, ext_version, platform, day, installers) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?)", k.TenantID, k.ChannelID, k.Name, k.ExtVersion, k.Platform, day, n); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, "DELETE FROM statistics_days WHERE day = ?", day); err != nil {
			return err
		}
		_, err := tx.exec(ctx, "INSERT INTO statistics_days (day, done_at) VALUES (?, ?)", day, tx.s.d.timeArg(tx.Now()))
		return err
	})
}

// PruneStatistics deletes counts, installers and day marks before a day.
func (s *Store) PruneStatistics(ctx context.Context, before string) (int, error) {
	total := 0
	for _, t := range []string{"download_counts", "download_installers", "statistics_days"} {
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
	Name      string
	Names     []string // at most these extensions (a caller's scope); nil: any
}

// StatsRow is a download count with its installers, at the dimensions a query keeps (the others
// empty). Installers are summed over the kept rows (a principal installing on two days or two
// platforms counts twice).
type StatsRow struct {
	ChannelID, Name, ExtVersion, Platform, DuckDBVersion, Day string
	Count, Authenticated, Installers                          int64
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
	err := s.eachRow(ctx, "SELECT channel_id, name, ext_version, platform, duckdb_version, day, authenticated, count FROM download_counts WHERE "+where,
		args, func(sc func(...any) error) error {
			var r StatsRow
			var auth bool
			if err := sc(&r.ChannelID, &r.Name, &r.ExtVersion, &r.Platform, &r.DuckDBVersion, &r.Day, &auth, &r.Count); err != nil {
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
	err = s.eachRow(ctx, "SELECT channel_id, name, ext_version, platform, day, installers FROM download_installers WHERE "+where,
		args, func(sc func(...any) error) error {
			var r StatsRow
			if err := sc(&r.ChannelID, &r.Name, &r.ExtVersion, &r.Platform, &r.Day, &r.Installers); err != nil {
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
	ChannelID, Name, ExtVersion, Platform string
	Last7, Last30                         int64
	LastDay                               string
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
	err = s.eachRow(ctx, "SELECT channel_id, name, ext_version, platform, MAX(day) FROM download_counts WHERE "+where+
		" GROUP BY channel_id, name, ext_version, platform", args, func(sc func(...any) error) error {
		var k [4]string
		var last string
		if err := sc(&k[0], &k[1], &k[2], &k[3], &last); err != nil {
			return err
		}
		by[k] = &ReleaseDownloads{ChannelID: k[0], Name: k[1], ExtVersion: k[2], Platform: k[3], LastDay: last}
		order = append(order, k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	where, args = statsWhere(tenantID, from30, today, f)
	err = s.eachRow(ctx, "SELECT channel_id, name, ext_version, platform, day, SUM(count) FROM download_counts WHERE "+where+
		" GROUP BY channel_id, name, ext_version, platform, day", args, func(sc func(...any) error) error {
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

// InstallEvents calls fn with every install event of a tenant's day, in pages, oldest first (the
// index on tenant_id, kind, at).
func (s *Store) InstallEvents(ctx context.Context, tenantID, day string, fn func(Event) error) error {
	from, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return fmt.Errorf("%w: a day", ErrInvalid)
	}
	to := from.AddDate(0, 0, 1)
	var afterAt time.Time
	afterID := ""
	for {
		q := "SELECT " + eventCols + " FROM events WHERE tenant_id = ? AND kind = 'install' AND at >= ? AND at < ?"
		args := []any{tenantID, s.d.timeArg(from), s.d.timeArg(to)}
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
