package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Spec 0033 phase B part 2: in a tenant, each licence key of a package (type, name, kid) has one
// owner, the source a licence signed with it is of. The first source whose entry recorded the key
// while the entry counted owns it, and keeps it until it goes (removing the source drops its keys
// and its ownership; the next source to record the key afresh owns it) or a tenant administrator
// gives it to another source. Another source's build declaring the same key gets no licence through
// it.

// ActorLicenses is the actor of the licence machinery's own acts: a first recorder's ownership.
const ActorLicenses = "system:licenses"

// ownerRemoved is the source of a key's owner row once its owner was removed: no source owns the
// key, and only the records made after the removal (the row's set_at) count toward its next owner,
// so the key never passes by itself to a source that recorded it while another owned it, or whose
// records did not count then (a distributor's publisher's, for a name an upstream listed).
const ownerRemoved = ""

// ErrNotDeclared is a key's owner set to a source that never declared the key for the name.
var ErrNotDeclared = errors.New("store: the source declares no such licence key for the name")

// KeyOwner is the source that owns a licence key for a package in a tenant: recorded (SetBy, SetAt),
// or the first recorder computed and not recorded yet (Recorded false). Source "" is no owner: no
// record of the key counts, or one still held may yet count before the others.
type KeyOwner struct {
	TenantID, PackageType, Name, Kid string
	Source                           string
	SetBy                            string
	SetAt                            time.Time
	Recorded                         bool
	// since is the removal of the key's owner before (zero: none): only records after it count
	since time.Time
}

// ValidLicenseOwner says whether s is a licence's source: an upstream's or a publisher's
// (upstream:<id>, publisher:<id>), or LicenseUnknown.
func ValidLicenseOwner(s string) bool {
	if s == LicenseUnknown {
		return true
	}
	_, id, ok := splitKeySource(s)
	return ok && id != "" && sourceID.MatchString(id)
}

// keySource is the source a key record's (kind, id) is: upstream:<id> or publisher:<id>.
func keySource(kind, id string) string { return kind + ":" + id }

// splitKeySource is keySource's inverse: false for a source no key record names (tenant, unknown).
func splitKeySource(src string) (kind, id string, ok bool) {
	if id, ok := strings.CutPrefix(src, SourceUpstreamPrefix); ok {
		return KeySourceUpstream, id, true
	}
	if id, ok := strings.CutPrefix(src, SourcePublisherPrefix); ok {
		return KeySourcePublisher, id, true
	}
	return "", "", false
}

// recordedOwner reads a key's recorded owner; ok false when none is recorded (since set when its
// owner was removed).
func (t *Tx) recordedOwner(ctx context.Context, tenantID, packageType, name, kid string) (KeyOwner, bool, error) {
	o := KeyOwner{TenantID: tenantID, PackageType: packageType, Name: name, Kid: kid}
	var src, by string
	var at time.Time
	err := t.queryRow(ctx, `SELECT source, set_by, set_at FROM license_key_owners WHERE tenant_id = ? AND package_type = ? AND name = ?
AND kid = ?`, tenantID, packageType, name, kid).Scan(&src, &by, scanTime{&at})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return o, false, nil
	case err != nil:
		return o, false, err
	case src == ownerRemoved:
		o.since = at
		return o, false, nil
	}
	o.Source, o.SetBy, o.SetAt, o.Recorded = src, by, at, true
	return o, true, nil
}

