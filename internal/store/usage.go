package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
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

// UsageRow is a usage count, or a day's users (Event ""), at the dimensions a query keeps; received
// aggregates (spec 0012 phase 1c) come summed over their senders, with Status StatusReceived.
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
	if err != nil {
		return nil, err
	}
	err = s.receivedRows(ctx, where, args, add)
	return out, err
}

// ReceivedRow is one count of a downstream kista's daily aggregate (spec 0012 phase 1c): an event's
// count (Counter "") or a counter's value, or, with Event "", the version's users.
type ReceivedRow struct {
	Name, ExtVersion, Event, Counter string
	Users, Value                     int64
}

// PutReceived stores part of a sender's day in a channel, in one transaction: the first part
// clears what the sender had for the day; each part replaces the (name, version) pairs it carries,
// so a resent day, whole or in part, never adds twice.
func (s *Store) PutReceived(ctx context.Context, tenantID, channelID, sender, day string, first bool, rows []ReceivedRow) error {
	return s.InTx(ctx, "kista/usage_received/"+tenantID+"/"+sender, func(tx *Tx) error {
		closed, err := tx.closedTenants(ctx, []string{tenantID})
		if err != nil || closed[tenantID] {
			return err
		}
		key := "tenant_id = ? AND channel_id = ? AND sender = ? AND day = ?"
		args := []any{tenantID, channelID, sender, day}
		if first {
			if _, err := tx.exec(ctx, "DELETE FROM usage_received WHERE "+key, args...); err != nil {
				return err
			}
		} else {
			done := map[[2]string]bool{}
			for _, r := range rows {
				p := [2]string{r.Name, r.ExtVersion}
				if done[p] {
					continue
				}
				done[p] = true
				if _, err := tx.exec(ctx, "DELETE FROM usage_received WHERE "+key+" AND name = ? AND ext_version = ?",
					append(args, r.Name, r.ExtVersion)...); err != nil {
					return err
				}
			}
		}
		for len(rows) > 0 { // 100 rows a statement: 1,000 parameters, within SQL Server's 2,100
			chunk := rows[:min(len(rows), 100)]
			rows = rows[len(chunk):]
			args := make([]any, 0, 10*len(chunk))
			for _, r := range chunk {
				args = append(args, tenantID, channelID, sender, day, r.Name, r.ExtVersion, r.Event, r.Counter, r.Users, r.Value)
			}
			if _, err := tx.exec(ctx, "INSERT INTO usage_received (tenant_id, channel_id, sender, day, name, ext_version, event, counter, "+
				"users, value) VALUES "+strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?), ", len(chunk)), ", "), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// receivedRows adds a tenant's received aggregates to UsageRows' out, summed over their senders
// (saturating at MaxCount, as stored counts do): counts with Status StatusReceived (a downstream's,
// whose statuses are not forwarded), users with Event "".
func (s *Store) receivedRows(ctx context.Context, where string, args []any, add func(UsageRow) error) error {
	type key struct{ channel, name, version, event, counter, day string }
	sums := map[key][2]int64{}
	var order []key
	err := s.eachRow(ctx, "SELECT channel_id, name, ext_version, event, counter, day, users, value FROM usage_received WHERE "+where,
		args, func(sc func(...any) error) error {
			var k key
			var users, value int64
			if err := sc(&k.channel, &k.name, &k.version, &k.event, &k.counter, &k.day, &users, &value); err != nil {
				return err
			}
			v, ok := sums[k]
			if !ok {
				order = append(order, k)
			}
			sums[k] = [2]int64{saturate(v[0], users), saturate(v[1], value)}
			return nil
		})
	if err != nil {
		return err
	}
	for _, k := range order {
		r := UsageRow{ChannelID: k.channel, Name: k.name, ExtVersion: k.version, Event: k.event, Counter: k.counter, Day: k.day}
		if k.event == "" {
			r.Users = sums[k][0]
		} else {
			r.Status, r.Value = StatusReceived, sums[k][1]
		}
		if err := add(r); err != nil {
			return err
		}
	}
	return nil
}

// saturate adds two counts within 0..MaxCount, stopping at MaxCount.
func saturate(a, b int64) int64 {
	if a > MaxCount-b {
		return MaxCount
	}
	return a + b
}

// StatusReceived is the status of received counts in UsageRows.
const StatusReceived = "received"

// UpstreamVersions are the (name, version) pairs an upstream brought into its channel since it was
// added, each with the channels of the tenant that hold a release of such a build (spec 0012: its
// usage there is forwarded to it; a promotion releases the same build elsewhere; the same version
// from another source is not its).
func (s *Store) UpstreamVersions(ctx context.Context, u Upstream) (map[[2]string]map[string]bool, error) {
	builds := map[string][2]string{}
	err := s.eachRow(ctx, "SELECT build_id, name, ext_version, provenance FROM releases WHERE channel_id = ? AND origin = ? AND created_at >= ?",
		[]any{u.ChannelID, OriginUpstream, s.d.timeArg(u.CreatedAt)}, func(sc func(...any) error) error {
			var id, name, version string
			var prov sql.NullString
			if err := sc(&id, &name, &version, &prov); err != nil {
				return err
			}
			var p struct {
				Upstream string `json:"upstream"`
			}
			if json.Unmarshal([]byte(prov.String), &p) == nil && p.Upstream == u.Name {
				builds[id] = [2]string{name, version}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := map[[2]string]map[string]bool{}
	ids := make([]string, 0, len(builds))
	for id := range builds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		args := []any{u.TenantID}
		for _, id := range chunk {
			args = append(args, id)
		}
		err := s.eachRow(ctx, "SELECT r.build_id, r.channel_id FROM releases r JOIN channels c ON c.id = r.channel_id WHERE c.tenant_id = ? AND r.build_id IN ("+
			strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")+")", args, func(sc func(...any) error) error {
			var id, ch string
			if err := sc(&id, &ch); err != nil {
				return err
			}
			p := builds[id]
			if out[p] == nil {
				out[p] = map[string]bool{}
			}
			out[p][ch] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

const dueUsageForwards = `SELECT u.id FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
LEFT JOIN usage_forwards f ON f.upstream_id = u.id
WHERE u.kind = 'repository' AND u.mode <> 'bundle' AND u.state = 'active' AND u.credential IS NOT NULL AND u.usage = 'forward'
AND t.state = 'active' AND t.deleting_at IS NULL
AND (f.last_run_at IS NULL OR f.last_run_at <= ?)`

// DueUsageForwards lists the upstreams whose daily usage forwarding is due: a repository upstream
// with a credential, active, set to forward, of an active tenant, last forwarded before since (or
// never).
func (s *Store) DueUsageForwards(ctx context.Context, since time.Time) ([]string, error) {
	var out []string
	err := s.eachRow(ctx, dueUsageForwards+" ORDER BY u.id", []any{s.d.timeArg(since)}, func(sc func(...any) error) error {
		var id string
		if err := sc(&id); err != nil {
			return err
		}
		out = append(out, id)
		return nil
	})
	return out, err
}

// UsageForwardDue is DueUsageForwards' rule for one upstream, checked again under its lease.
func (s *Store) UsageForwardDue(ctx context.Context, upstreamID string, since time.Time) (bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.d.rebind(dueUsageForwards+" AND u.id = ?"), s.d.timeArg(since), upstreamID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// GetUsageForward reads an upstream's last forwarding run (zero time and "" for none).
func (s *Store) GetUsageForward(ctx context.Context, upstreamID string) (time.Time, string, error) {
	var at time.Time
	var res string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT last_run_at, last_result FROM usage_forwards WHERE upstream_id = ?"), upstreamID).
		Scan(scanTime{&at}, &res)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", nil
	}
	return at, res, err
}

// FinishUsageForward records a forwarding run.
func (t *Tx) FinishUsageForward(ctx context.Context, tenantID, upstreamID, result string) error {
	now := t.s.d.timeArg(t.Now())
	res, err := t.exec(ctx, "UPDATE usage_forwards SET last_run_at = ?, last_result = ? WHERE upstream_id = ?", now, result, upstreamID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO usage_forwards (upstream_id, tenant_id, last_run_at, last_result) VALUES (?, ?, ?, ?)",
		upstreamID, tenantID, now, result)
	return t.s.mapErr(err, "usage forward")
}

// UsageForwarder is an upstream that forwards usage with a credential.
type UsageForwarder struct{ Tenant, Upstream, Credential string }

// UsageForwarders lists the upstreams of live tenants that forward usage with a credential.
func (s *Store) UsageForwarders(ctx context.Context) ([]UsageForwarder, error) {
	var out []UsageForwarder
	err := s.eachRow(ctx, `SELECT t.name, u.name, u.credential FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
WHERE u.kind = 'repository' AND u.mode <> 'bundle' AND u.credential IS NOT NULL AND u.usage = 'forward' AND t.deleting_at IS NULL
ORDER BY t.name, u.name`, nil, func(sc func(...any) error) error {
		var f UsageForwarder
		if err := sc(&f.Tenant, &f.Upstream, &f.Credential); err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
	return out, err
}
