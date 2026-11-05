package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Spec 0042: version retention in channels. A tenant's default rule and a channel's own keep the
// newest N versions of each name per DuckDB version the channel serves; the retention pass retires
// the others and purges them after a grace; pins keep a version whatever the rule.

// RetentionRule is a rule as stored: a field nil is not set (a channel's: the tenant's applies); a
// channel's Keep of 0 is no retention there, whatever the tenant's.
type RetentionRule struct {
	Keep  *int
	Grace *time.Duration
}

// Equal reports whether two rules store the same values.
func (r RetentionRule) Equal(o RetentionRule) bool {
	return (r.Keep == nil) == (o.Keep == nil) && (r.Keep == nil || *r.Keep == *o.Keep) &&
		(r.Grace == nil) == (o.Grace == nil) && (r.Grace == nil || *r.Grace == *o.Grace)
}

// MaxRetentionKeep is the most versions a rule keeps: what its column holds (a format bound).
const MaxRetentionKeep = math.MaxInt32

// MaxRetentionGrace is the longest grace a tenant or a channel may set (limits.ceilings.retention_grace,
// SetCeilings).
var MaxRetentionGrace = 90 * 24 * time.Hour

// MaxPinReason is the most bytes of a pin's reason.
const MaxPinReason = 400

// ValidRetention checks a rule's fields: keep from 1 (0 too at a channel: none there) to
// MaxRetentionKeep, a grace of whole seconds, at least 0. The grace's ceiling is checked by the caller
// where it changes (a stored grace above a lowered ceiling is kept, and capped where it is in force).
func ValidRetention(r RetentionRule, channel bool) error {
	if r.Keep != nil && (*r.Keep < 1 && (!channel || *r.Keep != 0) || *r.Keep > MaxRetentionKeep) {
		return fmt.Errorf("%w: keep is a whole number from 1 to %d", ErrInvalid, MaxRetentionKeep)
	}
	if r.Grace != nil && (*r.Grace < 0 || *r.Grace%time.Second != 0) {
		return fmt.Errorf("%w: grace is a duration of whole seconds, at least 0", ErrInvalid)
	}
	return nil
}

func ruleArgs(r RetentionRule) (keep, grace any) {
	if r.Keep != nil {
		keep = *r.Keep
	}
	if r.Grace != nil {
		grace = seconds(*r.Grace)
	}
	return keep, grace
}

func scanRule(keep, grace sql.NullInt64) RetentionRule {
	var r RetentionRule
	if keep.Valid {
		k := int(keep.Int64)
		r.Keep = &k
	}
	if grace.Valid {
		g := time.Duration(grace.Int64) * time.Second
		r.Grace = &g
	}
	return r
}

// TenantRetention reads a tenant's default rule.
func (s *Store) TenantRetention(ctx context.Context, tenantID string) (RetentionRule, error) {
	var keep, grace sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT retention_keep, retention_grace_seconds FROM tenants WHERE id = ?"), tenantID).Scan(&keep, &grace)
	return scanRule(keep, grace), notFound(err, "tenant")
}

// TenantRetention is the transactional form.
func (t *Tx) TenantRetention(ctx context.Context, tenantID string) (RetentionRule, error) {
	var keep, grace sql.NullInt64
	err := t.queryRow(ctx, "SELECT retention_keep, retention_grace_seconds FROM tenants WHERE id = ?", tenantID).Scan(&keep, &grace)
	return scanRule(keep, grace), notFound(err, "tenant")
}

// SetTenantRetention stores a tenant's default rule (checked by ValidRetention).
func (t *Tx) SetTenantRetention(ctx context.Context, tenantID string, r RetentionRule) error {
	if err := ValidRetention(r, false); err != nil {
		return err
	}
	keep, grace := ruleArgs(r)
	_, err := t.exec(ctx, "UPDATE tenants SET retention_keep = ?, retention_grace_seconds = ? WHERE id = ?", keep, grace, tenantID)
	return err
}

const channelRetentionQuery = "SELECT retention_keep, retention_grace_seconds, retention_checked_at FROM channels WHERE id = ?"

// ChannelRetention reads a channel's own rule and when the pass last looked at it.
func (s *Store) ChannelRetention(ctx context.Context, channelID string) (RetentionRule, time.Time, error) {
	var keep, grace sql.NullInt64
	var at time.Time
	err := s.db.QueryRowContext(ctx, s.d.rebind(channelRetentionQuery), channelID).Scan(&keep, &grace, scanTime{&at})
	return scanRule(keep, grace), at, notFound(err, "channel")
}

