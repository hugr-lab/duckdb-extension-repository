package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Spec 0030: the names DuckDB owns are the server's data. A name is reserved as core or community,
// with the sources that reserved it; the list only grows (a community name may become core), and
// reserved_version counts its changes so every replica's snapshots follow.

// Kinds of reserved names.
const (
	ReservedCore      = "core"
	ReservedCommunity = "community"
)

// ReservedName is a reserved name and why it is reserved.
type ReservedName struct {
	Name    string
	Kind    string
	AddedAt time.Time
	Sources []string
}

var reservedSourceRE = regexp.MustCompile(`^(builtin|(version|upstream|bundle|admin):[\x21-\x7e]{1,380}|community-sync@[0-9a-f]{7,64})$`)

// ValidReservedSource checks a source's form: builtin, version:<v>[@<commit>], upstream:<tenant>/<id>,
// bundle:<id>, community-sync@<commit> or admin:<principal>.
func ValidReservedSource(s string) error {
	if !reservedSourceRE.MatchString(s) {
		return fmt.Errorf("%w: reserved name source %q", ErrInvalid, s)
	}
	return nil
}

// ReservedNames reads every reserved name's kind and the list's version, read together.
func (s *Store) ReservedNames(ctx context.Context) (map[string]string, int64, error) {
	var names map[string]string
	var version int64
	err := s.tx(ctx, nil, func(tx *Tx) error {
		names, version = map[string]string{}, 0
		if err := tx.queryRow(ctx, "SELECT version FROM reserved_version WHERE id = 1").Scan(&version); err != nil {
			return err
		}
		rows, err := tx.query(ctx, "SELECT name, kind FROM reserved_names")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n, k string
			if err := rows.Scan(&n, &k); err != nil {
				return err
			}
			names[n] = k
		}
		return rows.Err()
	})
	return names, version, err
}

// ReservedVersion reads the list's version: a change of it means the list changed.
func (s *Store) ReservedVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, "SELECT version FROM reserved_version WHERE id = 1").Scan(&v)
	return v, err
}

// ReservedKind reads a name's kind inside the transaction ("" for a name not reserved): the check a
// release insert makes again under its locks (spec 0030).
func (t *Tx) ReservedKind(ctx context.Context, name string) (string, error) {
	var k string
	err := t.queryRow(ctx, "SELECT kind FROM reserved_names WHERE name = ?", name).Scan(&k)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return k, err
}

