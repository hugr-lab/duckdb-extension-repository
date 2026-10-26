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

// Rule modes (spec 0033), an administrator's: auto places each new version of the rule's source the
// channel serves; manual places none, but completes the versions the channel holds (their builds
// that arrive later). A rule an arrival wrote has none (RuleNone): the channel serves what lands in
// it, and the placement pass leaves it alone. A follow writes RuleFollow: auto while the channel
// follows the source, manual once it no longer does.
const (
	RuleNone   = ""
	RuleAuto   = "auto"
	RuleManual = "manual"
	RuleFollow = "follow"
)

// ChannelSource is the source a channel serves a name from, and its rule (spec 0033): the mode, and
// the placement pass's last look (CheckedAt, zero for never) and outcome.
type ChannelSource struct {
	ChannelID, Name, Source string
	SetBy                   string
	SetAt                   time.Time
	Mode                    string
	CheckedAt               time.Time
	Outcome                 string
	OutcomeAt               time.Time
}

const channelSourceCols = "name, source, set_by, set_at, mode, checked_at, outcome, outcome_at"

func scanChannelSource(r interface{ Scan(...any) error }, c *ChannelSource) error {
	var outcome sql.NullString
	err := r.Scan(&c.Name, &c.Source, &c.SetBy, scanTime{&c.SetAt}, &c.Mode, scanTime{&c.CheckedAt}, &outcome, scanTime{&c.OutcomeAt})
	c.Outcome = outcome.String
	return err
}

// ChannelSourceOf is a channel's source for a name; ok is false when it has none.
func (t *Tx) ChannelSourceOf(ctx context.Context, channelID, name string) (ChannelSource, bool, error) {
	c := ChannelSource{ChannelID: channelID}
	err := scanChannelSource(t.queryRow(ctx, `SELECT `+channelSourceCols+` FROM channel_sources WHERE channel_id = ? AND name = ?`,
		channelID, name), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelSource{}, false, nil
	}
	return c, err == nil, err
}

// SetChannelSource sets a channel's source for a name, keeping its mode (a new rule has none), and
// returns the source it had ("": none). The caller holds the channel's lock.
func (t *Tx) SetChannelSource(ctx context.Context, channelID, name, source, actor string) (string, error) {
	before, err := t.SetChannelRule(ctx, channelID, name, source, "", actor)
	return before.Source, err
}

// SetChannelRule sets a channel's rule for a name: its source and mode ("": the rule's mode, none
// for a new one), and returns the rule it had (Source "": none). A changed source or mode is looked
// at again by the placement pass; an unchanged rule is left as it is. The caller holds the channel's
// lock.
func (t *Tx) SetChannelRule(ctx context.Context, channelID, name, source, mode, actor string) (ChannelSource, error) {
	if !ValidSource(source) || mode != RuleNone && mode != RuleAuto && mode != RuleManual && mode != RuleFollow {
		return ChannelSource{}, ErrInvalid
	}
	before, had, err := t.ChannelSourceOf(ctx, channelID, name)
	if err != nil {
		return ChannelSource{}, err
	}
	if mode == RuleNone && had {
		mode = before.Mode
	}
	if had && before.Source == source && before.Mode == mode {
		return before, nil
	}
	now := t.s.d.timeArg(t.Now())
	if had {
		_, err = t.exec(ctx, `UPDATE channel_sources SET source = ?, mode = ?, set_by = ?, set_at = ?, checked_at = NULL, outcome = NULL,
outcome_at = NULL, due_at = NULL WHERE channel_id = ? AND name = ?`, source, mode, actor, now, channelID, name)
	} else {
		_, err = t.exec(ctx, `INSERT INTO channel_sources (channel_id, name, source, mode, set_by, set_at) VALUES (?, ?, ?, ?, ?, ?)`,
			channelID, name, source, mode, actor, now)
	}
	if err == nil && (!had || before.Source != source) { // spec 0035 phase 2: a rule selecting an entry
		t.warnRule(channelID, name, source, actor)
	}
	return before, t.s.mapErr(err, "channel source")
}

// RecheckRules makes the placement pass look at every rule of a channel again (a DuckDB version
// added: the builds for it are placed by the rules; a channel's hold changed, spec 0033 phase A2).
// The rule is due now (due_at), which also tells a look begun before it not to record itself
// (SetRuleChecked): the next look sees the change.
func (t *Tx) RecheckRules(ctx context.Context, channelID string) error {
	_, err := t.exec(ctx, "UPDATE channel_sources SET checked_at = NULL, due_at = ? WHERE channel_id = ?", t.s.d.timeArg(t.Now()), channelID)
	return err
}