// ChannelRetention is the transactional form.
func (t *Tx) ChannelRetention(ctx context.Context, channelID string) (RetentionRule, time.Time, error) {
	var keep, grace sql.NullInt64
	var at time.Time
	err := t.queryRow(ctx, channelRetentionQuery, channelID).Scan(&keep, &grace, scanTime{&at})
	return scanRule(keep, grace), at, notFound(err, "channel")
}

// SetChannelRetention stores a channel's own rule (checked by ValidRetention).
func (t *Tx) SetChannelRetention(ctx context.Context, channelID string, r RetentionRule) error {
	if err := ValidRetention(r, true); err != nil {
		return err
	}
	keep, grace := ruleArgs(r)
	_, err := t.exec(ctx, "UPDATE channels SET retention_keep = ?, retention_grace_seconds = ? WHERE id = ?", keep, grace, channelID)
	return err
}

// SetRetentionChecked records the pass's look at a channel.
func (t *Tx) SetRetentionChecked(ctx context.Context, channelID string, at time.Time) error {
	_, err := t.exec(ctx, "UPDATE channels SET retention_checked_at = ? WHERE id = ?", t.s.d.timeArg(at), channelID)
	return err
}

// RetentionChannel is a signed channel of an active tenant, as the retention pass lists them: its
// rule and its tenant's, and when the pass last looked at it.
type RetentionChannel struct {
	ChannelID, Channel, TenantID, Tenant string
	Rule, TenantRule                     RetentionRule
	CheckedAt                            time.Time
}

// RetentionChannels lists the signed channels of the tenants that are active (neither suspended nor
// being deleted) whose rule may be in force (a channel's keep set and not none, or not set with the
// tenant's set), by tenant and channel name.
func (s *Store) RetentionChannels(ctx context.Context) ([]RetentionChannel, error) {
	var out []RetentionChannel
	err := s.eachRow(ctx, `SELECT c.id, c.name, t.id, t.name, c.retention_keep, c.retention_grace_seconds, t.retention_keep,
t.retention_grace_seconds, c.retention_checked_at FROM channels c JOIN tenants t ON t.id = c.tenant_id
WHERE c.kind = ? AND t.state = ? AND t.deleting_at IS NULL AND (c.retention_keep > 0 OR c.retention_keep IS NULL AND t.retention_keep IS NOT NULL)
ORDER BY t.name, c.name`, []any{ChannelSigned, TenantActive}, func(sc func(...any) error) error {
		var x RetentionChannel
		var ck, cg, tk, tg sql.NullInt64
		if err := sc(&x.ChannelID, &x.Channel, &x.TenantID, &x.Tenant, &ck, &cg, &tk, &tg, scanTime{&x.CheckedAt}); err != nil {
			return err
		}
		x.Rule, x.TenantRule = scanRule(ck, cg), scanRule(tk, tg)
		out = append(out, x)
		return nil
	})
	return out, err
}

// --- pins ---

// RetentionPin keeps (name, version) in one DuckDB version of a channel, or (DuckDBVersion "") in
// every one, whatever the rule.
type RetentionPin struct {
	ID, ChannelID, Name, PackageVersion, DuckDBVersion, Reason, PinnedBy string
	PinnedAt                                                             time.Time
}

const pinCols = "id, channel_id, name, package_version, duckdb_version, reason, pinned_by, pinned_at"

func scanPin(sc func(...any) error) (RetentionPin, error) {
	var p RetentionPin
	err := sc(&p.ID, &p.ChannelID, &p.Name, &p.PackageVersion, &p.DuckDBVersion, &p.Reason, &p.PinnedBy, scanTime{&p.PinnedAt})
	return p, err
}

// pinOrder orders pins by name, version and DuckDB version (every line, "", first).
const pinOrder = " ORDER BY name, package_version, duckdb_version"

// ChannelPins lists a channel's pins.
func (s *Store) ChannelPins(ctx context.Context, channelID string) ([]RetentionPin, error) {
	var out []RetentionPin
	err := s.eachRow(ctx, "SELECT "+pinCols+" FROM retention_pins WHERE channel_id = ?"+pinOrder, []any{channelID},
		func(sc func(...any) error) error {
			p, err := scanPin(sc)
			out = append(out, p)
			return err
		})
	return out, err
}