// putOwner writes a key's owner row: inserted where there is none (upsert false; it reports 0 rows
// where there is one), or set whatever is there (upsert).
func (t *Tx) putOwner(ctx context.Context, tenantID, packageType, name, kid, source, by string, upsert bool) (int64, error) {
	now := t.s.d.timeArg(t.Now())
	var res sql.Result
	var err error
	if t.s.d.Name == "sqlserver" {
		matched := ""
		args := []any{tenantID, packageType, name, kid}
		if upsert {
			matched = "\nWHEN MATCHED THEN UPDATE SET source = ?, set_by = ?, set_at = ?"
			args = append(args, source, by, now)
		}
		res, err = t.exec(ctx, `MERGE license_key_owners WITH (HOLDLOCK) AS o
USING (SELECT ? AS tenant_id, ? AS package_type, ? AS name, ? AS kid) AS n
ON o.tenant_id = n.tenant_id AND o.package_type = n.package_type AND o.name = n.name AND o.kid = n.kid`+matched+`
WHEN NOT MATCHED THEN INSERT (tenant_id, package_type, name, kid, source, set_by, set_at) VALUES (?, ?, ?, ?, ?, ?, ?);`,
			append(args, tenantID, packageType, name, kid, source, by, now)...)
	} else {
		conflict := "DO NOTHING"
		if upsert {
			conflict = "DO UPDATE SET source = excluded.source, set_by = excluded.set_by, set_at = excluded.set_at"
		}
		res, err = t.exec(ctx, `INSERT INTO license_key_owners (tenant_id, package_type, name, kid, source, set_by, set_at)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, package_type, name, kid) `+conflict, tenantID, packageType, name, kid, source, by, now)
	}
	if err != nil {
		return 0, t.s.mapErr(err, "licence key owner")
	}
	return res.RowsAffected()
}

// upstreamListed reports whether a repository upstream of the tenant lists a name of a type, by name
// or "*": a distributor's publisher's records do not make it the name's key owner then (spec 0033
// phase B: the tenant's own upstream keeps the name's keys unless an administrator gives them).
func (t *Tx) upstreamListed(ctx context.Context, tenantID, packageType, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id JOIN channels c ON c.id = u.channel_id
WHERE u.tenant_id = ? AND u.kind = 'repository' AND c.package_type = ? AND (e.name = ? OR e.name = '*')`, tenantID, packageType, name).Scan(&n)
	return n > 0, err
}

// firstRecorder is the source whose record of a key for a name started counting first ("": none):
// a record counts from when its entry was available (its recording, or the end of its entry's hold,
// or the entry's first release if that came sooner: a held entry placed early), and never for a
// revoked entry or a blocked body, nor once the entry was withdrawn; a distributor's publisher's
// record does not count for a name a repository upstream of the tenant lists; a removed source's
// records count for nothing, and so do those made before the removal of the key's owner (since).
// Ties go to the smaller source, so every replica computes the same owner. While another source's
// record made before the first recorder counted is on an entry still held, there is no owner: that
// hold has not ended, and a copier without one does not take the key meanwhile.
func (t *Tx) firstRecorder(ctx context.Context, tenantID, packageType, name, kid string, since time.Time) (string, error) {
	listed, err := t.upstreamListed(ctx, tenantID, packageType, name)
	if err != nil {
		return "", err
	}
	rows, err := t.query(ctx, `SELECT k.source_kind, k.source_id, b.source, k.recorded_at, b.available_at, b.withdrawn_at,
(SELECT MIN(r.created_at) FROM releases r WHERE r.build_id = b.id), p.distributor
FROM build_license_keys k JOIN builds b ON b.id = k.build_id
LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
WHERE k.tenant_id = ? AND k.kid = ? AND b.package_type = ? AND b.name = ? AND b.revoked_at IS NULL AND `+sourceExists+`
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)`, tenantID, kid, packageType, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type held struct {
		src      string
		recorded time.Time
	}
	var pending []held
	now := t.Now()
	best, bestAt := "", time.Time{}
	for rows.Next() {
		var kind, id, buildSource string
		var recorded, available, withdrawn, released time.Time
		var distributor sql.NullString
		if err := rows.Scan(&kind, &id, &buildSource, scanTime{&recorded}, scanTime{&available}, scanTime{&withdrawn}, scanTime{&released},
			&distributor); err != nil {
			return "", err
		}
		src := keySource(kind, id)
		if buildSource != src || distributor.Valid && listed || !since.IsZero() && !recorded.After(since) {
			continue
		}
		at := recorded
		if available.After(recorded) { // recorded while held: it counts once the hold ended, or once placed early
			start := available
			if !released.IsZero() && released.Before(start) {
				start = released
			}
			if start.After(recorded) {
				at = start
			}
		}
		if !withdrawn.IsZero() && !withdrawn.After(at) {
			continue
		}
		if at.After(now) {
			pending = append(pending, held{src, recorded})
			continue
		}
		if best == "" || at.Before(bestAt) || at.Equal(bestAt) && src < best {
			best, bestAt = src, at
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, h := range pending {
		if h.src != best && !h.recorded.After(bestAt) {
			return "", nil
		}
	}
	return best, nil
}

// KeyOwnerOf is a key's owner for a name: the recorded one, else the first recorder, not recorded.
func (t *Tx) KeyOwnerOf(ctx context.Context, tenantID, packageType, name, kid string) (KeyOwner, error) {
	o, ok, err := t.recordedOwner(ctx, tenantID, packageType, name, kid)
	if err != nil || ok {
		return o, err
	}
	o.Source, err = t.firstRecorder(ctx, tenantID, packageType, name, kid, o.since)
	return o, err
}

// ClaimKeyOwner is KeyOwnerOf, recording a first recorder computed (by ActorLicenses), so it owns the
// key from then on whatever happens to its records; claimed reports that it recorded it now. A claim
// racing another one keeps the one that committed first.
func (t *Tx) ClaimKeyOwner(ctx context.Context, tenantID, packageType, name, kid string) (o KeyOwner, claimed bool, err error) {
	if o, err = t.KeyOwnerOf(ctx, tenantID, packageType, name, kid); err != nil || o.Recorded || o.Source == "" {
		return o, false, err
	}
	var n int64
	if o.since.IsZero() {
		n, err = t.putOwner(ctx, tenantID, packageType, name, kid, o.Source, ActorLicenses, false)
	} else { // over the removed owner's row
		var res sql.Result
		res, err = t.exec(ctx, `UPDATE license_key_owners SET source = ?, set_by = ?, set_at = ? WHERE tenant_id = ? AND package_type = ?
