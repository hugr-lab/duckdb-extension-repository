package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Licence sources and visibilities (spec 0012).
const (
	LicenseImport   = "import"   // an administrator's import
	LicenseUpstream = "upstream" // relayed from an upstream
)

// License is a tenant's licence for an extension name (spec 0012): its verified claims, and the
// token as received.
type License struct {
	TenantID, Name                  string
	LicenseID, Issuer               string
	HolderID, Holder, Plan          string
	Token, Key                      string
	IssuedAt, ExpiresAt, GraceUntil int64 // seconds since the epoch
	Source, UpstreamID              string
	Visibility                      string // Private or Public
	Actor                           string
	UpdatedAt                       time.Time
	Version                         int64
}

// LicenseLock orders the changes of a tenant's licences.
func LicenseLock(tenantID string) string { return "kista/licenses/" + tenantID }

// LicenseSyncLock orders the writes of an upstream's relay state (a request and a run's end may
// both create its row).
func LicenseSyncLock(upstreamID string) string { return "kista/license-sync/" + upstreamID }

// LicenseRelayLease is the lease an upstream's relay run holds (spec 0012).
func LicenseRelayLease(upstreamID string) string { return "kista/licenses/upstream/" + upstreamID }

const licenseCols = `tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at,
grace_until, source, upstream_id, visibility, actor, updated_at, version`

func scanLicense(r interface{ Scan(...any) error }) (License, error) {
	var l License
	var up sql.NullString
	err := r.Scan(&l.TenantID, &l.Name, &l.LicenseID, &l.Issuer, &l.HolderID, &l.Holder, &l.Plan, &l.Token, &l.Key, &l.IssuedAt,
		&l.ExpiresAt, &l.GraceUntil, &l.Source, &up, &l.Visibility, &l.Actor, scanTime{&l.UpdatedAt}, &l.Version)
	l.UpstreamID = up.String
	return l, err
}

