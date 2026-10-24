package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Spec 0033: the placement pass's reads and bookkeeping.

// DueRule is a channel's rule the placement pass looks at.
type DueRule struct {
	TenantID, ChannelID, Name, Source, Mode string
}

// DueRules lists the rules with a mode, of signed channels of active tenants, that the placement
// pass should look at: every one (sweep), or those whose source has a build of the name made since
// the pass last looked (or never looked). A rule of the tenant's own source is never the pass's: it
// is everyone who may publish in the tenant.
func (s *Store) DueRules(ctx context.Context, sweep bool) ([]DueRule, error) {
	q := `SELECT c.tenant_id, cs.channel_id, cs.name, cs.source, cs.mode FROM channel_sources cs
JOIN channels c ON c.id = cs.channel_id JOIN tenants t ON t.id = c.tenant_id
WHERE c.kind = 'signed' AND t.state = 'active' AND t.deleting_at IS NULL AND cs.mode IN ('auto', 'manual') AND cs.source <> 'tenant'`
	if !sweep {
		q += ` AND EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = c.tenant_id AND b.source = cs.source AND b.name = cs.name
	AND (cs.checked_at IS NULL OR b.created_at > cs.checked_at))`
	}
	var out []DueRule
	err := s.eachRow(ctx, q+" ORDER BY c.tenant_id, cs.channel_id, cs.name", nil, func(sc func(...any) error) error {
		var r DueRule
		if err := sc(&r.TenantID, &r.ChannelID, &r.Name, &r.Source, &r.Mode); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// SourceBuilds lists a tenant's builds of a name from a source: its catalogue entries.
func (s *Store) SourceBuilds(ctx context.Context, tenantID, source, name string) ([]Build, error) {
	var out []Build
	err := s.eachRow(ctx, "SELECT "+buildCols+" FROM builds WHERE tenant_id = ? AND source = ? AND name = ? ORDER BY created_at, id",
		[]any{tenantID, source, name}, func(sc func(...any) error) error {
			b, err := scanBuild(rowScanner(sc))
			if err == nil {
				out = append(out, b)
			}
			return err
		})
	return out, err
}

// rowScanner adapts eachRow's scan function to scanBuild's.
type rowScanner func(...any) error

func (f rowScanner) Scan(dst ...any) error { return f(dst...) }

// SourceReleases lists a source's releases of a name in a channel, every version and state, with
// what their builds say, and its purged slots (state ReleasePurged: the source's, and those
// purged before slots had a source).
func (t *Tx) SourceReleases(ctx context.Context, channelID, source, name string) ([]Candidate, error) {
	rows, err := t.query(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.source = ? AND r.name = ?", channelID, source, name)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	purged, err := t.query(ctx, "SELECT release_id, ext_version, platform, slot, abi, body_hash, seq FROM purged_slots"+
		" WHERE channel_id = ? AND source IN (?, '*') AND name = ?", channelID, source, name)
	if err != nil {
		return nil, err
	}
	defer purged.Close()
	for purged.Next() {
		c := Candidate{Release: Release{ChannelID: channelID, Source: source, Name: name, State: ReleasePurged}}
		var seq sql.NullInt64
		if err := purged.Scan(&c.ID, &c.ExtVersion, &c.Platform, &c.Slot, &c.ABI, &c.BodyHash, &seq); err != nil {
			return nil, err
		}
		c.Seq = seq.Int64
		out = append(out, c)
	}
	return out, purged.Err()
}

// SetRuleChecked records the placement pass's look at a rule, begun at start: when (builds made
// after it are looked at next time) and its outcome ("" for none). A rule changed since the look
// began (its source, its mode) is left alone, to be looked at again.
func (s *Store) SetRuleChecked(ctx context.Context, r DueRule, start, at time.Time, outcome string) error {
	if len(outcome) > 400 {
		outcome = strings.ToValidUTF8(outcome[:400], "")
	}
	return s.InTx(ctx, "", func(tx *Tx) error {
		var o, oat any
		if outcome != "" {
			o, oat = outcome, tx.s.d.timeArg(tx.Now())
		}
		_, err := tx.exec(ctx, `UPDATE channel_sources SET checked_at = ?, outcome = ?, outcome_at = ?
WHERE channel_id = ? AND name = ? AND source = ? AND mode = ? AND set_at <= ?`,
			tx.s.d.timeArg(at), o, oat, r.ChannelID, r.Name, r.Source, r.Mode, tx.s.d.timeArg(start))
		return err
	})
}

// PurgedBuilds are a tenant's builds of a name from a source that are released nowhere and whose
// body a purge took out of some channel (spec 0016: for good): never placed again.
func (s *Store) PurgedBuilds(ctx context.Context, tenantID, source, name string) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.eachRow(ctx, `SELECT b.id FROM builds b WHERE b.tenant_id = ? AND b.source = ? AND b.name = ?
AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id)
AND EXISTS (SELECT 1 FROM purged_slots p JOIN channels c ON c.id = p.channel_id WHERE c.tenant_id = b.tenant_id AND p.name = b.name
	AND p.ext_version = b.ext_version AND p.body_hash = b.body_hash AND p.source IN (b.source, '*'))`, []any{tenantID, source, name},
		func(sc func(...any) error) error {
			var id string
			if err := sc(&id); err != nil {
				return err
			}
			out[id] = true
			return nil
		})
	return out, err
}

// SetLanded marks a build landed in the catalogue without a release (spec 0033), kept from the
// collector until a release takes it; one with a release is left as it is.
func (t *Tx) SetLanded(ctx context.Context, buildID string) error {
	_, err := t.exec(ctx, "UPDATE builds SET landed = ? WHERE id = ? AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)", true, buildID)
	return err
}

// NarrowBuild makes a build private (spec 0033): a release of it an administrator made private, so
// no placement serves it publicly again.
func (t *Tx) NarrowBuild(ctx context.Context, buildID string) error {
	_, err := t.exec(ctx, "UPDATE builds SET visibility = ? WHERE id = ?", Private, buildID)
	return err
}

// ValidRuleMode says whether m is a mode an administrator sets.
func ValidRuleMode(m string) error {
	if m != RuleAuto && m != RuleManual {
		return fmt.Errorf("%w: a rule's mode is auto or manual", ErrInvalid)
	}
	return nil
}

// SourceVersions are a tenant's catalogue: the versions of each (source, name) it has builds of.
func (s *Store) SourceVersions(ctx context.Context, tenantID string) (map[[2]string][]string, error) {
	out := map[[2]string][]string{}
	err := s.eachRow(ctx, "SELECT source, name, ext_version FROM builds WHERE tenant_id = ? GROUP BY source, name, ext_version",
		[]any{tenantID}, func(sc func(...any) error) error {
			var src, name, v string
			if err := sc(&src, &name, &v); err != nil {
				return err
			}
			k := [2]string{src, name}
			out[k] = append(out[k], v)
			return nil
		})
	return out, err
}

// Rule is a channel's rule, by the channel's name (TenantRules).
type Rule struct {
	Channel, Name, Source, Mode string
}

// TenantRules lists every rule of a tenant's channels.
func (s *Store) TenantRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var out []Rule
	err := s.eachRow(ctx, `SELECT c.name, cs.name, cs.source, cs.mode FROM channel_sources cs JOIN channels c ON c.id = cs.channel_id
WHERE c.tenant_id = ? ORDER BY c.name, cs.name`, []any{tenantID}, func(sc func(...any) error) error {
		var r Rule
		if err := sc(&r.Channel, &r.Name, &r.Source, &r.Mode); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// Placed is a release of a build, by its channel's name and its state (BuildPlacements).
type Placed struct {
	Channel string `json:"channel"`
	State   string `json:"state"`
}

// BuildPlacements lists where a tenant's builds of a name from a source are released, by build id.
func (s *Store) BuildPlacements(ctx context.Context, tenantID, source, name string) (map[string][]Placed, error) {
	out := map[string][]Placed{}
	err := s.eachRow(ctx, `SELECT r.build_id, c.name, r.state FROM releases r JOIN channels c ON c.id = r.channel_id
WHERE r.tenant_id = ? AND r.source = ? AND r.name = ? ORDER BY c.name`, []any{tenantID, source, name}, func(sc func(...any) error) error {
		var id string
		var p Placed
		if err := sc(&id, &p.Channel, &p.State); err != nil {
			return err
		}
		out[id] = append(out[id], p)
		return nil
	})
	return out, err
}

// ErrCatalogueSlot is a build refused because its source has another body of the version in the
// slot (spec 0033: a version is immutable within its source in the whole tenant).
var ErrCatalogueSlot = errors.New("store: the source's slot holds another body; a fix is a new version")

// ErrCatalogueFull is a build refused because its source holds as many entries or bytes as it may
// (spec 0033: catalogue.max_entries, catalogue.max_bytes).
var ErrCatalogueFull = errors.New("store: the source's catalogue is full")

// SourceUsage is how many builds a tenant's source holds placed nowhere (its catalogue's backlog:
// released builds are the channels'), and their bodies' bytes.
func (s *Store) SourceUsage(ctx context.Context, tenantID, source string) (entries int, bytes int64, err error) {
	var n sql.NullInt64
	err = s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*), SUM(bl.body_len) FROM builds b JOIN tenants t ON t.id = b.tenant_id
LEFT JOIN blobs bl ON bl.domain = t.storage_domain AND bl.body_hash = b.body_hash WHERE b.tenant_id = ? AND b.source = ?
AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id)`), tenantID, source).Scan(&entries, &n)
	return entries, n.Int64, err
}

// LandedBuild is a build landed in the catalogue without a release (TrimLanded's reads).
type LandedBuild struct{ ID, TenantID, Source, Name, Version, Platform, Slot, BodyHash string }

const landedCols = "id, tenant_id, source, name, ext_version, platform, slot, body_hash"

func scanLanded(out *[]LandedBuild) func(sc func(...any) error) error {
	return func(sc func(...any) error) error {
		var b LandedBuild
		if err := sc(&b.ID, &b.TenantID, &b.Source, &b.Name, &b.Version, &b.Platform, &b.Slot, &b.BodyHash); err != nil {
			return err
		}
		*out = append(*out, b)
		return nil
	}
}

// CrowdedLanded lists the landed builds of a publisher's or the tenant's (tenant, source, name,
// platform, slot) that has more than keep landed versions.
func (s *Store) CrowdedLanded(ctx context.Context, keep int) ([]LandedBuild, error) {
	var out []LandedBuild
	err := s.eachRow(ctx, "SELECT "+landedCols+` FROM builds b WHERE landed = ? AND source NOT LIKE 'upstream:%' AND EXISTS (
SELECT 1 FROM builds o WHERE o.landed = ? AND o.tenant_id = b.tenant_id AND o.source = b.source AND o.name = b.name AND o.platform = b.platform
	AND o.slot = b.slot GROUP BY o.tenant_id HAVING COUNT(DISTINCT o.ext_version) > ?)`, []any{true, true, keep}, scanLanded(&out))
	return out, err
}

// StaleMirrorLanded lists a mirror's landed builds whose cell holds another body now (the cell moved
// on), or whose upstream is gone.
func (s *Store) StaleMirrorLanded(ctx context.Context) ([]LandedBuild, error) {
	var out []LandedBuild
	err := s.eachRow(ctx, "SELECT "+landedCols+" FROM builds b WHERE landed = ? AND source LIKE 'upstream:%'", []any{true}, scanLanded(&out))
	if err != nil {
		return nil, err
	}
	var stale []LandedBuild
	for _, b := range out {
		var n int
		if err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM upstream_cells WHERE upstream_id = ? AND name = ? AND body_hash = ?"),
			strings.TrimPrefix(b.Source, SourceUpstreamPrefix), b.Name, b.BodyHash).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			stale = append(stale, b)
		}
	}
	return stale, nil
}

// Unland lets the collector take builds landed in the catalogue that wait no more (spec 0033: an
// older version than those a source's name keeps): their grace starts now.
func (s *Store) Unland(ctx context.Context, ids []string) error {
	return s.InTx(ctx, "", func(tx *Tx) error {
		for _, id := range ids {
			if _, err := tx.exec(ctx, `UPDATE builds SET landed = ?, used_at = ? WHERE id = ? AND landed = ?
AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)`, false, tx.s.d.timeArg(tx.Now()), id, true); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteEntry deletes a tenant's build that no release holds (spec 0033: an entry purged from the
// catalogue; its slot is free, since no one could have installed it), with its licence-key records;
// the collector removes the body. It returns the build.
func (t *Tx) DeleteEntry(ctx context.Context, tenantID, id string) (Build, error) {
	b, err := scanBuild(t.queryRow(ctx, "SELECT "+buildCols+" FROM builds WHERE id = ? AND tenant_id = ?", id, tenantID))
	if err != nil {
		return Build{}, notFound(err, "build "+id)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM releases WHERE build_id = ?", id).Scan(&n); err != nil {
		return Build{}, err
	}
	if n > 0 {
		return Build{}, fmt.Errorf("%w: the build is released; yank and purge its releases first", ErrConflict)
	}
	// a build once released (its release purged since) was installable: it stays, its slot taken
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM purged_slots p JOIN channels c ON c.id = p.channel_id WHERE c.tenant_id = ?
AND p.source = ? AND p.name = ? AND p.ext_version = ? AND p.platform = ? AND p.slot = ? AND p.body_hash = ?`,
		tenantID, b.Source, b.Name, b.ExtVersion, b.Platform, b.Slot(), b.BodyHash).Scan(&n); err != nil {
		return Build{}, err
	}
	if n > 0 {
		return Build{}, fmt.Errorf("%w: the build was released (its release purged since): it stays in the catalogue", ErrConflict)
	}
	if _, err := t.exec(ctx, "DELETE FROM build_license_keys WHERE build_id = ?", id); err != nil {
		return Build{}, err
	}
	if _, err := t.exec(ctx, "DELETE FROM builds WHERE id = ?", id); err != nil {
		if t.s.d.foreignKey(err) { // a placement released it meanwhile
			return Build{}, fmt.Errorf("%w: the build was just released", ErrConflict)
		}
		return Build{}, err
	}
	// a mirror's cell that landed it fetches it again at its next run
	if id, ok := strings.CutPrefix(b.Source, SourceUpstreamPrefix); ok {
		if _, err := t.exec(ctx, "UPDATE upstream_cells SET etag = NULL WHERE upstream_id = ? AND name = ? AND body_hash = ?", id, b.Name, b.BodyHash); err != nil {
			return Build{}, err
		}
	}
	return b, nil
}