AND name = ? AND kid = ? AND source = ?`, o.Source, ActorLicenses, t.s.d.timeArg(t.Now()), tenantID, packageType, name, kid, ownerRemoved)
		if err == nil {
			n, err = res.RowsAffected()
		}
	}
	if err != nil {
		return o, false, err
	}
	if n == 1 {
		o.Recorded, o.SetBy, o.SetAt = true, ActorLicenses, t.Now()
		label, err := t.SourceLabel(ctx, o.Source)
		if err != nil {
			return o, false, err
		}
		f := map[string]any{"name": name, "kid": kid, "source": label, "first": true}
		if packageType != pkgtype.Default {
			f["package_type"] = packageType
		}
		return o, true, t.Event(ctx, tenantID, ActorLicenses, "license.key_owner", "license:"+name, f)
	}
	o, _, err = t.recordedOwner(ctx, tenantID, packageType, name, kid)
	return o, false, err
}

// SetKeyOwner gives a key of a name to a source, a tenant administrator's act (spec 0033 phase B),
// and returns its owner before. The source must have recorded the key for the name
// (ErrNotDeclared otherwise). The caller holds the tenant's licence lock, and moves the licences.
func (t *Tx) SetKeyOwner(ctx context.Context, tenantID, packageType, name, kid, source, actor string) (KeyOwner, error) {
	kind, id, ok := splitKeySource(source)
	if !ok {
		return KeyOwner{}, ErrNotDeclared
	}
	var n int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM build_license_keys k JOIN builds b ON b.id = k.build_id
WHERE k.tenant_id = ? AND k.kid = ? AND b.package_type = ? AND b.name = ? AND b.source = ? AND k.source_kind = ? AND k.source_id = ? AND `+
		sourceExists, tenantID, kid, packageType, name, source, kind, id).Scan(&n); err != nil {
		return KeyOwner{}, err
	}
	if n == 0 {
		return KeyOwner{}, ErrNotDeclared
	}
	before, err := t.KeyOwnerOf(ctx, tenantID, packageType, name, kid)
	if err != nil {
		return before, err
	}
	// an upsert: a first recorder's claim racing the act (an intake's, which takes no licence lock)
	// is overwritten by it
	_, err = t.putOwner(ctx, tenantID, packageType, name, kid, source, actor, true)
	return before, err
}

