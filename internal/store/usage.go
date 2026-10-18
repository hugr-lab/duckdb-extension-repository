package store

import (
	"cmp"
	"context"
	"fmt"
)

// UsageKey is what a usage count counts by (spec 0012 phase 1b): no principal, no address. Counter
// is "" for the event's own count; Status is the licence status the report gave.
type UsageKey struct {
	TenantID, ChannelID, Name, ExtVersion, Event, Counter, Status string
	Day                                                           string // YYYY-MM-DD, UTC
	Authenticated                                                 bool
}

// UsageUserKey is what a day's users of usage reports are counted by.
type UsageUserKey struct {
	TenantID, ChannelID, Name, ExtVersion, Day string
}

// AddUsageCounts adds usage counts to the stored ones, as AddDownloadCounts does, under the lock
// kista/usage_counts on SQL Server.
func (s *Store) AddUsageCounts(ctx context.Context, counts map[UsageKey]int64) (done []UsageKey, err error) {
	return addCounts(ctx, s, "kista/usage_counts", counts, func(a, b UsageKey) int {
		return cmp.Or(cmp.Compare(a.TenantID, b.TenantID), cmp.Compare(a.ChannelID, b.ChannelID), cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.ExtVersion, b.ExtVersion), cmp.Compare(a.Event, b.Event), cmp.Compare(a.Counter, b.Counter),
			cmp.Compare(a.Status, b.Status), cmp.Compare(a.Day, b.Day), cmp.Compare(boolInt(a.Authenticated), boolInt(b.Authenticated)))
	}, func(k UsageKey) string { return k.TenantID }, func(tx *Tx, k UsageKey, n int64) error {
		return tx.upsertCount(ctx, "usage_counts", "value",
			[]string{"tenant_id", "channel_id", "name", "ext_version", "event", "counter", "status", "day", "authenticated"},
			[]any{k.TenantID, k.ChannelID, k.Name, k.ExtVersion, k.Event, k.Counter, k.Status, k.Day, k.Authenticated}, n)
	})
}

// UsageRow is a usage count, or a day's users (Event ""), at the dimensions a query keeps.
type UsageRow struct {
	ChannelID, Name, ExtVersion, Event, Counter, Status, Day string
	Value, Authenticated, Users                              int64
}

// UsageRows reads a tenant's usage counts and users within a filter, keeping the rows keep takes (a
// caller's scope); more than limit rows kept is ErrInvalid (the caller narrows the range).
func (s *Store) UsageRows(ctx context.Context, tenantID string, f StatsFilter, limit int, keep func(channelID, name string) bool) ([]UsageRow, error) {
	where, args := statsWhere(tenantID, f.From, f.To, f)
	var out []UsageRow
	add := func(r UsageRow) error {
		if keep != nil && !keep(r.ChannelID, r.Name) {
			return nil
		}
		out = append(out, r)
		if len(out) > limit {
			return fmt.Errorf("%w: more than %d rows; narrow the range", ErrInvalid, limit)
		}
		return nil
	}
	err := s.eachRow(ctx, "SELECT channel_id, name, ext_version, event, counter, status, day, authenticated, value FROM usage_counts WHERE "+where,
		args, func(sc func(...any) error) error {
			var r UsageRow
			var auth bool
			if err := sc(&r.ChannelID, &r.Name, &r.ExtVersion, &r.Event, &r.Counter, &r.Status, &r.Day, &auth, &r.Value); err != nil {
				return err
			}
			if auth {
				r.Authenticated = r.Value
			}
			return add(r)
		})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, "SELECT channel_id, name, ext_version, day, users FROM usage_users WHERE "+where,
		args, func(sc func(...any) error) error {
			var r UsageRow
			if err := sc(&r.ChannelID, &r.Name, &r.ExtVersion, &r.Day, &r.Users); err != nil {
				return err
			}
			return add(r)
		})
	return out, err
}
