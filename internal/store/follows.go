package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Spec 0033: a channel following a whole source.

// Follow is a channel following a source.
type Follow struct {
	ChannelID, Source, SetBy string
	SetAt, CheckedAt         time.Time
}

// SetFollow makes a channel follow a source; it reports whether the follow is new.
func (t *Tx) SetFollow(ctx context.Context, channelID, source, actor string) (bool, error) {
	if !ValidSource(source) {
		return false, ErrInvalid
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM channel_follows WHERE channel_id = ? AND source = ?", channelID, source).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	_, err := t.exec(ctx, "INSERT INTO channel_follows (channel_id, source, set_by, set_at) VALUES (?, ?, ?, ?)",
		channelID, source, actor, t.s.d.timeArg(t.Now()))
	return err == nil, t.s.mapErr(err, "follow")
}

// RemoveFollow stops a channel following a source, with its skipped names; the rules it wrote stay
// (their mode follow then acts as manual). ErrNotFound when it did not follow it.
func (t *Tx) RemoveFollow(ctx context.Context, channelID, source string) error {
	res, err := t.exec(ctx, "DELETE FROM channel_follows WHERE channel_id = ? AND source = ?", channelID, source)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = t.exec(ctx, "DELETE FROM follow_skips WHERE channel_id = ? AND source = ?", channelID, source)
	return err
}

// Follows lists a channel's follows.
func (s *Store) Follows(ctx context.Context, channelID string) ([]Follow, error) {
	var out []Follow
	err := s.eachRow(ctx, "SELECT channel_id, source, set_by, set_at, checked_at FROM channel_follows WHERE channel_id = ? ORDER BY source",
		[]any{channelID}, func(sc func(...any) error) error {
			var f Follow
			if err := sc(&f.ChannelID, &f.Source, &f.SetBy, scanTime{&f.SetAt}, scanTime{&f.CheckedAt}); err != nil {
				return err
			}
			out = append(out, f)
			return nil
		})
	return out, err
}

// Following reports whether a channel follows a source.
func (s *Store) Following(ctx context.Context, channelID, source string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM channel_follows WHERE channel_id = ? AND source = ?"), channelID, source).Scan(&n)
	return n > 0, err
}

// FollowSkips are the names a follow skips, with the reason, by name.
func (s *Store) FollowSkips(ctx context.Context, channelID, source string) (map[string]string, error) {
	out := map[string]string{}
	err := s.eachRow(ctx, "SELECT name, reason FROM follow_skips WHERE channel_id = ? AND source = ?", []any{channelID, source},
		func(sc func(...any) error) error {
			var n, r string
			if err := sc(&n, &r); err != nil {
				return err
			}
			out[n] = r
			return nil
		})
	return out, err
}

// DueFollow is a follow the placement pass looks at, with its tenant.
type DueFollow struct {
	TenantID, ChannelID, Source string
}

// DueFollows lists the follows of signed channels of active tenants the placement pass should look
// at: every one (sweep), or those whose source has a build made since the pass last looked.
func (s *Store) DueFollows(ctx context.Context, sweep bool) ([]DueFollow, error) {
	q := `SELECT c.tenant_id, f.channel_id, f.source FROM channel_follows f JOIN channels c ON c.id = f.channel_id
JOIN tenants t ON t.id = c.tenant_id WHERE c.kind = 'signed' AND t.state = 'active' AND t.deleting_at IS NULL`
	if !sweep {
		q += ` AND EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = c.tenant_id AND b.source = f.source
	AND (f.checked_at IS NULL OR b.created_at > f.checked_at))`
	}
	var out []DueFollow
	err := s.eachRow(ctx, q+" ORDER BY c.tenant_id, f.channel_id, f.source", nil, func(sc func(...any) error) error {
		var f DueFollow
		if err := sc(&f.TenantID, &f.ChannelID, &f.Source); err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

// FollowLook records the placement pass's look at a follow, begun at start: when (builds made after
// it are looked at next time) and the names it skipped, with the reason. A follow set again since
// the look began is left alone.
func (s *Store) FollowLook(ctx context.Context, f DueFollow, start, at time.Time, skips map[string]string) error {
	return s.InTx(ctx, "", func(tx *Tx) error {
		res, err := tx.exec(ctx, "UPDATE channel_follows SET checked_at = ? WHERE channel_id = ? AND source = ? AND set_at <= ?",
			tx.s.d.timeArg(at), f.ChannelID, f.Source, tx.s.d.timeArg(start))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.exec(ctx, "DELETE FROM follow_skips WHERE channel_id = ? AND source = ?", f.ChannelID, f.Source); err != nil {
			return err
		}
		for name, reason := range skips {
			if len(reason) > 200 {
				reason = strings.ToValidUTF8(reason[:200], "")
			}
			if _, err := tx.exec(ctx, "INSERT INTO follow_skips (channel_id, source, name, reason) VALUES (?, ?, ?, ?)",
				f.ChannelID, f.Source, name, reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// SourceNames lists the names a tenant's source has builds of.
func (s *Store) SourceNames(ctx context.Context, tenantID, source string) ([]string, error) {
	var out []string
	err := s.eachRow(ctx, "SELECT DISTINCT name FROM builds WHERE tenant_id = ? AND source = ? ORDER BY name", []any{tenantID, source},
		func(sc func(...any) error) error {
			var n string
			if err := sc(&n); err != nil {
				return err
			}
			out = append(out, n)
			return nil
		})
	return out, err
}

// NameUsedElsewhere reports why a tenant uses a name from another source than source, "" when it
// does not: another of its sources has builds of it, or a rule of one of its channels names another
// source (spec 0033: a follow does not take such a name).
func (s *Store) NameUsedElsewhere(ctx context.Context, tenantID, name, source string) (string, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*) FROM builds WHERE tenant_id = ? AND name = ? AND source <> ?`),
		tenantID, name, source).Scan(&n); err != nil || n > 0 {
		if n > 0 {
			return "another source of the tenant has builds of it", nil
		}
		return "", err
	}
	if err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*) FROM channel_sources cs JOIN channels c ON c.id = cs.channel_id
WHERE c.tenant_id = ? AND cs.name = ? AND cs.source <> ?`), tenantID, name, source).Scan(&n); err != nil || n > 0 {
		if n > 0 {
			return "a channel of the tenant serves it from another source", nil
		}
		return "", err
	}
	return "", nil
}

// UpstreamKindOf is an upstream source's kind ("" for any other source, or a removed upstream's).
func (s *Store) UpstreamKindOf(ctx context.Context, source string) (string, error) {
	id, ok := strings.CutPrefix(source, SourceUpstreamPrefix)
	if !ok {
		return "", nil
	}
	var kind string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT kind FROM upstreams WHERE id = ?"), id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return kind, err
}

// NameReservedFor is how a name is reserved, for a follow's limits: by the server (core or
// community), or in the tenant ("tenant"); "" when it is not.
func (s *Store) NameReservedFor(ctx context.Context, tenantID, name string) (string, error) {
	var out string
	err := s.InTx(ctx, "", func(tx *Tx) error {
		k, err := tx.ReservedKind(ctx, name)
		if err != nil || k != "" {
			out = k
			return err
		}
		held, err := tx.TenantReserves(ctx, tenantID, name)
		if held {
			out = "tenant"
		}
		return err
	})
	return out, err
}