// RecheckRule makes the placement pass look at a channel's rule for a name again, as RecheckRules
// does every rule of the channel (spec 0033 phase A2: a mirror's entry found again and left to the
// pass in its home channel, which a look begun before does not record itself over).
func (t *Tx) RecheckRule(ctx context.Context, channelID, name string) error {
	_, err := t.exec(ctx, "UPDATE channel_sources SET checked_at = NULL, due_at = ? WHERE channel_id = ? AND name = ?",
		t.s.d.timeArg(t.Now()), channelID, name)
	return err
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
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT `+channelSourceCols+` FROM channel_sources WHERE channel_id = ?`), channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ChannelSource{}
	for rows.Next() {
		c := ChannelSource{ChannelID: channelID}
		if err := scanChannelSource(rows, &c); err != nil {
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

// ValidLicenseEntry says whether s is a licence's entry (spec 0035): an upstream's or a publisher's
// source (upstream:<id>, publisher:<id>), the only ones whose builds record licence keys.
func ValidLicenseEntry(s string) bool {
	_, id, ok := splitKeySource(s)
	return ok && id != "" && sourceID.MatchString(id)
}

// LicenseEntrySource reads a source as a tenant's licence entry (spec 0035): live reports that the
// tenant has the upstream or publisher, and upstreamKind is a live upstream's kind ("" for a
// publisher).
func (t *Tx) LicenseEntrySource(ctx context.Context, tenantID, source string) (live bool, upstreamKind string, err error) {
	kind, id, ok := splitKeySource(source)
	if !ok {
		return false, "", nil
	}
	q := "SELECT kind FROM upstreams WHERE id = ? AND tenant_id = ?"
	if kind == KeySourcePublisher {
		q = "SELECT '' FROM publishers WHERE id = ? AND tenant_id = ?"
	}
	err = t.queryRow(ctx, q, id, tenantID).Scan(&upstreamKind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return err == nil, upstreamKind, err
}

// LicenseEntrySource reads a source as a tenant's licence entry outside a transaction.
func (s *Store) LicenseEntrySource(ctx context.Context, tenantID, source string) (live bool, upstreamKind string, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		live, upstreamKind, err = tx.LicenseEntrySource(ctx, tenantID, source)
		return err
	})
	return live, upstreamKind, err
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

// SourceType is the package type a source's names are of (spec 0034): an upstream's home channel's;
// "" for a source of any type (the tenant, a publisher) or an upstream removed.
func (s *Store) SourceType(ctx context.Context, source string) (string, error) {
	id, ok := strings.CutPrefix(source, SourceUpstreamPrefix)
	if !ok {
		return "", nil
	}
	var t string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT c.package_type FROM upstreams u JOIN channels c ON c.id = u.channel_id WHERE u.id = ?"),
		id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return t, err
}

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

// TakeReleases gives a removed upstream's releases of a name in a channel to the upstream that
// succeeds it there (spec 0031): each moves, with its state, visibility and sequence number, to the
// successor's build of its body (found, or made as a copy of the removed one's), and the removed
// upstream's purged slots of the name become the successor's, so an administrator's yank, purge or
// choice survives an upstream removed and added again (spec 0033: slots are per source). A slot the
// successor holds already is left as it is. Call it under the channel lock. It returns how many
// releases moved.
func (t *Tx) TakeReleases(ctx context.Context, channelID, name, from, to string) (int, error) {
	type moving struct{ id, buildID string }
	var ms []moving
	rows, err := t.query(ctx, `SELECT r.id, r.build_id FROM releases r WHERE r.channel_id = ? AND r.name = ? AND r.source = ?
AND NOT EXISTS (SELECT 1 FROM releases o WHERE o.channel_id = r.channel_id AND o.source = ? AND o.name = r.name
	AND o.package_version = r.package_version AND o.platform = r.platform AND o.slot = r.slot)`, channelID, name, from, to)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var m moving
		if err := rows.Scan(&m.id, &m.buildID); err != nil {
			rows.Close()
			return 0, err
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range ms {
		b, err := t.GetBuild(ctx, m.buildID)
		if err != nil {
			return 0, err
		}
		// a copy keeps the original's arrival, hold and revoked marker: the successor's hold is for
		// what arrives
		b.ID, b.Source, b.Copied = "", to, true
		if err := t.FindOrInsertBuild(ctx, &b); errors.Is(err, ErrCatalogueSlot) {
			// the successor holds the version with another body: the copy is a legacy duplicate
			b.ID, b.Legacy = "", true
			err = t.FindOrInsertBuild(ctx, &b)
			if err != nil {
				return 0, err
			}
		} else if err != nil {
			return 0, err
		}
		if _, err := t.exec(ctx, "UPDATE releases SET source = ?, build_id = ?, version = version + 1 WHERE id = ?", to, b.ID, m.id); err != nil {
			return 0, err
		}
		t.warnRelease(m.id, "upstream.release", "") // spec 0035 phase 2: the successor's entry may hold a licence
	}
	_, err = t.exec(ctx, `UPDATE purged_slots SET source = ? WHERE channel_id = ? AND name = ? AND source = ?
AND NOT EXISTS (SELECT 1 FROM purged_slots o WHERE o.channel_id = purged_slots.channel_id AND o.source = ? AND o.name = purged_slots.name
	AND o.package_version = purged_slots.package_version AND o.platform = purged_slots.platform AND o.slot = purged_slots.slot)`,
		to, channelID, name, from, to)
	return len(ms), err
}