// DuckDBVersionKnown reports whether a DuckDB version is registered on the server.
func (s *Store) DuckDBVersionKnown(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM duckdb_versions WHERE name = ?"), name).Scan(&n)
	return n > 0, err
}

// BuildOf reads a tenant's build of a name and body from a source; ok is false when there is none.
func (s *Store) BuildOf(ctx context.Context, tenantID, source, name, bodyHash string) (b Build, ok bool, err error) {
	b, err = scanBuild(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+buildCols+" FROM builds WHERE tenant_id = ? AND source = ? AND name = ? AND body_hash = ?"),
		tenantID, source, name, bodyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, false, nil
	}
	return b, err == nil, err
}

// SlotTaken refuses a build of a source other than the tenant's own when the source's slot holds
// another body, among its builds (legacy duplicates included) or its purged slots in the tenant's
// channels (spec 0033: a version is immutable within its source, even once its build is gone). A
// slot purged before slots had a source ('*') stays a matter of its own channel.
func (t *Tx) SlotTaken(ctx context.Context, b Build) error {
	if b.Source == SourceTenant || b.Source == "" {
		return nil // everyone who may publish in the tenant: the rule stays per channel
	}
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM builds WHERE tenant_id = ? AND source = ? AND name = ? AND ext_version = ? AND platform = ?
AND slot = ? AND body_hash <> ?`, b.TenantID, b.Source, b.Name, b.ExtVersion, b.Platform, b.Slot(), b.BodyHash).Scan(&n)
	if err == nil && n == 0 {
		err = t.queryRow(ctx, `SELECT COUNT(*) FROM purged_slots p JOIN channels c ON c.id = p.channel_id WHERE c.tenant_id = ?
AND p.source = ? AND p.name = ? AND p.ext_version = ? AND p.platform = ? AND p.slot = ? AND p.body_hash <> ?`,
			b.TenantID, b.Source, b.Name, b.ExtVersion, b.Platform, b.Slot(), b.BodyHash).Scan(&n)
	}
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s %s %s %s", ErrCatalogueSlot, b.Name, b.ExtVersion, b.Platform, b.Slot())
	}
	return nil
}

// SlotTaken is the read before a body is stored (the insert checks again).
func (s *Store) SlotTaken(ctx context.Context, b Build) error {
	return s.InTx(ctx, "", func(tx *Tx) error { return tx.SlotTaken(ctx, b) })
}