// GetLicense reads a tenant's licence for a name.
func (t *Tx) GetLicense(ctx context.Context, tenantID, name string) (License, error) {
	l, err := scanLicense(t.queryRow(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND name = ?", tenantID, name))
	return l, notFound(err, "licence "+name)
}

// GetLicense reads a tenant's licence for a name, outside a transaction (the DuckDB routes).
func (s *Store) GetLicense(ctx context.Context, tenantID, name string) (License, error) {
	l, err := scanLicense(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND name = ?"), tenantID, name))
	return l, notFound(err, "licence "+name)
}

// ListLicenses lists a tenant's licences by name.
func (s *Store) ListLicenses(ctx context.Context, tenantID string) ([]License, error) {
	rows, err := s.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? ORDER BY name", tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []License
	for rows.Next() {
		l, err := scanLicense(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PutLicense stores a licence for its name: a new row, or a replacement of the row at version
// expected (ErrConflict otherwise; 0 when there is none). The caller holds the tenant's licence
// lock and has verified the licence.
func (t *Tx) PutLicense(ctx context.Context, l *License, expected int64) error {
	l.UpdatedAt = t.Now()
	if expected == 0 {
		l.Version = 1
		_, err := t.exec(ctx, "INSERT INTO licenses ("+licenseCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			l.TenantID, l.Name, l.LicenseID, l.Issuer, l.HolderID, l.Holder, l.Plan, l.Token, l.Key, l.IssuedAt, l.ExpiresAt, l.GraceUntil,
			l.Source, nullable(l.UpstreamID), l.Visibility, l.Actor, t.s.d.timeArg(l.UpdatedAt), l.Version)
		if err := t.s.mapErr(err, "licence "+l.Name); errors.Is(err, ErrExists) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		return nil
	}
	return t.cas(ctx, `UPDATE licenses SET license_id = ?, issuer = ?, holder_id = ?, holder = ?, plan_name = ?, token = ?, key_fp = ?,
issued_at = ?, expires_at = ?, grace_until = ?, source = ?, upstream_id = ?, visibility = ?, actor = ?, updated_at = ?, version = version + 1
WHERE tenant_id = ? AND name = ? AND version = ?`,
		[]any{l.LicenseID, l.Issuer, l.HolderID, l.Holder, l.Plan, l.Token, l.Key, l.IssuedAt, l.ExpiresAt, l.GraceUntil, l.Source,
			nullable(l.UpstreamID), l.Visibility, l.Actor, t.s.d.timeArg(l.UpdatedAt), l.TenantID, l.Name, expected},
		func() { l.Version = expected + 1 })
}

// SetLicenseVisibility changes a licence's visibility (compare-and-set on its version).
func (t *Tx) SetLicenseVisibility(ctx context.Context, l *License, visibility, actor string, expected int64) error {
	now := t.Now()
	return t.cas(ctx, "UPDATE licenses SET visibility = ?, actor = ?, updated_at = ?, version = version + 1 WHERE tenant_id = ? AND name = ? AND version = ?",
		[]any{visibility, actor, t.s.d.timeArg(now), l.TenantID, l.Name, expected},
		func() { l.Visibility, l.Actor, l.UpdatedAt, l.Version = visibility, actor, now, expected+1 })
}

// DeleteLicense removes a licence (compare-and-set on its version; 0 skips the check).
func (t *Tx) DeleteLicense(ctx context.Context, l License, expected int64) error {
	if expected == 0 {
		expected = l.Version
	}
	return t.cas(ctx, "DELETE FROM licenses WHERE tenant_id = ? AND name = ? AND version = ?", []any{l.TenantID, l.Name, expected}, func() {})
}

// UpstreamLicenses lists the licences an upstream relayed.
func (t *Tx) UpstreamLicenses(ctx context.Context, upstreamID string) ([]License, error) {
	rows, err := t.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE upstream_id = ? ORDER BY name", upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []License
	for rows.Next() {
		l, err := scanLicense(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ExpiredLicenses lists, across tenants, the licences whose grace ended before now (seconds).
func (s *Store) ExpiredLicenses(ctx context.Context, now int64) ([]License, error) {
	rows, err := s.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE grace_until <= ? ORDER BY tenant_id, name", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []License
	for rows.Next() {
		l, err := scanLicense(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SuppliedBy reports whether an upstream supplies name to its tenant (spec 0012: an upstream hands
// out licences only for names it supplies): a live release in the tenant, in any channel, of a
// build the upstream brought into its channel since it was added (a promotion releases the same
// build; an older upstream of the same name brought nothing for this one).
func (t *Tx) SuppliedBy(ctx context.Context, u Upstream, name string) (bool, error) {
	rows, err := t.query(ctx, `SELECT r.build_id, r.provenance FROM releases r
WHERE r.channel_id = ? AND r.name = ? AND r.origin = ? AND r.created_at >= ?`, u.ChannelID, name, OriginUpstream, t.s.d.timeArg(u.CreatedAt))
	if err != nil {
		return false, err
	}
	var builds []string
	for rows.Next() {
		var id string
		var prov sql.NullString
		if err := rows.Scan(&id, &prov); err != nil {
			rows.Close()
			return false, err
		}
		var p struct {
			Upstream string `json:"upstream"`
		}
		if json.Unmarshal([]byte(prov.String), &p) == nil && p.Upstream == u.Name {
			builds = append(builds, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, b := range builds {
		var n int
		if err := t.queryRow(ctx, `SELECT COUNT(*) FROM releases r JOIN channels c ON c.id = r.channel_id
WHERE c.tenant_id = ? AND r.build_id = ? AND r.state IN (?, ?)`, u.TenantID, b, ReleaseActive, ReleaseDeprecated).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// LicenseSync is an upstream's licence relay state.
type LicenseSync struct {
	UpstreamID, TenantID   string
	RequestedAt, LastRunAt time.Time
	LastResult             string // JSON
}

// GetLicenseSync reads an upstream's relay state (zero LastRunAt: never run).
func (s *Store) GetLicenseSync(ctx context.Context, upstreamID string) (LicenseSync, error) {
	ls := LicenseSync{UpstreamID: upstreamID}
	var res sql.NullString
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT tenant_id, requested_at, last_run_at, last_result FROM license_syncs WHERE upstream_id = ?"),
		upstreamID).Scan(&ls.TenantID, scanTime{&ls.RequestedAt}, scanTime{&ls.LastRunAt}, &res)
	if errors.Is(err, sql.ErrNoRows) {
		return ls, nil
	}
	ls.LastResult = res.String
	return ls, err
}

// RequestLicenseSync asks for a relay run of an upstream now.
func (t *Tx) RequestLicenseSync(ctx context.Context, tenantID, upstreamID string) error {
	now := t.s.d.timeArg(t.Now())
	res, err := t.exec(ctx, "UPDATE license_syncs SET requested_at = ? WHERE upstream_id = ?", now, upstreamID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO license_syncs (upstream_id, tenant_id, requested_at) VALUES (?, ?, ?)", upstreamID, tenantID, now)
	return t.s.mapErr(err, "licence sync")
}

const dueLicenseSyncs = `SELECT u.id FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
LEFT JOIN license_syncs l ON l.upstream_id = u.id
WHERE u.kind = 'repository' AND u.mode <> 'bundle' AND u.state = 'active' AND u.credential IS NOT NULL
AND t.state = 'active' AND t.deleting_at IS NULL
AND (l.requested_at IS NOT NULL OR l.last_run_at IS NULL OR l.last_run_at <= ?)`

// DueLicenseSyncs lists the upstreams whose relay is due: a repository upstream with a credential,
// active, of an active tenant, whose run was requested or whose last run is older than interval
// (or never ran).
func (s *Store) DueLicenseSyncs(ctx context.Context, now time.Time, interval time.Duration) ([]string, error) {
	rows, err := s.query(ctx, dueLicenseSyncs+" ORDER BY u.id", s.d.timeArg(now.Add(-interval)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LicenseSyncDue reports whether one upstream's relay is due (DueLicenseSyncs' rule): a run checks
// again once it holds the lease, so a run another replica just finished is not repeated.
func (s *Store) LicenseSyncDue(ctx context.Context, upstreamID string, now time.Time, interval time.Duration) (bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.d.rebind(dueLicenseSyncs+" AND u.id = ?"), s.d.timeArg(now.Add(-interval)), upstreamID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// FinishLicenseSync records a relay run's end: its result, and a request made before it started
// cleared.
func (t *Tx) FinishLicenseSync(ctx context.Context, tenantID, upstreamID, result string, started time.Time) error {
	now := t.s.d.timeArg(t.Now())
	res, err := t.exec(ctx, `UPDATE license_syncs SET last_run_at = ?, last_result = ?,
requested_at = CASE WHEN requested_at <= ? THEN NULL ELSE requested_at END WHERE upstream_id = ?`,
		now, result, t.s.d.timeArg(started), upstreamID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO license_syncs (upstream_id, tenant_id, last_run_at, last_result) VALUES (?, ?, ?, ?)",
		upstreamID, tenantID, now, result)
	return t.s.mapErr(err, "licence sync")
}
