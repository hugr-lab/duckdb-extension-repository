package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Spec 0031: a name's source in a channel.

// Sources of releases.
const (
	// SourceTenant: an administrator's add, or a publication or promotion by a tenant principal.
	SourceTenant = "tenant"
	// SourceUpstreamPrefix and SourcePublisherPrefix take the upstream's or the publisher's id; an
	// upstream source without an id is a removed upstream's.
	SourceUpstreamPrefix  = "upstream:"
	SourcePublisherPrefix = "publisher:"
)

var sourceID = regexp.MustCompile(`^[A-Za-z0-9-]{0,36}$`)

// ValidSource says whether s is a source.
func ValidSource(s string) bool {
	if s == SourceTenant {
		return true
	}
	if id, ok := strings.CutPrefix(s, SourceUpstreamPrefix); ok {
		return sourceID.MatchString(id)
	}
	if id, ok := strings.CutPrefix(s, SourcePublisherPrefix); ok {
		return id != "" && sourceID.MatchString(id)
	}
	return false
}

// UpstreamSource is an upstream's source.
func UpstreamSource(upstreamID string) string { return SourceUpstreamPrefix + upstreamID }

// PublisherSource is a publisher's source.
func PublisherSource(publisherID string) string { return SourcePublisherPrefix + publisherID }

// ChannelSource is the source a channel serves a name from.
type ChannelSource struct {
	ChannelID, Name, Source string
	SetBy                   string
	SetAt                   time.Time
}

// ChannelSourceOf is a channel's source for a name; ok is false when it has none.
func (t *Tx) ChannelSourceOf(ctx context.Context, channelID, name string) (ChannelSource, bool, error) {
	c := ChannelSource{ChannelID: channelID, Name: name}
	err := t.queryRow(ctx, `SELECT source, set_by, set_at FROM channel_sources WHERE channel_id = ? AND name = ?`, channelID, name).
		Scan(&c.Source, &c.SetBy, scanTime{&c.SetAt})
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelSource{}, false, nil
	}
	return c, err == nil, err
}

// SetChannelSource sets a channel's source for a name, and returns the source it had ("": none).
// The caller holds the channel's lock.
func (t *Tx) SetChannelSource(ctx context.Context, channelID, name, source, actor string) (string, error) {
	if !ValidSource(source) {
		return "", ErrInvalid
	}
	before, had, err := t.ChannelSourceOf(ctx, channelID, name)
	if err != nil {
		return "", err
	}
	now := t.s.d.timeArg(t.Now())
	if had {
		_, err = t.exec(ctx, `UPDATE channel_sources SET source = ?, set_by = ?, set_at = ? WHERE channel_id = ? AND name = ?`,
			source, actor, now, channelID, name)
	} else {
		_, err = t.exec(ctx, `INSERT INTO channel_sources (channel_id, name, source, set_by, set_at) VALUES (?, ?, ?, ?, ?)`,
			channelID, name, source, actor, now)
	}
	return before.Source, t.s.mapErr(err, "channel source")
}

// ClearChannelSource removes a channel's source for a name, and returns the one it had ("": none).
func (t *Tx) ClearChannelSource(ctx context.Context, channelID, name string) (string, error) {
	before, had, err := t.ChannelSourceOf(ctx, channelID, name)
	if err != nil || !had {
		return "", err
	}
	_, err = t.exec(ctx, `DELETE FROM channel_sources WHERE channel_id = ? AND name = ?`, channelID, name)
	return before.Source, err
}

// UpstreamRemoved reports whether a source is a removed upstream's (spec 0031): an upstream source
// whose upstream is gone, or the migration's upstream source without an id.
func (t *Tx) UpstreamRemoved(ctx context.Context, source string) (bool, error) {
	id, ok := strings.CutPrefix(source, SourceUpstreamPrefix)
	if !ok {
		return false, nil
	}
	if id == "" {
		return true, nil
	}
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM upstreams WHERE id = ?`, id).Scan(&n)
	return n == 0, err
}

// SourceTakes reports whether a channel takes a name from a source (spec 0031): it has no source
// for the name, the source is it, or the name's source is a removed upstream's and the source is
// an upstream (its successor).
func (s *Store) SourceTakes(ctx context.Context, channelID, name, source string) (sel ChannelSource, takes bool, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		var had bool
		if sel, had, err = tx.ChannelSourceOf(ctx, channelID, name); err != nil || !had || sel.Source == source {
			takes = err == nil
			return err
		}
		if !strings.HasPrefix(source, SourceUpstreamPrefix) {
			return nil
		}
		takes, err = tx.UpstreamRemoved(ctx, sel.Source)
		return err
	})
	return sel, takes, err
}

// ChannelSources are a channel's sources, by name.
func (s *Store) ChannelSources(ctx context.Context, channelID string) (map[string]ChannelSource, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT name, source, set_by, set_at FROM channel_sources WHERE channel_id = ?`), channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ChannelSource{}
	for rows.Next() {
		c := ChannelSource{ChannelID: channelID}
		if err := rows.Scan(&c.Name, &c.Source, &c.SetBy, scanTime{&c.SetAt}); err != nil {
			return nil, err
		}
		out[c.Name] = c
	}
	return out, rows.Err()
}