// ChannelPins is the transactional form (under the channel lock).
func (t *Tx) ChannelPins(ctx context.Context, channelID string) ([]RetentionPin, error) {
	rows, err := t.query(ctx, "SELECT "+pinCols+" FROM retention_pins WHERE channel_id = ?"+pinOrder, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetentionPin
	for rows.Next() {
		p, err := scanPin(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PinKey is a pin's place in ListPins' order, a cursor.
func PinKey(p RetentionPin) string {
	return p.Name + "\x00" + p.PackageVersion + "\x00" + p.DuckDBVersion
}

// ListPins lists a channel's pins (of one name: "" every one) after a cursor (PinKey), up to limit.
func (s *Store) ListPins(ctx context.Context, channelID, name, after string, limit int) ([]RetentionPin, error) {
	q, args := "SELECT "+pinCols+" FROM retention_pins WHERE channel_id = ?", []any{channelID}
	if name != "" {
		q, args = q+" AND name = ?", append(args, name)
	}
	if after != "" {
		n, rest, _ := strings.Cut(after, "\x00")
		v, d, _ := strings.Cut(rest, "\x00")
		q += " AND (name > ? OR name = ? AND (package_version > ? OR package_version = ? AND duckdb_version > ?))"
		args = append(args, n, n, v, v, d)
	}
	var out []RetentionPin
	if limit > 0 {
		q += pinOrder + s.limit(limit)
	} else {
		q += pinOrder
	}
	err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
		p, err := scanPin(sc)
		out = append(out, p)
		return err
	})
	return out, err
}

// InsertPin pins (name, version, DuckDB version) in a channel; one pinned already is loaded into p
// instead (existed). Call it under the channel lock: the caller checks the version's releases.
func (t *Tx) InsertPin(ctx context.Context, p *RetentionPin) (existed bool, err error) {
	if !validText(p.Reason, MaxPinReason) {
		return false, fmt.Errorf("%w: a pin's reason is 1 to %d bytes of printable text", ErrInvalid, MaxPinReason)
	}
	rows, err := t.query(ctx, "SELECT "+pinCols+" FROM retention_pins WHERE channel_id = ? AND name = ? AND package_version = ? AND duckdb_version = ?",
		p.ChannelID, p.Name, p.PackageVersion, p.DuckDBVersion)
	if err != nil {
		return false, err
	}
	var cur []RetentionPin
	for rows.Next() {
		c, err := scanPin(rows.Scan)
		if err != nil {
			rows.Close()
			return false, err
		}
		cur = append(cur, c)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(cur) > 0 {
		*p = cur[0]
		return true, nil
	}
	p.ID, p.PinnedAt = NewID(), t.Now()
	_, err = t.exec(ctx, "INSERT INTO retention_pins ("+pinCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)", p.ID, p.ChannelID, p.Name,
		p.PackageVersion, p.DuckDBVersion, p.Reason, p.PinnedBy, t.s.d.timeArg(p.PinnedAt))
	return false, t.s.mapErr(err, "pin")
}

// DeletePin removes a channel's pin by id (ErrNotFound for another channel's) and returns it.
func (t *Tx) DeletePin(ctx context.Context, channelID, id string) (RetentionPin, error) {
	var p RetentionPin
	rows, err := t.query(ctx, "SELECT "+pinCols+" FROM retention_pins WHERE id = ? AND channel_id = ?", id, channelID)
	if err != nil {
		return p, err
	}
	found := false
	for rows.Next() {
		if p, err = scanPin(rows.Scan); err != nil {
			rows.Close()
			return p, err
		}
		found = true
	}
	if err := rows.Close(); err != nil {
		return p, err
	}
	if !found {
		return p, fmt.Errorf("%w: pin %s", ErrNotFound, id)
	}
	_, err = t.exec(ctx, "DELETE FROM retention_pins WHERE id = ?", id)
	return p, err
}

// DeletePins removes pins of a channel by id.
func (t *Tx) DeletePins(ctx context.Context, channelID string, ids []string) error {
	for _, id := range ids {
		if _, err := t.exec(ctx, "DELETE FROM retention_pins WHERE id = ? AND channel_id = ?", id, channelID); err != nil {
			return err
		}
	}
	return nil
}

// --- the plan's bodies ---

// BodyHolder is a release of a tenant holding a body: its id and channel.
type BodyHolder struct{ ReleaseID, ChannelID, Channel string }

// BodyUse is what holds a body in a tenant: its releases (every channel, every state), whether a
// landed catalogue build holds it, and its length (builds.body_len, spec 0038; 0 unknown).
type BodyUse struct {
	Holders []BodyHolder
	Landed  bool
	Len     int64
}

// BodyUses reads the uses of a tenant's bodies by hash: the tenant's own rows only.
func (s *Store) BodyUses(ctx context.Context, tenantID string, hashes []string) (map[string]BodyUse, error) {
	out := map[string]BodyUse{}
	for chunk := range chunked(hashes, 500) {
		in := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		args := []any{tenantID}
		for _, h := range chunk {
			args = append(args, h)
		}
		if err := s.eachRow(ctx, `SELECT b.body_hash, r.id, c.id, c.name FROM releases r JOIN builds b ON b.id = r.build_id
JOIN channels c ON c.id = r.channel_id WHERE r.tenant_id = ? AND b.body_hash IN (`+in+`)`, args, func(sc func(...any) error) error {
			var h string
			var x BodyHolder
			if err := sc(&h, &x.ReleaseID, &x.ChannelID, &x.Channel); err != nil {
				return err
			}
			u := out[h]
			u.Holders = append(u.Holders, x)
			out[h] = u
			return nil
		}); err != nil {
			return nil, err
		}
		if err := s.eachRow(ctx, "SELECT body_hash, MAX(body_len), MAX(CASE WHEN landed = ? THEN 1 ELSE 0 END) FROM builds WHERE tenant_id = ? AND body_hash IN ("+in+") GROUP BY body_hash",
			append([]any{true}, args...), func(sc func(...any) error) error {
				var h string
				var n sql.NullInt64
				var landed int
				if err := sc(&h, &n, &landed); err != nil {
					return err
				}
				u := out[h]
				u.Len, u.Landed = n.Int64, landed == 1
				out[h] = u
				return nil
			}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chunked yields s in chunks of at most n.
func chunked[T any](s []T, n int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for len(s) > 0 {
			k := min(n, len(s))
			if !yield(s[:k]) {
				return
			}
			s = s[k:]
		}
	}
}

// DuckDBVersionCAPIs reads a DuckDB version's C API maxima by name (spec 0042: a line the channel
// may no longer serve); the legacy column for a version an older binary added. ErrNotFound for a
// version the server does not have.
func (s *Store) DuckDBVersionCAPIs(ctx context.Context, name string) ([]CAPI, error) {
	var out []CAPI
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		out, err = tx.DuckDBVersionCAPIs(ctx, name)
		return err
	})
	return out, err
}

// DuckDBVersionCAPIs is the transactional form (under the channel lock: a purge's pins).
func (t *Tx) DuckDBVersionCAPIs(ctx context.Context, name string) ([]CAPI, error) {
	var id string
	var legacy sql.NullString
	err := t.queryRow(ctx, "SELECT id, c_api_version FROM duckdb_versions WHERE name = ?", name).Scan(&id, &legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: DuckDB version %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	out, err := t.VersionCAPIs(ctx, id)
	if out == nil {
		out = []CAPI{}
	}
	if err == nil && len(out) == 0 && legacy.Valid {
		if c, perr := ParseCAPI(strings.TrimSpace(legacy.String)); perr == nil {
			out = append(out, c)
		}
	}
	return out, err
}

// ChannelCandidates lists a channel's releases (of one name: "" every one), every source and state,
// with what their builds say, under the caller's channel lock (spec 0042: the line act's read).
func (t *Tx) ChannelCandidates(ctx context.Context, channelID, name string) ([]Candidate, error) {
	q, args := "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id WHERE r.channel_id = ?", []any{channelID}
	if name != "" {
		q, args = q+" AND r.name = ?", append(args, name)
	}
	return t.candidates(ctx, q+" ORDER BY r.created_at, r.id", args...)
}

// VersionReleases lists a channel's releases of a (name, version), every source and state.
func (t *Tx) VersionReleases(ctx context.Context, channelID, name, version string) ([]Candidate, error) {
	return t.candidates(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.name = ? AND r.package_version = ? ORDER BY r.created_at, r.id", channelID, name, version)
}

func (t *Tx) candidates(ctx context.Context, q string, args ...any) ([]Candidate, error) {
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