// ReserveName reserves a name as kind with a source: inserted if absent (core always wins: a
// community name becomes core, never the other way), and the source recorded. It says whether the
// name or its kind changed; the caller then bumps the list's version once, as its transaction's last
// write to the list (BumpReservedVersion), so that names are always locked before the version.
// A name already reserved as kind (or as core) with the source is only read: an intake of a known
// name takes no lock on it.
func (t *Tx) ReserveName(ctx context.Context, name, kind, source string) (bool, error) {
	if kind != ReservedCore && kind != ReservedCommunity {
		return false, fmt.Errorf("%w: reserved name kind %q", ErrInvalid, kind)
	}
	if err := ValidReservedSource(source); err != nil {
		return false, err
	}
	cur, err := t.ReservedKind(ctx, name)
	if err != nil {
		return false, err
	}
	var hasSource bool
	if cur != "" {
		var n int
		if err := t.queryRow(ctx, "SELECT COUNT(*) FROM reserved_name_sources WHERE name = ? AND source = ?", name, source).Scan(&n); err != nil {
			return false, err
		}
		hasSource = n > 0
	}
	if (cur == kind || cur == ReservedCore) && hasSource {
		return false, nil
	}
	now := t.s.d.timeArg(t.Now())
	changed := false
	if cur == "" {
		var inserted int64
		if t.s.d.Name == "sqlserver" {
			res, err := t.exec(ctx, `MERGE reserved_names WITH (HOLDLOCK) AS r USING (SELECT ? AS name) AS n ON r.name = n.name
WHEN NOT MATCHED THEN INSERT (name, kind, added_at) VALUES (?, ?, ?);`, name, name, kind, now)
			if err != nil {
				return false, err
			}
			inserted, _ = res.RowsAffected()
		} else {
			res, err := t.exec(ctx, "INSERT INTO reserved_names (name, kind, added_at) VALUES (?, ?, ?) ON CONFLICT (name) DO NOTHING", name, kind, now)
			if err != nil {
				return false, err
			}
			inserted, _ = res.RowsAffected()
		}
		changed = inserted > 0
	}
	if !changed && kind == ReservedCore && cur != ReservedCore {
		res, err := t.exec(ctx, "UPDATE reserved_names SET kind = ? WHERE name = ? AND kind = ?", ReservedCore, name, ReservedCommunity)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
	}
	if !hasSource {
		if t.s.d.Name == "sqlserver" {
			_, err := t.exec(ctx, `MERGE reserved_name_sources WITH (HOLDLOCK) AS r USING (SELECT ? AS name, ? AS source) AS n
ON r.name = n.name AND r.source = n.source WHEN NOT MATCHED THEN INSERT (name, source, added_at) VALUES (?, ?, ?);`,
				name, source, name, source, now)
			if err != nil {
				return false, err
			}
		} else if _, err := t.exec(ctx, "INSERT INTO reserved_name_sources (name, source, added_at) VALUES (?, ?, ?) ON CONFLICT (name, source) DO NOTHING",
			name, source, now); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// BumpReservedVersion records that the list changed: every replica's view and snapshots follow.
func (t *Tx) BumpReservedVersion(ctx context.Context) error {
	_, err := t.exec(ctx, "UPDATE reserved_version SET version = version + 1 WHERE id = 1")
	return err
}

// ListReservedNames lists the reserved names with their sources, by name (one name, or all when name
// is "").
func (s *Store) ListReservedNames(ctx context.Context, name string) ([]ReservedName, error) {
	q, args := "SELECT r.name, r.kind, r.added_at, s.source FROM reserved_names r JOIN reserved_name_sources s ON s.name = r.name", []any{}
	if name != "" {
		q, args = q+" WHERE r.name = ?", append(args, name)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q+" ORDER BY r.name, s.source"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReservedName
	for rows.Next() {
		var r ReservedName
		var src string
		if err := rows.Scan(&r.Name, &r.Kind, scanTime{&r.AddedAt}, &src); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Name == r.Name {
			out[n-1].Sources = append(out[n-1].Sources, src)
			continue
		}
		r.Sources = []string{src}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TenantsWithReleases lists the tenants with releases under a name that are not yanked or purged, by name:
// the tenants a reservation of the name would leave holding them.
func (s *Store) TenantsWithReleases(ctx context.Context, name string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT DISTINCT t.name FROM releases r JOIN tenants t ON t.id = r.tenant_id
WHERE r.name = ? AND r.state NOT IN ('yanked', 'purged') ORDER BY t.name`), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// RemoveReservedName removes a name an administrator alone reserved (every source admin:) and
// returns its sources; a name DuckDB brought stays (ErrConflict). Only admin: sources are deleted, and
// the name only when no other source is left, so a source another transaction commits meanwhile
// keeps the name.
func (t *Tx) RemoveReservedName(ctx context.Context, name string) ([]string, error) {
	rows, err := t.query(ctx, "SELECT source FROM reserved_name_sources WHERE name = ?", name)
	if err != nil {
		return nil, err
	}
	var sources []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: reserved name %s", ErrNotFound, name)
	}
	conflict := func(s string) error {
		return fmt.Errorf("%w: %s is reserved by %s, not by an administrator alone: it stays", ErrConflict, name, s)
	}
	for _, s := range sources {
		if !strings.HasPrefix(s, "admin:") {
			return nil, conflict(s)
		}
	}
	if _, err := t.exec(ctx, "DELETE FROM reserved_name_sources WHERE name = ? AND source LIKE 'admin:%'", name); err != nil {
		return nil, err
	}
	res, err := t.exec(ctx, "DELETE FROM reserved_names WHERE name = ? AND NOT EXISTS (SELECT 1 FROM reserved_name_sources s WHERE s.name = ?)", name, name)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, conflict("a source added meanwhile") // the caller's transaction rolls the deletion back
	}
	return sources, t.BumpReservedVersion(ctx)
}

// --- names reserved in a tenant (spec 0030 part 2) ---

// TenantReservedName is a name reserved in a tenant, for a holder (who holds it: free text).
type TenantReservedName struct {
	TenantID, Name, Holder, AddedBy string
	AddedAt                         time.Time
}

// MaxHolder bounds a holder's text.
const MaxHolder = 128

// ReserveTenantName reserves a name in a tenant, or changes its holder; it returns the holder before
// ("" for a new name) and whether anything changed.
func (t *Tx) ReserveTenantName(ctx context.Context, r *TenantReservedName) (string, bool, error) {
	if r.Holder == "" || len(r.Holder) > MaxHolder || !utf8.ValidString(r.Holder) || strings.IndexFunc(r.Holder, func(c rune) bool { return !unicode.IsPrint(c) }) >= 0 {
		return "", false, fmt.Errorf("%w: holder: 1 to %d bytes of printable text", ErrInvalid, MaxHolder)
	}
	var cur string
	err := t.queryRow(ctx, "SELECT holder FROM tenant_reserved_names WHERE tenant_id = ? AND name = ?", r.TenantID, r.Name).Scan(&cur)
	switch {
	case err == sql.ErrNoRows:
		r.AddedAt = t.Now()
		_, err = t.exec(ctx, "INSERT INTO tenant_reserved_names (tenant_id, name, holder, added_at, added_by) VALUES (?, ?, ?, ?, ?)",
			r.TenantID, r.Name, r.Holder, t.s.d.timeArg(r.AddedAt), r.AddedBy)
		if err != nil {
			return "", false, t.s.mapErr(err, "the tenant's reserved name "+r.Name)
		}
		return "", true, nil
	case err != nil:
		return "", false, err
	case cur == r.Holder:
		return cur, false, nil
	}
	_, err = t.exec(ctx, "UPDATE tenant_reserved_names SET holder = ? WHERE tenant_id = ? AND name = ?", r.Holder, r.TenantID, r.Name)
	return cur, true, err
}

// ReleaseTenantName removes a name's reservation in a tenant and returns it (ErrNotFound when none).
func (t *Tx) ReleaseTenantName(ctx context.Context, tenantID, name string) (TenantReservedName, error) {
	r := TenantReservedName{TenantID: tenantID, Name: name}
	err := t.queryRow(ctx, "SELECT holder, added_by, added_at FROM tenant_reserved_names WHERE tenant_id = ? AND name = ?", tenantID, name).
		Scan(&r.Holder, &r.AddedBy, scanTime{&r.AddedAt})
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("%w: %s is not reserved in the tenant", ErrNotFound, name)
	}
	if err != nil {
		return r, err
	}
	_, err = t.exec(ctx, "DELETE FROM tenant_reserved_names WHERE tenant_id = ? AND name = ?", tenantID, name)
	return r, err
}

// TenantReserves reports whether a name is reserved in the tenant, inside the transaction: the check
// a release insert makes again under its locks.
func (t *Tx) TenantReserves(ctx context.Context, tenantID, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT COUNT(*) FROM tenant_reserved_names WHERE tenant_id = ? AND name = ?", tenantID, name).Scan(&n)
	return n > 0, err
}

// ListTenantReservedNames lists a tenant's reserved names, by name (one name, or all when name is "").
func (s *Store) ListTenantReservedNames(ctx context.Context, tenantID, name string) ([]TenantReservedName, error) {
	q, args := "SELECT name, holder, added_by, added_at FROM tenant_reserved_names WHERE tenant_id = ?", []any{tenantID}
	if name != "" {
		q, args = q+" AND name = ?", append(args, name)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q+" ORDER BY name"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantReservedName
	for rows.Next() {
		r := TenantReservedName{TenantID: tenantID}
		if err := rows.Scan(&r.Name, &r.Holder, &r.AddedBy, scanTime{&r.AddedAt}); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TenantReservedSet is the names reserved in a tenant.
func (s *Store) TenantReservedSet(ctx context.Context, tenantID string) (map[string]bool, error) {
	rs, err := s.ListTenantReservedNames(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rs))
	for _, r := range rs {
		out[r.Name] = true
	}
	return out, nil
}

// LiveRelease is a release under a name, as a reservation's answer names it.
type LiveRelease struct {
	Channel, Version, Platform, State, Origin string
}

// TenantLiveReleases lists a tenant's releases under a name that are not yanked or purged.
func (s *Store) TenantLiveReleases(ctx context.Context, tenantID, name string) ([]LiveRelease, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT c.name, r.package_version, r.platform, r.state, b.origin
FROM releases r JOIN channels c ON c.id = r.channel_id JOIN builds b ON b.id = r.build_id
WHERE r.tenant_id = ? AND r.name = ? AND r.state NOT IN ('yanked', 'purged') ORDER BY c.name, r.package_version, r.platform`), tenantID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveRelease
	for rows.Next() {
		var r LiveRelease
		if err := rows.Scan(&r.Channel, &r.Version, &r.Platform, &r.State, &r.Origin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reservation kinds a lookup answers beyond DuckDB's (spec 0030): a name reserved in the tenant, and
// one an upstream of the tenant provides (spec 0009).
const (
	ReservedTenant   = "tenant"
	ReservedUpstream = "upstream"
)

// ReservedFor is the one lookup of a name's reservation for a tenant (by name), in precedence: core
// or community (the server's list, the process's view synced first), tenant, upstream; "" for a name
// free in the tenant. For publish and promote, a reserved name is reached only by grants naming it.
func (s *Store) ReservedFor(ctx context.Context, tenant, name string) (string, error) {
	if err := reserved.Sync(ctx, s); err != nil {
		return "", err
	}
	if k := reserved.Kind(name); k != "" {
		return k, nil
	}
	var tenantN, upstreamN int
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT
(SELECT COUNT(*) FROM tenant_reserved_names r JOIN tenants t ON t.id = r.tenant_id WHERE t.name = ? AND r.name = ?),
(SELECT COUNT(*) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id JOIN tenants t ON t.id = u.tenant_id WHERE t.name = ? AND e.name = ?)`),
		tenant, name, tenant, name).Scan(&tenantN, &upstreamN)
	switch {
	case err != nil:
		return "", err
	case tenantN > 0:
		return ReservedTenant, nil
	case upstreamN > 0:
		return ReservedUpstream, nil
	}
	return "", nil
}

// BumpTenantChannels bumps every channel of a tenant, in id order: their snapshots follow a change
// of what the tenant reserves.
func (t *Tx) BumpTenantChannels(ctx context.Context, tenantID string) error {
	return t.bumpBuildChannels(ctx, "SELECT id FROM channels WHERE tenant_id = ? ORDER BY id", tenantID)
}
