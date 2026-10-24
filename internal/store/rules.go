package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Spec 0033: the placement pass's reads and bookkeeping.

// DueRule is a channel's rule the placement pass looks at.
type DueRule struct {
	TenantID, ChannelID, Name, Source, Mode string
}

// DueRules lists the rules of signed channels of active tenants that the placement pass should
// look at: every one (sweep), or those whose source has a build of the name made since the pass
// last looked (or never looked).
func (s *Store) DueRules(ctx context.Context, sweep bool) ([]DueRule, error) {
	q := `SELECT c.tenant_id, cs.channel_id, cs.name, cs.source, cs.mode FROM channel_sources cs
JOIN channels c ON c.id = cs.channel_id JOIN tenants t ON t.id = c.tenant_id
WHERE c.kind = 'signed' AND t.state = 'active' AND t.deleting_at IS NULL`
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

// SetRuleChecked records the placement pass's look at a rule: when (builds made after it are looked
// at next time) and its outcome ("" for none). A rule whose source changed since is left alone.
func (s *Store) SetRuleChecked(ctx context.Context, channelID, name, source string, at time.Time, outcome string) error {
	if len(outcome) > 400 {
		outcome = outcome[:400]
	}
	return s.InTx(ctx, "", func(tx *Tx) error {
		var o, oat any
		if outcome != "" {
			o, oat = outcome, tx.s.d.timeArg(tx.Now())
		}
		_, err := tx.exec(ctx, "UPDATE channel_sources SET checked_at = ?, outcome = ?, outcome_at = ? WHERE channel_id = ? AND name = ? AND source = ?",
			tx.s.d.timeArg(at), o, oat, channelID, name, source)
		return err
	})
}

// ValidRuleMode says whether m is a rule mode.
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
