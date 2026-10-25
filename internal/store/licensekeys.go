package store

import (
	"context"
	"database/sql"
	"slices"
)

// The trusted sources of a build's licence keys (spec 0021).
const (
	KeySourcePublisher = "publisher" // a marked publisher's publication
	KeySourceUpstream  = "upstream"  // a repository upstream's fetch, or a bundle import through one
)

// LicenseKey is a licence key a build declares in its key block: its kid and its PEM.
type LicenseKey struct{ Kid, PEM string }

// sourceExists is the condition that key row k's source still exists: a key recorded by an intake
// that raced its source's removal counts for nothing.
const sourceExists = `(k.source_kind = 'publisher' AND EXISTS (SELECT 1 FROM publishers sp WHERE sp.id = k.source_id)
OR k.source_kind = 'upstream' AND EXISTS (SELECT 1 FROM upstreams su WHERE su.id = k.source_id))`

// liveBuild is the condition that build b is live in its tenant (spec 0021): an active or
// deprecated release in a signed channel that serves its source for the name (spec 0031; another
// source's releases stay stored, unserved), its body not blocked, and it not withdrawn (spec 0033
// phase B: served nowhere); and k's source exists.
const liveBuild = sourceExists + ` AND b.withdrawn_at IS NULL AND EXISTS (SELECT 1 FROM releases r JOIN channels c ON c.id = r.channel_id
JOIN channel_sources cs ON cs.channel_id = r.channel_id AND cs.name = r.name AND cs.source = r.source
WHERE r.build_id = b.id AND r.state IN ('active', 'deprecated') AND c.kind = 'signed')
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)`

// RecordLicenseKeys records the keys of a build that a trusted source's intake carried, those not
// recorded for that source yet. When it records any, it bumps the release version of each channel
// holding the build, so served snapshots pick them up. It reports whether it recorded any.
func (t *Tx) RecordLicenseKeys(ctx context.Context, tenantID, buildID, kind, sourceID string, keys []LicenseKey) (bool, error) {
	if len(keys) == 0 {
		return false, nil
	}
	// the source as it is now: a publisher removed or unmarked, or an upstream removed, since the
	// intake began records nothing
	q := "SELECT COUNT(*) FROM upstreams WHERE id = ?"
	if kind == KeySourcePublisher {
		q = "SELECT COUNT(*) FROM publishers WHERE id = ? AND license_keys = ?"
	}
	args := []any{sourceID}
	if kind == KeySourcePublisher {
		args = append(args, true)
	}
	var n int
	if err := t.queryRow(ctx, q, args...).Scan(&n); err != nil || n == 0 {
		return false, err
	}
	rows, err := t.query(ctx, "SELECT kid FROM build_license_keys WHERE build_id = ? AND source_kind = ? AND source_id = ?",
		buildID, kind, sourceID)
	if err != nil {
		return false, err
	}
	have, err := scanStrings(rows)
	if err != nil {
		return false, err
	}
	added := false
	for _, k := range keys {
		if slices.Contains(have, k.Kid) {
			continue
		}
		have = append(have, k.Kid)
		if _, err := t.exec(ctx, `INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, tenantID, buildID, k.Kid, k.PEM, kind, sourceID, t.s.d.timeArg(t.Now())); err != nil {
			return false, err
		}
		// spec 0033 phase B: the key's owner for the name, claimed by its first recorder
		if err := t.noteKeyOwner(ctx, tenantID, buildID, keySource(kind, sourceID), k.Kid); err != nil {
			return false, err
		}
		added = true
	}
	if !added {
		return false, nil
	}
	return true, t.bumpBuildChannels(ctx, "SELECT DISTINCT channel_id FROM releases WHERE build_id = ? ORDER BY channel_id", buildID)
}

// DeleteLicenseKeysOf drops the keys a source brought, from every build, and bumps the release
// version of each channel holding one of those builds. The keys it owns (recorded, or as the first
// recorder computed) are left without an owner (spec 0033 phase B): only a record made after this
// one makes a source their owner, or a tenant administrator's act. Its caller calls it before it
// removes the source's row and entries, which the first recorder reads.
func (t *Tx) DeleteLicenseKeysOf(ctx context.Context, kind, sourceID string) error {
	src := keySource(kind, sourceID)
	rows, err := t.query(ctx, `SELECT DISTINCT b.tenant_id, b.package_type, b.name, k.kid FROM build_license_keys k JOIN builds b ON b.id = k.build_id
WHERE k.source_kind = ? AND k.source_id = ? AND b.source = ?`, kind, sourceID, src)
	if err != nil {
		return err
	}
	var keys [][4]string
	for rows.Next() {
		var k [4]string
		if err := rows.Scan(&k[0], &k[1], &k[2], &k[3]); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		o, err := t.KeyOwnerOf(ctx, k[0], k[1], k[2], k[3])
		if err != nil {
			return err
		}
		if o.Recorded || o.Source != src {
			continue
		}
		// never over another row than the one read: an administrator's act racing the removal wins
		if o.since.IsZero() {
			_, err = t.putOwner(ctx, k[0], k[1], k[2], k[3], ownerRemoved, ActorLicenses, false)
		} else {
			_, err = t.overRemoved(ctx, o, ownerRemoved)
		}
		if err != nil {
			return err
		}
	}
	if _, err := t.exec(ctx, "UPDATE license_key_owners SET source = ?, set_by = ?, set_at = ? WHERE source = ?",
		ownerRemoved, ActorLicenses, t.s.d.timeArg(t.Now()), src); err != nil {
		return err
	}
	if err := t.bumpBuildChannels(ctx, `SELECT DISTINCT r.channel_id FROM releases r
WHERE r.build_id IN (SELECT build_id FROM build_license_keys WHERE source_kind = ? AND source_id = ?) ORDER BY r.channel_id`, kind, sourceID); err != nil {
		return err
	}
	_, err = t.exec(ctx, "DELETE FROM build_license_keys WHERE source_kind = ? AND source_id = ?", kind, sourceID)
	return err
}

// bumpBuildChannels bumps the channels q lists, in id order (no deadlock between two of them).
func (t *Tx) bumpBuildChannels(ctx context.Context, q string, args ...any) error {
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return err
	}
	channels, err := scanStrings(rows)
	if err != nil {
		return err
	}
	for _, c := range channels {
		if err := t.BumpReleaseVersion(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// BuildKids are the kids recorded for each of a channel's builds (by build id), from any source:
// what a served snapshot carries.
func (s *Store) BuildKids(ctx context.Context, channelID string) (map[string][]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT k.build_id, k.kid FROM build_license_keys k
WHERE k.build_id IN (SELECT build_id FROM releases WHERE channel_id = ?) AND `+sourceExists+` ORDER BY k.build_id, k.kid`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var b, kid string
		if err := rows.Scan(&b, &kid); err != nil {
			return nil, err
		}
		out[b] = append(out[b], kid)
	}
	return out, rows.Err()
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