// MoveLicenses gives a tenant's licences of a package signed with a key to a source (an
// administrator's change of the key's owner), and returns them as they were: the owner before's
// (from; "" none), or, where it has none, the one of unknown source signed with the key (migration
// 0042 found no owner for it). A relayed licence (source upstream) moved to an upstream is that
// upstream's to revoke from then on (its upstream_id follows); an imported one stays imported, which
// no relay removes. The caller holds the tenant's licence lock, and has checked the target has none
// for the name.
func (t *Tx) MoveLicenses(ctx context.Context, tenantID, packageType, name, kid, from, to string) ([]License, error) {
	for _, src := range []string{from, LicenseUnknown} {
		if src == "" || src == to {
			continue
		}
		ls, err := t.licenses(ctx, "SELECT "+licenseCols+` FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? AND key_fp = ?
AND owner_source = ?`, tenantID, packageType, name, kid, src)
		if err != nil {
			return nil, err
		}
		if len(ls) == 0 {
			continue
		}
		set, args := "owner_source = ?", []any{to}
		if kind, id, _ := splitKeySource(to); kind == KeySourceUpstream {
			set += ", upstream_id = CASE WHEN source = 'upstream' THEN ? ELSE upstream_id END"
			args = append(args, id)
		}
		_, err = t.exec(ctx, "UPDATE licenses SET "+set+`, updated_at = ?, version = version + 1 WHERE tenant_id = ? AND package_type = ?
AND name = ? AND key_fp = ? AND owner_source = ?`, append(args, t.s.d.timeArg(t.Now()), tenantID, packageType, name, kid, src)...)
		return ls, t.s.mapErr(err, "licence "+name)
	}
	return nil, nil
}

