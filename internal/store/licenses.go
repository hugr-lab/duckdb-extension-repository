package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// How a licence arrived (spec 0012).
const (
	LicenseImport   = "import"   // an administrator's import
	LicenseUpstream = "upstream" // relayed from an upstream
)

// License is a tenant's licence for a package (spec 0012; spec 0034: per type and name) in a
// catalogue entry (spec 0035: Entry, the entry's source, upstream:<id> or publisher:<id>): its
// verified claims, and the token as received. Source is how it arrived.
type License struct {
	TenantID, PackageType, Name     string
	Entry                           string
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

const licenseCols = `tenant_id, package_type, name, entry_source, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at,
grace_until, source, upstream_id, visibility, actor, updated_at, version`

func scanLicense(r interface{ Scan(...any) error }) (License, error) {
	var l License
	var up sql.NullString
	err := r.Scan(&l.TenantID, &l.PackageType, &l.Name, &l.Entry, &l.LicenseID, &l.Issuer, &l.HolderID, &l.Holder, &l.Plan, &l.Token, &l.Key, &l.IssuedAt,
		&l.ExpiresAt, &l.GraceUntil, &l.Source, &up, &l.Visibility, &l.Actor, scanTime{&l.UpdatedAt}, &l.Version)
	l.UpstreamID = up.String
	return l, err
}

// GetLicense reads a tenant's licence for a package (type, name) in an entry (spec 0035).
func (t *Tx) GetLicense(ctx context.Context, tenantID, packageType, name, entry string) (License, error) {
	l, err := scanLicense(t.queryRow(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? AND entry_source = ?",
		tenantID, packageType, name, entry))
	return l, notFound(err, "licence "+name)
}

// GetLicense reads a tenant's licence for a package (type, name) in an entry, outside a transaction
// (the DuckDB routes: a channel serves its type's licences, of the entry it serves the name from).
func (s *Store) GetLicense(ctx context.Context, tenantID, packageType, name, entry string) (License, error) {
	l, err := scanLicense(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+licenseCols+
		" FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? AND entry_source = ?"), tenantID, packageType, name, entry))
	return l, notFound(err, "licence "+name)
}

// NameLicenses lists a tenant's licences of a package (type, name), one per entry, by entry.
func (t *Tx) NameLicenses(ctx context.Context, tenantID, packageType, name string) ([]License, error) {
	return t.licenses(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? ORDER BY entry_source",
		tenantID, packageType, name)
}

