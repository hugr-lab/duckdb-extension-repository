package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

// The trusted sources of a build's licence keys (spec 0021).
const (
	KeySourcePublisher = "publisher" // a marked publisher's publication
	KeySourceUpstream  = "upstream"  // a repository upstream's fetch, or a bundle import through one
)

// LicenseKey is a licence key a build declares in its key block: its kid and its PEM.
type LicenseKey struct{ Kid, PEM string }

// keySource is the source a key record's (kind, id) is: upstream:<id> or publisher:<id>.
func keySource(kind, id string) string { return kind + ":" + id }

// splitKeySource is keySource's inverse: false for a source no key record names (tenant).
func splitKeySource(src string) (kind, id string, ok bool) {
	if id, ok := strings.CutPrefix(src, SourceUpstreamPrefix); ok {
		return KeySourceUpstream, id, true
	}
	if id, ok := strings.CutPrefix(src, SourcePublisherPrefix); ok {
		return KeySourcePublisher, id, true
	}
	return "", "", false
}

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
		added = true
	}
	if !added {
		return false, nil
	}
	return true, t.bumpBuildChannels(ctx, "SELECT DISTINCT channel_id FROM releases WHERE build_id = ? ORDER BY channel_id", buildID)
}

// DeleteLicenseKeysOf drops the keys a source brought, from every build, and bumps the release
// version of each channel holding one of those builds: the source's entries have no live key from
// then on (spec 0035).
func (t *Tx) DeleteLicenseKeysOf(ctx context.Context, kind, sourceID string) error {
	if err := t.bumpBuildChannels(ctx, `SELECT DISTINCT r.channel_id FROM releases r
WHERE r.build_id IN (SELECT build_id FROM build_license_keys WHERE source_kind = ? AND source_id = ?) ORDER BY r.channel_id`, kind, sourceID); err != nil {
		return err
	}
	_, err := t.exec(ctx, "DELETE FROM build_license_keys WHERE source_kind = ? AND source_id = ?", kind, sourceID)
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

// LiveLicenseKeys are the live keys of a catalogue entry (spec 0035): of a package (type, name) of
// one source, upstream:<id> or publisher:<id>, the keys that source recorded on its own builds of
// the name, while it exists (spec 0021), from a live build (an active or deprecated release in a
// signed channel that serves the build's source for the name, its body not blocked, not withdrawn;
// a held build placed early is live), or from the source's newest available version of the name
// in the catalogue (not held, withdrawn nor revoked, its body not blocked), so a licence is taken
// before any placement. A record one source made on another source's build counts for neither. Any
// other source (tenant) has none.
func (t *Tx) LiveLicenseKeys(ctx context.Context, tenantID, packageType, name, source string) ([]LicenseKey, error) {
	kind, id, ok := splitKeySource(source)
	if !ok {
		return nil, nil
	}
	keys := map[string]string{}
	rows, err := t.query(ctx, `SELECT DISTINCT k.kid, k.pem FROM build_license_keys k JOIN builds b ON b.id = k.build_id
WHERE b.tenant_id = ? AND b.package_type = ? AND b.name = ? AND b.source = ? AND k.source_kind = ? AND k.source_id = ? AND `+liveBuild,
		tenantID, packageType, name, source, kind, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kid, pem string
		if err := rows.Scan(&kid, &pem); err != nil {
			rows.Close()
			return nil, err
		}
		keys[kid] = pem
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the source's newest available version, and the keys it recorded on its builds of it
	type versionKey struct {
		version  string
		kid, pem sql.NullString
	}
	var vks []versionKey
	rows, err = t.query(ctx, `SELECT b.package_version, k.kid, k.pem FROM builds b
LEFT JOIN build_license_keys k ON k.build_id = b.id AND k.source_kind = ? AND k.source_id = ?
WHERE b.tenant_id = ? AND b.package_type = ? AND b.name = ? AND b.source = ? AND b.withdrawn_at IS NULL AND b.revoked_at IS NULL
AND (b.available_at IS NULL OR b.available_at <= ?)
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)`,
		kind, id, tenantID, packageType, name, source, t.s.d.timeArg(t.Now()))
	if err != nil {
		return nil, err
	}
	newest := ""
	for rows.Next() {
		var vk versionKey
		if err := rows.Scan(&vk.version, &vk.kid, &vk.pem); err != nil {
			rows.Close()
			return nil, err
		}
		vks = append(vks, vk)
		if newest == "" || CompareVersions(vk.version, newest) > 0 {
			newest = vk.version
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if newest != "" {
		var exists int
		// the source as it is now: a removed one's keys count for nothing
		q := "SELECT COUNT(*) FROM upstreams WHERE id = ?"
		if kind == KeySourcePublisher {
			q = "SELECT COUNT(*) FROM publishers WHERE id = ?"
		}
		if err := t.queryRow(ctx, q, id).Scan(&exists); err != nil {
			return nil, err
		}
		for _, vk := range vks {
			if exists > 0 && vk.version == newest && vk.kid.Valid {
				keys[vk.kid.String] = vk.pem.String
			}
		}
	}
	out := make([]LicenseKey, 0, len(keys))
	for kid, pem := range keys {
		out = append(out, LicenseKey{Kid: kid, PEM: pem})
	}
	slices.SortFunc(out, func(a, b LicenseKey) int { return strings.Compare(a.Kid, b.Kid) })
	return out, nil
}

// SuppliedBy reports whether an upstream supplies a name of a type to its tenant (spec 0012: an
// upstream hands out licences only for names it supplies; spec 0033 phase B: through the catalogue):
// a build of the name the upstream brought since it was added (a successor's copies of a removed
// upstream's builds are not its own), not withdrawn nor revoked, its body not blocked, that is
// available in the catalogue or live in a channel that serves the upstream for the name.
func (t *Tx) SuppliedBy(ctx context.Context, u Upstream, packageType, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM builds b WHERE b.tenant_id = ? AND b.package_type = ? AND b.name = ? AND b.source = ?
AND b.created_at >= ? AND b.withdrawn_at IS NULL AND b.revoked_at IS NULL
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)
AND (b.available_at IS NULL OR b.available_at <= ? OR EXISTS (SELECT 1 FROM releases r JOIN channels c ON c.id = r.channel_id
	JOIN channel_sources cs ON cs.channel_id = r.channel_id AND cs.name = r.name AND cs.source = r.source
	WHERE r.build_id = b.id AND r.state IN ('active', 'deprecated') AND c.kind = 'signed'))`,
		u.TenantID, packageType, name, UpstreamSource(u.ID), t.s.d.timeArg(u.CreatedAt), t.s.d.timeArg(t.Now())).Scan(&n)
	return n > 0, err
}

// NameKey is a live licence key of a tenant's package (LiveLicenseKeys' rule), with the entries for
// which it is live (spec 0035).
type NameKey struct {
	Name, Kid string
	Sources   []KeySource
}

// KeySource is an entry a key is live for: its source by kind and name (and, for a distributor's
// publisher, its distributor), and the number of the entry's builds declaring the key that count
// (live, or of its newest available version).
type KeySource struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Distributor string `json:"distributor,omitempty"`
	Builds      int    `json:"builds"`
	// Source is the entry's source as stored: upstream:<id> or publisher:<id>
	Source string `json:"-"`
}

const keySourceName = "COALESCE(p.name, u.name, k.source_id)"

// TenantLicenseKeys lists the live keys of a tenant's packages of a type (of one name, or of all:
// name ""), by name and kid, with the entries for which each is live (of one source, or of all:
// source ""): what an import into an entry verifies against (LiveLicenseKeys, every entry read at
// once).
func (s *Store) TenantLicenseKeys(ctx context.Context, tenantID, packageType, name, source string) (out []NameKey, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		out, err = tx.tenantLicenseKeys(ctx, tenantID, packageType, name, source)
		return err
	})
	return out, err
}

func (t *Tx) tenantLicenseKeys(ctx context.Context, tenantID, packageType, name, source string) ([]NameKey, error) {
	filter := ""
	args := []any{tenantID, packageType, t.s.d.timeArg(t.Now())}
	if name != "" {
		filter = " AND b.name = ?"
		args = append(args, name)
	}
	if source != "" {
		filter += " AND b.source = ?"
		args = append(args, source)
	}
	// the newest available version of each (name, source) in the catalogue
	newest := map[[2]string]string{}
	rows, err := t.query(ctx, `SELECT b.name, b.source, b.package_version FROM builds b
WHERE b.tenant_id = ? AND b.package_type = ? AND b.withdrawn_at IS NULL AND b.revoked_at IS NULL AND (b.available_at IS NULL OR b.available_at <= ?)
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)`+filter+`
GROUP BY b.name, b.source, b.package_version`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, src, v string
		if err := rows.Scan(&n, &src, &v); err != nil {
			rows.Close()
			return nil, err
		}
		k := [2]string{n, src}
		if cur, ok := newest[k]; !ok || CompareVersions(v, cur) > 0 {
			newest[k] = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type rec struct {
		name, kid, build, buildSource, version string
		src                                    KeySource
		srcID                                  string
		distributor                            sql.NullString
		live, avail                            bool
	}
	var recs []rec
	rows, err = t.query(ctx, `SELECT b.name, k.kid, k.build_id, b.source, b.package_version, k.source_kind, k.source_id, `+keySourceName+`,
p.distributor, CASE WHEN `+liveBuild+` THEN 1 ELSE 0 END,
CASE WHEN b.withdrawn_at IS NULL AND b.revoked_at IS NULL AND (b.available_at IS NULL OR b.available_at <= ?)
	AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash) THEN 1 ELSE 0 END
FROM build_license_keys k JOIN builds b ON b.id = k.build_id
LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
LEFT JOIN upstreams u ON k.source_kind = 'upstream' AND u.id = k.source_id
WHERE b.tenant_id = ? AND b.package_type = ? AND `+sourceExists+filter+`
ORDER BY b.name, k.kid, k.source_kind, `+keySourceName+`, k.build_id`, append([]any{args[2], args[0], args[1]}, args[3:]...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r rec
		var live, avail int
		if err := rows.Scan(&r.name, &r.kid, &r.build, &r.buildSource, &r.version, &r.src.Kind, &r.srcID, &r.src.Name, &r.distributor,
			&live, &avail); err != nil {
			rows.Close()
			return nil, err
		}
		r.live, r.avail = live == 1, avail == 1
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []NameKey
	var builds []string // of the last entry
	for _, r := range recs {
		src := keySource(r.src.Kind, r.srcID)
		if r.buildSource != src { // a record on another source's build counts for neither
			continue
		}
		catalogue := r.avail && newest[[2]string{r.name, src}] == r.version // the source's newest available version
		if !r.live && !catalogue {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Name != r.name || out[len(out)-1].Kid != r.kid {
			out = append(out, NameKey{Name: r.name, Kid: r.kid})
		}
		last := &out[len(out)-1]
		if n := len(last.Sources); n == 0 || last.Sources[n-1].Source != src {
			ks := r.src
			ks.Source, ks.Distributor = src, r.distributor.String
			last.Sources = append(last.Sources, ks)
			builds = builds[:0]
		}
		if !slices.Contains(builds, r.build) {
			builds = append(builds, r.build)
			last.Sources[len(last.Sources)-1].Builds++
		}
	}
	return out, nil
}

// BuildKids are the kids recorded for each of a channel's builds (by build id), each by the build's
// own source (spec 0035: a record one source made on another source's build counts for neither):
// what a served snapshot carries.
func (s *Store) BuildKids(ctx context.Context, channelID string) (map[string][]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT k.build_id, k.kid, k.source_kind, k.source_id, b.source FROM build_license_keys k
JOIN builds b ON b.id = k.build_id
WHERE k.build_id IN (SELECT build_id FROM releases WHERE channel_id = ?) AND `+sourceExists+` ORDER BY k.build_id, k.kid`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var b, kid, kind, id, src string
		if err := rows.Scan(&b, &kid, &kind, &id, &src); err != nil {
			return nil, err
		}
		if src == keySource(kind, id) {
			out[b] = append(out[b], kid)
		}
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