// ChannelReleaseSources are the sources that have releases in a channel (any state), by name, each
// with its count of releases.
func (s *Store) ChannelReleaseSources(ctx context.Context, channelID string) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT name, source, COUNT(*) FROM releases WHERE channel_id = ? GROUP BY name, source`),
		channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var name, src string
		var n int
		if err := rows.Scan(&name, &src, &n); err != nil {
			return nil, err
		}
		if out[name] == nil {
			out[name] = map[string]int{}
		}
		out[name][src] = n
	}
	return out, rows.Err()
}

// SourceLabels name a tenant's sources as answers show them (spec 0031): upstream:<name>,
// publisher:<name>, tenant; a source missing from the map is a removed upstream's or publisher's.
func (s *Store) SourceLabels(ctx context.Context, tenantID string) (map[string]string, error) {
	out := map[string]string{SourceTenant: SourceTenant}
	for _, q := range []struct{ table, prefix string }{{"upstreams", SourceUpstreamPrefix}, {"publishers", SourcePublisherPrefix}} {
		rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT id, name FROM `+q.table+` WHERE tenant_id = ?`), tenantID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			out[q.prefix+id] = q.prefix + name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Label is a source as answers show it, by labels (SourceLabels): a removed upstream's or
// publisher's is "upstream:(removed)" or "publisher:(removed)".
func Label(labels map[string]string, source string) string {
	if l, ok := labels[source]; ok {
		return l
	}
	if strings.HasPrefix(source, SourceUpstreamPrefix) {
		return SourceUpstreamPrefix + "(removed)"
	}
	if strings.HasPrefix(source, SourcePublisherPrefix) {
		return SourcePublisherPrefix + "(removed)"
	}
	return source
}

// SourceLabel is one source as answers show it (Label).
func (t *Tx) SourceLabel(ctx context.Context, source string) (string, error) {
	q, prefix := `SELECT name FROM upstreams WHERE id = ?`, SourceUpstreamPrefix
	id, ok := strings.CutPrefix(source, SourceUpstreamPrefix)
	if !ok {
		if id, ok = strings.CutPrefix(source, SourcePublisherPrefix); !ok {
			return source, nil
		}
		q, prefix = `SELECT name FROM publishers WHERE id = ?`, SourcePublisherPrefix
	}
	var name string
	err := t.queryRow(ctx, q, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return prefix + "(removed)", nil
	}
	return prefix + name, err
}

// SourceLabel is one source as answers show it, outside a transaction.
func (s *Store) SourceLabel(ctx context.Context, source string) (label string, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		label, err = tx.SourceLabel(ctx, source)
		return err
	})
	return label, err
}

// ErrNoSource: the source named is not one of the tenant's (an upstream, a publisher, the tenant).
var ErrNoSource = errors.New("store: no such source for the channel")

// ResolveSource reads a source as a caller names it (tenant, upstream:<name>, publisher:<name>)
// into the stored form: an upstream or a publisher of the tenant (an upstream of another channel
// is the source of the mirrored releases promoted from it).
func (s *Store) ResolveSource(ctx context.Context, tenantID, label string) (string, error) {
	if label == SourceTenant {
		return SourceTenant, nil
	}
	var q string
	var args []any
	prefix := SourceUpstreamPrefix
	if name, ok := strings.CutPrefix(label, SourceUpstreamPrefix); ok {
		q, args = `SELECT id FROM upstreams WHERE tenant_id = ? AND name = ?`, []any{tenantID, name}
	} else if name, ok := strings.CutPrefix(label, SourcePublisherPrefix); ok {
		q, args, prefix = `SELECT id FROM publishers WHERE tenant_id = ? AND name = ?`, []any{tenantID, name}, SourcePublisherPrefix
	} else {
		return "", ErrNoSource
	}
	var id string
	err := s.db.QueryRowContext(ctx, s.d.rebind(q), args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSource
	}
	if err != nil {
		return "", err
	}
	return prefix + id, nil
}