// LaterLicenses lists a tenant's licences of a package (type, name), in any entry, from issuer for
// holderID, issued later than issuedAt (spec 0037: what makes a licence older), by entry. It compares
// the issuer and the holder here, byte for byte: SQL Server's comparison ignores trailing spaces.
// Whether each still counts (its key, its entry, its grace) is the caller's to decide.
func (t *Tx) LaterLicenses(ctx context.Context, tenantID, packageType, name, issuer, holderID string, issuedAt int64) ([]License, error) {
	ls, err := t.licenses(ctx, "SELECT "+licenseCols+` FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? AND issued_at > ?
ORDER BY entry_source`, tenantID, packageType, name, issuedAt)
	if err != nil {
		return nil, err
	}
	out := ls[:0]
	for _, l := range ls {
		if l.Issuer == issuer && l.HolderID == holderID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (t *Tx) licenses(ctx context.Context, q string, args ...any) ([]License, error) {
	rows, err := t.query(ctx, q, args...)
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

// ListLicenses lists a tenant's licences of a type by name and entry.
func (s *Store) ListLicenses(ctx context.Context, tenantID, packageType string) ([]License, error) {
	rows, err := s.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND package_type = ? ORDER BY name, entry_source",
		tenantID, packageType)
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

// ErrHolderMismatch is a licence whose holder is not the one its tenant expects (spec 0028); the
// error is a *HolderMismatch.
var ErrHolderMismatch = errors.New("store: the licence names another holder than its tenant expects")

// HolderMismatch names the holder a tenant expects and the one a licence names.
type HolderMismatch struct{ Expected, Given string }

func (e *HolderMismatch) Error() string {
	return fmt.Sprintf("the tenant takes licences of holder %q only; this one is of %q", e.Expected, e.Given)
}

// Is makes a HolderMismatch an ErrHolderMismatch.
func (e *HolderMismatch) Is(target error) bool { return target == ErrHolderMismatch }

// ValidHolderID checks a tenant's expected holder: 1 to 128 bytes, as a licence's sub (spec 0028),
// of UTF-8 without U+0000 (PostgreSQL stores no NUL; elsewhere it would be a holder no licence
// matches). Other control characters stay, as a sub may carry them escaped.
func ValidHolderID(id string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("%w: a holder id is 1 to 128 bytes of UTF-8 without U+0000, as a licence's sub", ErrInvalid)
	}
	return nil
}

// CheckLicenseHolder refuses (a *HolderMismatch) a licence of holderID when its tenant expects
// another holder (spec 0028). It reads the tenant's row with a shared lock held until commit: a
// write later in the same transaction sees the holder as it is, since changing the holder waits
// for the transaction (SetTenantHolder updates the row, after taking the licence lock every
// licence write holds).
func (t *Tx) CheckLicenseHolder(ctx context.Context, tenantID, holderID string) error {
	var expected sql.NullString
	if err := t.queryRow(ctx, t.lockedTenantQuery("SELECT holder_id FROM tenants", "id = ?", false), tenantID).Scan(&expected); err != nil {
		return notFound(err, "tenant")
	}
	if expected.Valid && expected.String != holderID {
		return &HolderMismatch{Expected: expected.String, Given: holderID}
	}
	return nil
}

// OtherHolderLicenses lists a tenant's licences of a holder other than holderID, by name. It
// compares here, byte for byte, as CheckLicenseHolder does: SQL Server's comparison ignores
// trailing spaces.
func (t *Tx) OtherHolderLicenses(ctx context.Context, tenantID, holderID string) ([]License, error) {
	rows, err := t.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? ORDER BY name, entry_source", tenantID)
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
		if l.HolderID != holderID {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

// SetTenantHolder sets (holder "": clears) the holder a tenant's licences must name, if the
// tenant's version is still tn.Version and its deletion has not started (spec 0028). The caller
// holds the tenant's licence lock and has checked the licences stored (OtherHolderLicenses).
func (t *Tx) SetTenantHolder(ctx context.Context, tn *Tenant, holder string) error {
	if holder != "" {
		if err := ValidHolderID(holder); err != nil {
			return err
		}
	}
	return t.cas(ctx, "UPDATE tenants SET holder_id = ?, version = version + 1 WHERE id = ? AND version = ? AND deleting_at IS NULL",
		[]any{nullable(holder), tn.ID, tn.Version}, func() { tn.HolderID = holder; tn.Version++ })
}

// PutLicense stores a licence in its entry (spec 0035): a new row, or a replacement of the row at
// version expected (ErrConflict otherwise; 0 when there is none). The caller holds the tenant's
// licence lock and has verified the licence. A licence of another holder than its tenant expects is
// refused here, whatever the path (spec 0028: a *HolderMismatch, nothing written).
func (t *Tx) PutLicense(ctx context.Context, l *License, expected int64) error {
	if err := t.CheckLicenseHolder(ctx, l.TenantID, l.HolderID); err != nil {
		return err
	}
	if l.PackageType == "" {
		l.PackageType = pkgtype.Default
	}
	if !pkgtype.Known(l.PackageType) {
		return fmt.Errorf("%w: package type %q", ErrInvalid, l.PackageType)
	}
	if !ValidLicenseEntry(l.Entry) {
		return fmt.Errorf("%w: a licence's entry %q", ErrInvalid, l.Entry)
	}
	l.UpdatedAt = t.Now()
	if expected == 0 {
		l.Version = 1
		_, err := t.exec(ctx, "INSERT INTO licenses ("+licenseCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			l.TenantID, l.PackageType, l.Name, l.Entry, l.LicenseID, l.Issuer, l.HolderID, l.Holder, l.Plan, l.Token, l.Key, l.IssuedAt, l.ExpiresAt, l.GraceUntil,
			l.Source, nullable(l.UpstreamID), l.Visibility, l.Actor, t.s.d.timeArg(l.UpdatedAt), l.Version)
		if err := t.s.mapErr(err, "licence "+l.Name); errors.Is(err, ErrExists) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		t.warnLicense(l)
		return nil
	}
	err := t.cas(ctx, `UPDATE licenses SET license_id = ?, issuer = ?, holder_id = ?, holder = ?, plan_name = ?, token = ?, key_fp = ?,
issued_at = ?, expires_at = ?, grace_until = ?, source = ?, upstream_id = ?, visibility = ?, actor = ?, updated_at = ?, version = version + 1
WHERE tenant_id = ? AND package_type = ? AND name = ? AND entry_source = ? AND version = ?`,
		[]any{l.LicenseID, l.Issuer, l.HolderID, l.Holder, l.Plan, l.Token, l.Key, l.IssuedAt, l.ExpiresAt, l.GraceUntil, l.Source,
			nullable(l.UpstreamID), l.Visibility, l.Actor, t.s.d.timeArg(l.UpdatedAt), l.TenantID, l.PackageType, l.Name, l.Entry, expected},
		func() { l.Version = expected + 1 })
	if err == nil {
		t.warnLicense(l)
	}
	return err
}

// warnLicense notes a licence stored (spec 0035 phase 2): its entry warns when it is served
// publicly.
func (t *Tx) warnLicense(l *License) {
	act := "license.import"
	if l.Source == LicenseUpstream {
		act = "license.sync"
	}
	t.warnEntry(l, act)
}

// SetLicenseVisibility changes a licence's visibility (compare-and-set on its version).
func (t *Tx) SetLicenseVisibility(ctx context.Context, l *License, visibility, actor string, expected int64) error {
	now := t.Now()
	err := t.cas(ctx, `UPDATE licenses SET visibility = ?, actor = ?, updated_at = ?, version = version + 1
WHERE tenant_id = ? AND package_type = ? AND name = ? AND entry_source = ? AND version = ?`,
		[]any{visibility, actor, t.s.d.timeArg(now), l.TenantID, l.PackageType, l.Name, l.Entry, expected},
		func() { l.Visibility, l.Actor, l.UpdatedAt, l.Version = visibility, actor, now, expected+1 })
	if err == nil && visibility == Public { // spec 0035 phase 2: a public licence in an entry served publicly warns
		t.warnEntry(l, "license.change")
	}
	return err
}

// DeleteLicense removes a licence (compare-and-set on its version; 0 skips the check).
func (t *Tx) DeleteLicense(ctx context.Context, l License, expected int64) error {
	if expected == 0 {
		expected = l.Version
	}
	return t.cas(ctx, "DELETE FROM licenses WHERE tenant_id = ? AND package_type = ? AND name = ? AND entry_source = ? AND version = ?",
		[]any{l.TenantID, l.PackageType, l.Name, l.Entry, expected}, func() {})
}

// UpstreamLicenses lists the licences an upstream of a tenant governs (spec 0035): those relayed
// (source upstream) in its own entry. Neither the relay's revocation nor the upstream's removal
// takes an import, nor another entry's copy.
func (t *Tx) UpstreamLicenses(ctx context.Context, tenantID, upstreamID string) ([]License, error) {
	rows, err := t.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE tenant_id = ? AND source = ? AND entry_source = ? ORDER BY name",
		tenantID, LicenseUpstream, UpstreamSource(upstreamID))
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

// ExpiredLicenses lists, across tenants, the licences whose grace ended before now (seconds), of the
// types this binary knows.
func (s *Store) ExpiredLicenses(ctx context.Context, now int64) ([]License, error) {
	rows, err := s.query(ctx, "SELECT "+licenseCols+" FROM licenses WHERE grace_until <= ? ORDER BY tenant_id, package_type, name, entry_source", now)
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
		if pkgtype.Known(l.PackageType) {
			out = append(out, l)
		}
	}
	return out, rows.Err()
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