// noteKeyOwner follows a key just recorded on a build by a source (spec 0033 phase B): the key's
// owner for the name is claimed if it is still to be computed (a license.key_owner event), and a
// source declaring a key another source owns is told to the tenant's administrators
// (license.key_conflict), the first time it declares it for the name.
func (t *Tx) noteKeyOwner(ctx context.Context, tenantID, buildID, src, kid string) error {
	var packageType, name string
	if err := t.queryRow(ctx, "SELECT package_type, name FROM builds WHERE id = ?", buildID).Scan(&packageType, &name); err != nil {
		return err
	}
	o, _, err := t.ClaimKeyOwner(ctx, tenantID, packageType, name, kid)
	if err != nil {
		return err
	}
	if o.Source == "" || o.Source == src {
		return nil
	}
	kind, id, _ := splitKeySource(src)
	var n int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM build_license_keys k JOIN builds b ON b.id = k.build_id
WHERE k.tenant_id = ? AND k.kid = ? AND b.package_type = ? AND b.name = ? AND k.source_kind = ? AND k.source_id = ? AND k.build_id <> ?`,
		tenantID, kid, packageType, name, kind, id, buildID).Scan(&n); err != nil || n > 0 {
		return err
	}
	label, err := t.SourceLabel(ctx, src)
	if err != nil {
		return err
	}
	owner, err := t.SourceLabel(ctx, o.Source)
	if err != nil {
		return err
	}
	f := map[string]any{"name": name, "kid": kid, "source": label, "owner": owner}
	if packageType != pkgtype.Default {
		f["package_type"] = packageType
	}
	return t.Event(ctx, tenantID, ActorLicenses, "license.key_conflict", "license:"+name, f)
}

// LiveLicenseKeys are the live keys of a package (type, name) of one source, upstream:<id> or
// publisher:<id> (spec 0021; spec 0033 phase B), those it recorded: declared by a live build (an
// active or deprecated release in a signed channel that serves the build's source for the name, its
// body not blocked, not withdrawn), or by the source's newest available version of the name in the
// catalogue (not held, withdrawn nor revoked, its body not blocked), so a licence is taken before
// any placement. Any other source (tenant, unknown) has none.
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

// NameKey is a live licence key of a tenant's package (LiveLicenseKeys' rule, any source), with the
// sources that brought it, the number of builds declaring it that count, and its owner for the name
// (spec 0033 phase B: upstream:<id> or publisher:<id>; "" none; OwnerRecorded false: the first
// recorder, not recorded yet).
type NameKey struct {
	Name, Kid     string
	Sources       []KeySource
	Builds        int
	Owner         string
	OwnerRecorded bool
}

// KeySource is a trusted source by kind and name.
type KeySource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

const keySourceName = "COALESCE(p.name, u.name, k.source_id)"

// TenantLicenseKeys lists the live keys of a tenant's packages of a type (of one name, or of all:
// name ""), by name and kid, with their owners: those its sources recorded on live builds, or on
// their newest available version of the name in the catalogue.
func (s *Store) TenantLicenseKeys(ctx context.Context, tenantID, packageType, name string) (out []NameKey, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		out, err = tx.tenantLicenseKeys(ctx, tenantID, packageType, name)
		return err
	})
	return out, err
}

func (t *Tx) tenantLicenseKeys(ctx context.Context, tenantID, packageType, name string) ([]NameKey, error) {
	byName := ""
	args := []any{tenantID, packageType, t.s.d.timeArg(t.Now())}
	if name != "" {
		byName = " AND b.name = ?"
		args = append(args, name)
	}
	// the newest available version of each (name, source) in the catalogue
	newest := map[[2]string]string{}
	rows, err := t.query(ctx, `SELECT b.name, b.source, b.package_version FROM builds b
WHERE b.tenant_id = ? AND b.package_type = ? AND b.withdrawn_at IS NULL AND b.revoked_at IS NULL AND (b.available_at IS NULL OR b.available_at <= ?)
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)`+byName+`
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
		live, avail                            bool
	}
	var recs []rec
	rows, err = t.query(ctx, `SELECT b.name, k.kid, k.build_id, b.source, b.package_version, k.source_kind, k.source_id, `+keySourceName+`,
CASE WHEN `+liveBuild+` THEN 1 ELSE 0 END,
CASE WHEN b.withdrawn_at IS NULL AND b.revoked_at IS NULL AND (b.available_at IS NULL OR b.available_at <= ?)
	AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash) THEN 1 ELSE 0 END
FROM build_license_keys k JOIN builds b ON b.id = k.build_id
LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
LEFT JOIN upstreams u ON k.source_kind = 'upstream' AND u.id = k.source_id
WHERE b.tenant_id = ? AND b.package_type = ? AND `+sourceExists+byName+`
ORDER BY b.name, k.kid, k.source_kind, `+keySourceName+`, k.build_id`, append([]any{args[2], args[0], args[1]}, args[3:]...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r rec
		var live, avail int
		if err := rows.Scan(&r.name, &r.kid, &r.build, &r.buildSource, &r.version, &r.src.Kind, &r.srcID, &r.src.Name, &live, &avail); err != nil {
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
	var builds []string
	for _, r := range recs {
		src := keySource(r.src.Kind, r.srcID)
		catalogue := r.avail && r.buildSource == src && newest[[2]string{r.name, src}] == r.version // the source's newest available version
		if !r.live && !catalogue {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Name != r.name || out[len(out)-1].Kid != r.kid {
			out = append(out, NameKey{Name: r.name, Kid: r.kid})
			builds = builds[:0]
		}
		last := &out[len(out)-1]
		if !slices.Contains(last.Sources, r.src) {
			last.Sources = append(last.Sources, r.src)
		}
		if !slices.Contains(builds, r.build) {
			builds = append(builds, r.build)
			last.Builds++
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	// the owners recorded, in one read; the others computed
	owners := map[[2]string]string{}
	q, qargs := "SELECT name, kid, source FROM license_key_owners WHERE tenant_id = ? AND package_type = ?", []any{tenantID, packageType}
	if name != "" {
		q, qargs = q+" AND name = ?", append(qargs, name)
	}
	rows, err = t.query(ctx, q, qargs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, kid, src string
		if err := rows.Scan(&n, &kid, &src); err != nil {
			rows.Close()
			return nil, err
		}
		owners[[2]string{n, kid}] = src
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if src, ok := owners[[2]string{out[i].Name, out[i].Kid}]; ok && src != ownerRemoved {
			out[i].Owner, out[i].OwnerRecorded = src, true
			continue
		}
		o, err := t.KeyOwnerOf(ctx, tenantID, packageType, out[i].Name, out[i].Kid)
		if err != nil {
			return nil, err
		}
		out[i].Owner, out[i].OwnerRecorded = o.Source, o.Recorded
	}
	return out, nil
}
