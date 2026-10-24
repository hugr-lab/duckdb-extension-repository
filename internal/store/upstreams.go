package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Upstream kinds, modes, states and cell outcomes (spec 0009).
const (
	UpstreamCore      = "duckdb-core"
	UpstreamCommunity = "duckdb-community"
	UpstreamRepo      = "repository"

	ModeMirror      = "mirror"
	ModePullThrough = "pull-through"
	ModeBundle      = "bundle" // filled only by bundle imports; never contacts its source (spec 0014)

	// a mirror's fetch mode (spec 0032): its whole matrix at every run, or only the cells asked for
	// and its eager entries
	FetchEager = "eager"
	FetchLazy  = "lazy"

	UpstreamActive = "active"

	// an upstream's usage (spec 0012 phase 1c): forwarded daily, or not
	UsageForward   = "forward"
	UsageOff       = "off"
	UpstreamPaused = "paused"

	OriginUpstream = "upstream"

	CellReleased  = "released"
	CellUnchanged = "unchanged"
	CellYanked    = "yanked"
	CellConflict  = "conflict"
	CellMissing   = "missing"
	CellRejected  = "rejected"
	CellBlocked   = "blocked"
	CellShadowed  = "shadowed"
	// CellOtherSource: another source holds the name in the channel (spec 0031), decided before the
	// download and asked again each run.
	CellOtherSource = "other_source"
	// CellLanded: the build is in the catalogue, placed nowhere: the channel's rule for the name is
	// manual and holds no release of its version (spec 0033).
	CellLanded = "landed"
	CellFailed = "failed" // the fetch failed (network, size, rate): retried by the next run
)

// Limits (spec 0009).
const (
	MaxUpstreams       = 100
	MaxUpstreamEntries = 1000
	MaxEntryVersions   = 100
	MaxUpstreamKeys    = 10
	MaxUpstreamCells   = 20000
	MaxTenantCells     = 100000
)

// Upstream is a source a tenant mirrors from into one channel.
type Upstream struct {
	ID, TenantID, Name, Kind, Prefix, ChannelID string
	Mode, Visibility, State                     string
	Version                                     int64
	RequestedAt                                 time.Time // zero: no run requested
	RequestDryRun                               bool
	NextRunAt, LastRunAt                        time.Time
	LastRun                                     string // JSON
	CreatedAt                                   time.Time
	CreatedBy                                   string
	Credential                                  string // a configured credential's name (spec 0009 phase 3); "": none
	Usage                                       string // UsageForward or UsageOff (spec 0012 phase 1c)
	FetchMode                                   string // FetchEager or FetchLazy (spec 0032)

	// bundle mode's freshness mark (spec 0014): the last imported manifest
	BundleID, BundleManifestSHA256 string
	BundleCreatedAt                time.Time

	Keys      []string // fingerprints
	Platforms []string
	Entries   []UpstreamEntry
}

// UpstreamEntry is an allowlist entry: a name and the extension versions accepted (none: any).
type UpstreamEntry struct {
	Name          string
	AllowReserved bool
	Versions      []string
	Eager         bool // a lazy upstream's runs fetch its cells unasked (spec 0032)
}

// UpstreamCell is the last outcome of one cell of an upstream's matrix.
type UpstreamCell struct {
	UpstreamID, DuckDBVersion, Platform, Name string
	ETag, BodyHash, Outcome, Detail           string
	FetchedAt                                 time.Time
}

// TenantUpstreamLock is the lock that orders reservation changes (allowlists) against releases of
// other origins in a tenant (spec 0009); it is taken before any channel lock.
func TenantUpstreamLock(tenantID string) string { return "kista/tenant-upstreams/" + tenantID }

const upstreamCols = `id, tenant_id, name, kind, prefix, channel_id, mode, visibility, state, version, requested_at,
request_dry_run, next_run_at, last_run_at, last_run, created_at, created_by, credential, bundle_id, bundle_created_at,
bundle_manifest_sha256, usage, fetch_mode`

func scanUpstream(r interface{ Scan(...any) error }) (Upstream, error) {
	var u Upstream
	var prefix, lastRun, cred, bundleID, bundleSHA sql.NullString
	err := r.Scan(&u.ID, &u.TenantID, &u.Name, &u.Kind, &prefix, &u.ChannelID, &u.Mode, &u.Visibility, &u.State, &u.Version,
		scanTime{&u.RequestedAt}, &u.RequestDryRun, scanTime{&u.NextRunAt}, scanTime{&u.LastRunAt}, &lastRun,
		scanTime{&u.CreatedAt}, &u.CreatedBy, &cred, &bundleID, scanTime{&u.BundleCreatedAt}, &bundleSHA, &u.Usage, &u.FetchMode)
	u.Prefix, u.LastRun, u.Credential = prefix.String, lastRun.String, cred.String
	u.BundleID, u.BundleManifestSHA256 = bundleID.String, bundleSHA.String
	return u, err
}

func (d *Dialect) nullTimeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return d.timeArg(t)
}

// InsertUpstream adds an upstream with its keys, platforms and entries. The caller holds the
// tenant's upstream lock and has validated the fields.
func (t *Tx) InsertUpstream(ctx context.Context, u *Upstream) error {
	if err := ValidName(u.Name); err != nil {
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM upstreams WHERE tenant_id = ?", u.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxUpstreams {
		return fmt.Errorf("%w: a tenant holds at most %d upstreams", ErrInvalid, MaxUpstreams)
	}
	u.ID, u.CreatedAt, u.Version = NewID(), t.Now(), 1
	if u.Usage == "" {
		u.Usage = UsageForward
	}
	if u.FetchMode == "" {
		u.FetchMode = FetchEager
	}
	_, err := t.exec(ctx, "INSERT INTO upstreams ("+upstreamCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.TenantID, u.Name, u.Kind, nullable(u.Prefix), u.ChannelID, u.Mode, u.Visibility, u.State, u.Version,
		t.s.d.nullTimeArg(u.RequestedAt), u.RequestDryRun, t.s.d.nullTimeArg(u.NextRunAt), nil, nil,
		t.s.d.timeArg(u.CreatedAt), u.CreatedBy, nullable(u.Credential), nil, nil, nil, u.Usage, u.FetchMode)
	if err := t.s.mapErr(err, "upstream "+u.Name); err != nil {
		return err
	}
	for _, k := range u.Keys {
		if err := t.AddUpstreamKey(ctx, u.ID, k); err != nil {
			return err
		}
	}
	for _, p := range u.Platforms {
		if err := t.AddUpstreamPlatform(ctx, u.ID, p); err != nil {
			return err
		}
	}
	for _, e := range u.Entries {
		if err := t.PutUpstreamEntry(ctx, u.ID, e); err != nil {
			return err
		}
	}
	return nil
}

// GetUpstream reads a tenant's upstream by name, with its keys, platforms and entries.
func (t *Tx) GetUpstream(ctx context.Context, tenantID, name string) (Upstream, error) {
	u, err := scanUpstream(t.queryRow(ctx, "SELECT "+upstreamCols+" FROM upstreams WHERE tenant_id = ? AND name = ?", tenantID, name))
	if err != nil {
		return u, notFound(err, "upstream "+name)
	}
	return u, t.upstreamChildren(ctx, &u)
}

// GetUpstreamByID reads an upstream by id, with its children.
func (t *Tx) GetUpstreamByID(ctx context.Context, id string) (Upstream, error) {
	u, err := scanUpstream(t.queryRow(ctx, "SELECT "+upstreamCols+" FROM upstreams WHERE id = ?", id))
	if err != nil {
		return u, notFound(err, "upstream "+id)
	}
	return u, t.upstreamChildren(ctx, &u)
}

func (t *Tx) upstreamChildren(ctx context.Context, u *Upstream) error {
	var err error
	if u.Keys, err = t.strings(ctx, "SELECT fingerprint FROM upstream_keys WHERE upstream_id = ? ORDER BY fingerprint", u.ID); err != nil {
		return err
	}
	if u.Platforms, err = t.strings(ctx, "SELECT platform FROM upstream_platforms WHERE upstream_id = ? ORDER BY platform", u.ID); err != nil {
		return err
	}
	rows, err := t.query(ctx, "SELECT name, allow_reserved, eager FROM upstream_entries WHERE upstream_id = ? ORDER BY name", u.ID)
	if err != nil {
		return err
	}
	u.Entries = nil
	for rows.Next() {
		var e UpstreamEntry
		if err := rows.Scan(&e.Name, &e.AllowReserved, &e.Eager); err != nil {
			rows.Close()
			return err
		}
		u.Entries = append(u.Entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	vrows, err := t.query(ctx, "SELECT name, package_version FROM upstream_versions WHERE upstream_id = ? ORDER BY name, package_version", u.ID)
	if err != nil {
		return err
	}
	defer vrows.Close()
	idx := map[string]int{}
	for i, e := range u.Entries {
		idx[e.Name] = i
	}
	for vrows.Next() {
		var n, v string
		if err := vrows.Scan(&n, &v); err != nil {
			return err
		}
		if i, ok := idx[n]; ok {
			u.Entries[i].Versions = append(u.Entries[i].Versions, v)
		}
	}
	return vrows.Err()
}

func (t *Tx) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
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

// ListUpstreams lists a tenant's upstreams by name, with their children.
func (t *Tx) ListUpstreams(ctx context.Context, tenantID string) ([]Upstream, error) {
	rows, err := t.query(ctx, "SELECT "+upstreamCols+" FROM upstreams WHERE tenant_id = ? ORDER BY name", tenantID)
	if err != nil {
		return nil, err
	}
	var out []Upstream
	for rows.Next() {
		u, err := scanUpstream(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := t.upstreamChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteUpstream removes an upstream (compare-and-set on its version; 0 skips the check) with its
// children and cells. Its releases stay.
func (t *Tx) DeleteUpstream(ctx context.Context, u Upstream, expected int64) error {
	if expected != 0 && expected != u.Version {
		return ErrConflict
	}
	for _, c := range []string{"upstream_asks", "upstream_cells", "upstream_versions", "upstream_entries", "upstream_platforms", "upstream_keys"} {
		if _, err := t.exec(ctx, "DELETE FROM "+c+" WHERE upstream_id = ?", u.ID); err != nil {
			return err
		}
	}
	if err := t.DeleteLicenseKeysOf(ctx, KeySourceUpstream, u.ID); err != nil {
		return err // spec 0021: the keys it brought go with it
	}
	if err := t.dropFollows(ctx, UpstreamSource(u.ID)); err != nil { // spec 0033: nothing follows a gone source
		return err
	}
	return t.cas(ctx, "DELETE FROM upstreams WHERE id = ? AND version = ?", []any{u.ID, u.Version}, func() {})
}

// SetUpstream changes an upstream's visibility or state (compare-and-set; expected 0 skips the
// check). An empty value leaves the field.
func (t *Tx) SetUpstream(ctx context.Context, u *Upstream, visibility, state string, expected int64) error {
	if visibility == "" {
		visibility = u.Visibility
	}
	if state == "" {
		state = u.State
	}
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET visibility = ?, state = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{visibility, state, u.ID, expected}, func() { u.Visibility, u.State, u.Version = visibility, state, expected+1 })
}

// SetUpstreamCredential sets or clears an upstream's credential (expected: its version).
func (t *Tx) SetUpstreamCredential(ctx context.Context, u *Upstream, credential string, expected int64) error {
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET credential = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{nullable(credential), u.ID, expected}, func() { u.Credential, u.Version = credential, expected+1 })
}

// SetUpstreamUsage sets whether an upstream gets this tenant's usage (spec 0012 phase 1c).
func (t *Tx) SetUpstreamUsage(ctx context.Context, u *Upstream, usage string, expected int64) error {
	if usage != UsageForward && usage != UsageOff {
		return fmt.Errorf("%w: usage is %s or %s", ErrInvalid, UsageForward, UsageOff)
	}
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET usage = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{usage, u.ID, expected}, func() { u.Usage, u.Version = usage, expected+1 })
}

// SetUpstreamFetchMode sets a mirror's fetch mode (spec 0032; expected: its version).
func (t *Tx) SetUpstreamFetchMode(ctx context.Context, u *Upstream, mode string, expected int64) error {
	if mode != FetchEager && mode != FetchLazy {
		return fmt.Errorf("%w: fetch is %s or %s", ErrInvalid, FetchEager, FetchLazy)
	}
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET fetch_mode = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{mode, u.ID, expected}, func() { u.FetchMode, u.Version = mode, expected+1 })
}

// BumpUpstream moves an upstream's version: its keys, platforms or entries changed.
func (t *Tx) BumpUpstream(ctx context.Context, u *Upstream) error {
	return t.cas(ctx, "UPDATE upstreams SET version = version + 1 WHERE id = ? AND version = ?", []any{u.ID, u.Version},
		func() { u.Version++ })
}

// AddUpstreamKey pins a key fingerprint.
func (t *Tx) AddUpstreamKey(ctx context.Context, upstreamID, fingerprint string) error {
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM upstream_keys WHERE upstream_id = ?", upstreamID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxUpstreamKeys {
		return fmt.Errorf("%w: an upstream pins at most %d keys", ErrInvalid, MaxUpstreamKeys)
	}
	_, err := t.exec(ctx, "INSERT INTO upstream_keys (upstream_id, fingerprint) VALUES (?, ?)", upstreamID, fingerprint)
	return t.s.mapErr(err, "key "+fingerprint)
}

// RemoveUpstreamKey unpins a key.
func (t *Tx) RemoveUpstreamKey(ctx context.Context, upstreamID, fingerprint string) error {
	return t.deleteOne(ctx, "DELETE FROM upstream_keys WHERE upstream_id = ? AND fingerprint = ?", "key "+fingerprint, upstreamID, fingerprint)
}

// AddUpstreamPlatform adds a platform.
func (t *Tx) AddUpstreamPlatform(ctx context.Context, upstreamID, platform string) error {
	_, err := t.exec(ctx, "INSERT INTO upstream_platforms (upstream_id, platform) VALUES (?, ?)", upstreamID, platform)
	return t.s.mapErr(err, "platform "+platform)
}

// RemoveUpstreamPlatform removes a platform.
func (t *Tx) RemoveUpstreamPlatform(ctx context.Context, upstreamID, platform string) error {
	if err := t.deleteOne(ctx, "DELETE FROM upstream_platforms WHERE upstream_id = ? AND platform = ?", "platform "+platform, upstreamID, platform); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM upstream_asks WHERE upstream_id = ? AND platform = ?", upstreamID, platform); err != nil {
		return err
	}
	_, err := t.exec(ctx, "DELETE FROM upstream_cells WHERE upstream_id = ? AND platform = ?", upstreamID, platform)
	return err
}

// PutUpstreamEntry adds an allowlist entry or replaces its versions, allow_reserved and eager. It
// reports nothing about whether it existed; callers read first when they need to.
func (t *Tx) PutUpstreamEntry(ctx context.Context, upstreamID string, e UpstreamEntry) error {
	if len(e.Versions) > MaxEntryVersions {
		return fmt.Errorf("%w: an entry lists at most %d versions", ErrInvalid, MaxEntryVersions)
	}
	if _, err := t.exec(ctx, "DELETE FROM upstream_versions WHERE upstream_id = ? AND name = ?", upstreamID, e.Name); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM upstream_entries WHERE upstream_id = ? AND name = ?", upstreamID, e.Name); err != nil {
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM upstream_entries WHERE upstream_id = ?", upstreamID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxUpstreamEntries {
		return fmt.Errorf("%w: an upstream lists at most %d extensions", ErrInvalid, MaxUpstreamEntries)
	}
	if _, err := t.exec(ctx, "INSERT INTO upstream_entries (upstream_id, name, allow_reserved, eager) VALUES (?, ?, ?, ?)",
		upstreamID, e.Name, e.AllowReserved, e.Eager); err != nil {
		return t.s.mapErr(err, "extension "+e.Name)
	}
	for _, v := range e.Versions {
		if _, err := t.exec(ctx, "INSERT INTO upstream_versions (upstream_id, name, package_version) VALUES (?, ?, ?)",
			upstreamID, e.Name, v); err != nil {
			return t.s.mapErr(err, "version "+v)
		}
	}
	return nil
}

// RemoveUpstreamEntry removes an allowlist entry and its versions.
func (t *Tx) RemoveUpstreamEntry(ctx context.Context, upstreamID, name string) error {
	if _, err := t.exec(ctx, "DELETE FROM upstream_versions WHERE upstream_id = ? AND name = ?", upstreamID, name); err != nil {
		return err
	}
	if err := t.deleteOne(ctx, "DELETE FROM upstream_entries WHERE upstream_id = ? AND name = ?", "extension "+name, upstreamID, name); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM upstream_asks WHERE upstream_id = ? AND name = ?", upstreamID, name); err != nil {
		return err
	}
	_, err := t.exec(ctx, "DELETE FROM upstream_cells WHERE upstream_id = ? AND name = ?", upstreamID, name)
	return err
}

func (t *Tx) deleteOne(ctx context.Context, q, what string, args ...any) error {
	res, err := t.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

// UpstreamProvides reports whether an upstream of the tenant lists a package (type, name) (spec
// 0009: the name is reserved in the tenant; spec 0034: an upstream's names are of its home
// channel's type).
func (t *Tx) UpstreamProvides(ctx context.Context, tenantID, packageType, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
JOIN channels c ON c.id = u.channel_id WHERE u.tenant_id = ? AND c.package_type = ? AND e.name = ?`, tenantID, packageType, name).Scan(&n)
	return n > 0, err
}

// UpstreamProvides is Tx.UpstreamProvides outside a transaction.
func (s *Store) UpstreamProvides(ctx context.Context, tenantID, packageType, name string) (bool, error) {
	var ok bool
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		ok, err = tx.UpstreamProvides(ctx, tenantID, packageType, name)
		return err
	})
	return ok, err
}

// ChannelEntryOwner returns the name of another upstream of the channel that lists name, or "".
func (t *Tx) ChannelEntryOwner(ctx context.Context, channelID, name, exceptUpstreamID string) (string, error) {
	var owner sql.NullString
	err := t.queryRow(ctx, `SELECT MIN(u.name) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
WHERE u.channel_id = ? AND e.name = ? AND u.id <> ?`, channelID, name, exceptUpstreamID).Scan(&owner)
	return owner.String, err
}

// TenantCells counts the cells of the matrices of a tenant's upstreams other than one: entries ×
// platforms × the channel's DuckDB versions.
func (t *Tx) TenantCells(ctx context.Context, tenantID, exceptUpstreamID string) (int, error) {
	rows, err := t.query(ctx, `SELECT
 (SELECT COUNT(*) FROM upstream_entries e WHERE e.upstream_id = u.id) *
 (SELECT COUNT(*) FROM upstream_platforms p WHERE p.upstream_id = u.id) *
 (SELECT COUNT(*) FROM channel_duckdb_versions v WHERE v.channel_id = u.channel_id)
FROM upstreams u WHERE u.tenant_id = ? AND u.id <> ?`, tenantID, exceptUpstreamID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, rows.Err()
}

// RequestUpstreamRun asks for a run; a real request pending with a dry one makes one real run.
func (t *Tx) RequestUpstreamRun(ctx context.Context, u *Upstream, dryRun bool) error {
	if !u.RequestedAt.IsZero() {
		dryRun = dryRun && u.RequestDryRun
	} else {
		u.RequestedAt = t.Now()
	}
	u.RequestDryRun = dryRun
	_, err := t.exec(ctx, "UPDATE upstreams SET requested_at = ?, request_dry_run = ? WHERE id = ?",
		t.s.d.timeArg(u.RequestedAt), dryRun, u.ID)
	return err
}

// DueUpstreams lists the ids of active upstreams with a run requested or, when scheduled, mirrors due
// by their schedule (a mirror never run is due).
func (s *Store) DueUpstreams(ctx context.Context, now time.Time, scheduled bool) ([]string, error) {
	var out []string
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		if !scheduled {
			out, err = tx.strings(ctx, "SELECT id FROM upstreams WHERE state = ? AND mode <> ? AND requested_at IS NOT NULL AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL) ORDER BY id",
				UpstreamActive, ModeBundle)
			return err
		}
		// requested runs first, then the longest due
		// a bundle-mode upstream is never run, even with a request (spec 0014)
		out, err = tx.strings(ctx, `SELECT id FROM upstreams WHERE state = ? AND mode <> ? AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL) AND (requested_at IS NOT NULL OR
(mode = ? AND (next_run_at IS NULL OR next_run_at <= ?)))
ORDER BY CASE WHEN requested_at IS NULL THEN 1 ELSE 0 END, CASE WHEN next_run_at IS NULL THEN 0 ELSE 1 END, next_run_at, id`,
			UpstreamActive, ModeBundle, ModeMirror, s.d.timeArg(now))
		return err
	})
	return out, err
}

// AskRefresh is how old an ask is before a new one renews its time (spec 0032): retries and polls
// cost a read.
const AskRefresh = time.Hour

// Ask records that a cell of a lazy upstream was asked for (spec 0032): inserted when absent, its
// time renewed when older than refresh (AskRefresh, or less to reopen a first request's window). It
// reports whether it wrote.
func (t *Tx) Ask(ctx context.Context, upstreamID, duckdbVersion, platform, name string, refresh time.Duration) (bool, error) {
	var at time.Time
	err := t.queryRow(ctx, "SELECT asked_at FROM upstream_asks WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?",
		upstreamID, duckdbVersion, platform, name).Scan(scanTime{&at})
	now := t.Now()
	switch {
	case errors.Is(err, sql.ErrNoRows): // inserted unless another request did meanwhile
		var res sql.Result
		if t.s.d.Name == "sqlserver" {
			res, err = t.exec(ctx, `MERGE upstream_asks WITH (HOLDLOCK) AS a
USING (SELECT ? AS upstream_id, ? AS duckdb_version, ? AS platform, ? AS name) AS n
ON a.upstream_id = n.upstream_id AND a.duckdb_version = n.duckdb_version AND a.platform = n.platform AND a.name = n.name
WHEN NOT MATCHED THEN INSERT (upstream_id, duckdb_version, platform, name, asked_at) VALUES (?, ?, ?, ?, ?);`,
				upstreamID, duckdbVersion, platform, name, upstreamID, duckdbVersion, platform, name, t.s.d.timeArg(now))
		} else {
			res, err = t.exec(ctx, `INSERT INTO upstream_asks (upstream_id, duckdb_version, platform, name, asked_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (upstream_id, duckdb_version, platform, name) DO NOTHING`, upstreamID, duckdbVersion, platform, name, t.s.d.timeArg(now))
		}
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	case err != nil:
		return false, err
	case now.Sub(at) < refresh:
		return false, nil
	}
	_, err = t.exec(ctx, "UPDATE upstream_asks SET asked_at = ? WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?",
		t.s.d.timeArg(now), upstreamID, duckdbVersion, platform, name)
	return err == nil, err
}

// AskState is what a lazy mirror's first request reads of a cell (spec 0032), outside any write
// transaction: when it was last asked for (zero: never), whether it has a record, and whether a fetch
// holds its lease.
type AskState struct {
	AskedAt          time.Time
	Recorded, Leased bool
}

// ReadAsk reads a cell's AskState; lease is its lease's name.
func (s *Store) ReadAsk(ctx context.Context, upstreamID, duckdbVersion, platform, name, lease string) (AskState, error) {
	var st AskState
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT asked_at FROM upstream_asks WHERE upstream_id = ? AND duckdb_version = ?
AND platform = ? AND name = ?`), upstreamID, duckdbVersion, platform, name).Scan(scanTime{&st.AskedAt})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*) FROM upstream_cells WHERE upstream_id = ? AND duckdb_version = ?
AND platform = ? AND name = ?`), upstreamID, duckdbVersion, platform, name).Scan(&n); err != nil {
		return st, err
	}
	st.Recorded = n > 0
	_, until, found, err := s.LeaseState(ctx, lease)
	st.Leased = found && until.After(s.Now())
	return st, err
}

// Asks reads a lazy upstream's asks, by (DuckDB version, platform, name).
func (t *Tx) Asks(ctx context.Context, upstreamID string) (map[[3]string]time.Time, error) {
	rows, err := t.query(ctx, "SELECT duckdb_version, platform, name, asked_at FROM upstream_asks WHERE upstream_id = ?", upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[3]string]time.Time{}
	for rows.Next() {
		var k [3]string
		var at time.Time
		if err := rows.Scan(&k[0], &k[1], &k[2], scanTime{&at}); err != nil {
			return nil, err
		}
		out[k] = at
	}
	return out, rows.Err()
}

// DeleteAsk removes an ask (one that left the matrix).
func (t *Tx) DeleteAsk(ctx context.Context, upstreamID, duckdbVersion, platform, name string) error {
	_, err := t.exec(ctx, "DELETE FROM upstream_asks WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?",
		upstreamID, duckdbVersion, platform, name)
	return err
}

// AskFilled records an ask for each cell of an upstream that was fetched and found (spec 0032: an
// upstream turning lazy keeps its releases current), with the cell's time: every recorded cell but
// a missing one, since a failed fetch's record no longer names the body a release holds. An ask
// recorded meanwhile is kept.
func (t *Tx) AskFilled(ctx context.Context, upstreamID string) error {
	const cells = `SELECT c.upstream_id, c.duckdb_version, c.platform, c.name, c.fetched_at FROM upstream_cells c
WHERE c.upstream_id = ? AND c.outcome <> ?`
	if t.s.d.Name == "sqlserver" {
		_, err := t.exec(ctx, `MERGE upstream_asks WITH (HOLDLOCK) AS a USING (`+cells+`) AS n (upstream_id, duckdb_version, platform, name, asked_at)
ON a.upstream_id = n.upstream_id AND a.duckdb_version = n.duckdb_version AND a.platform = n.platform AND a.name = n.name
WHEN NOT MATCHED THEN INSERT (upstream_id, duckdb_version, platform, name, asked_at)
VALUES (n.upstream_id, n.duckdb_version, n.platform, n.name, n.asked_at);`, upstreamID, CellMissing)
		return err
	}
	_, err := t.exec(ctx, `INSERT INTO upstream_asks (upstream_id, duckdb_version, platform, name, asked_at) `+cells+`
ON CONFLICT (upstream_id, duckdb_version, platform, name) DO NOTHING`, upstreamID, CellMissing)
	return err
}

// MakeDue makes mirrors due now: an upstream's (its matrix grew), or every mirror feeding a channel
// (a DuckDB version was added).
func (t *Tx) MakeDue(ctx context.Context, upstreamID, channelID string) error {
	now := t.s.d.timeArg(t.Now())
	if upstreamID != "" {
		_, err := t.exec(ctx, "UPDATE upstreams SET next_run_at = ? WHERE id = ? AND mode = ?", now, upstreamID, ModeMirror)
		return err
	}
	_, err := t.exec(ctx, "UPDATE upstreams SET next_run_at = ? WHERE channel_id = ? AND mode = ?", now, channelID, ModeMirror)
	return err
}

// Shadow is a core name a tenant replaced (spec 0009): its passthrough channels do not serve it.
type Shadow struct {
	TenantID, Name string
	CreatedAt      time.Time
	CreatedBy      string
}

// AddShadow records a shadow if it is new, and bumps the tenant's passthrough channels; it reports
// whether it was new.
func (t *Tx) AddShadow(ctx context.Context, tenantID, name, actor string) (bool, error) {
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM shadows WHERE tenant_id = ? AND name = ?", tenantID, name).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	if _, err := t.exec(ctx, "INSERT INTO shadows (tenant_id, name, created_at, created_by) VALUES (?, ?, ?, ?)",
		tenantID, name, t.s.d.timeArg(t.Now()), actor); err != nil {
		return false, t.s.mapErr(err, "shadow "+name)
	}
	return true, t.bumpPassthrough(ctx, tenantID)
}

// DeleteShadow removes a shadow: the tenant's passthrough channels serve DuckDB's build again.
func (t *Tx) DeleteShadow(ctx context.Context, tenantID, name string) error {
	if err := t.deleteOne(ctx, "DELETE FROM shadows WHERE tenant_id = ? AND name = ?", "shadow "+name, tenantID, name); err != nil {
		return err
	}
	return t.bumpPassthrough(ctx, tenantID)
}

func (t *Tx) bumpPassthrough(ctx context.Context, tenantID string) error {
	_, err := t.exec(ctx, "UPDATE channels SET release_version = release_version + 1 WHERE tenant_id = ? AND kind = ?", tenantID, ChannelPassthrough)
	return err
}

// ListShadows lists a tenant's shadows by name.
func (s *Store) ListShadows(ctx context.Context, tenantID string) ([]Shadow, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT tenant_id, name, created_at, created_by FROM shadows WHERE tenant_id = ? ORDER BY name"), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shadow
	for rows.Next() {
		var x Shadow
		if err := rows.Scan(&x.TenantID, &x.Name, scanTime{&x.CreatedAt}, &x.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LiveSignedBuilds lists the Builds of a tenant's live releases in its signed channels (name, body
// hash and original signature only), for the shadow backfill (spec 0009).
func (t *Tx) LiveSignedBuilds(ctx context.Context, tenantID string) ([]Build, error) {
	rows, err := t.query(ctx, `SELECT DISTINCT b.name, b.body_hash, b.origin_signature FROM releases r JOIN builds b ON b.id = r.build_id
JOIN channels c ON c.id = r.channel_id WHERE r.tenant_id = ? AND c.kind = ? AND c.package_type = ? AND r.state <> ?`,
		tenantID, ChannelSigned, pkgtype.DuckDBExtension, ReleaseYanked) // shadows are DuckDB's (spec 0034)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		if err := rows.Scan(&b.Name, &b.BodyHash, &b.OriginSignature); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TakeShadowBackfill marks a tenant's shadow backfill done; it reports whether this call did, so
// the backfill runs once per tenant (an administrator's removals stay removed).
func (t *Tx) TakeShadowBackfill(ctx context.Context, tenantID string) (bool, error) {
	res, err := t.exec(ctx, "UPDATE tenants SET shadows_backfilled = ? WHERE id = ? AND shadows_backfilled = ?", true, tenantID, false)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PassthroughProvides reports whether an upstream feeding one of the tenant's passthrough channels
// lists name.
func (t *Tx) PassthroughProvides(ctx context.Context, tenantID, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
JOIN channels c ON c.id = u.channel_id WHERE u.tenant_id = ? AND c.kind = ? AND e.name = ?`, tenantID, ChannelPassthrough, name).Scan(&n)
	return n > 0, err
}

// OriginSignature reads an upstream release's original signature (spec 0009: what a passthrough
// channel serves).
func (s *Store) OriginSignature(ctx context.Context, releaseID string) ([]byte, error) {
	var sig []byte
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT origin_signature FROM releases WHERE id = ?"), releaseID).Scan(&sig)
	if err == nil && len(sig) == 0 {
		err = sql.ErrNoRows
	}
	return sig, notFound(err, "original signature")
}

// StartUpstreamRun takes the pending request of an upstream that is active and requested or due
// (runnable false otherwise: paused, or run by another replica meanwhile). It reports whether the
// run is a dry one and whether it was requested, and clears the request, so one that arrives
// during the run is served by the next.
func (t *Tx) StartUpstreamRun(ctx context.Context, id string, scheduled bool) (runnable, dryRun, requested bool, err error) {
	u, err := scanUpstream(t.queryRow(ctx, "SELECT "+upstreamCols+" FROM upstreams WHERE id = ?", id))
	if err != nil {
		return false, false, false, notFound(err, "upstream "+id)
	}
	requested = !u.RequestedAt.IsZero()
	due := scheduled && u.Mode == ModeMirror && !u.NextRunAt.After(t.Now()) // a zero next run: never run
	if u.State != UpstreamActive || u.Mode == ModeBundle || !requested && !due {
		return false, false, false, nil
	}
	_, err = t.exec(ctx, "UPDATE upstreams SET requested_at = NULL, request_dry_run = ? WHERE id = ?", false, id)
	return true, requested && u.RequestDryRun, requested, err
}

// FinishUpstreamRun records a run's result and, when schedule is set, the next scheduled run
// (zero: none), unless the upstream was made due during the run (after started); a dry run leaves
// the schedule.
func (t *Tx) FinishUpstreamRun(ctx context.Context, id, result string, schedule bool, started, next time.Time) error {
	if _, err := t.exec(ctx, "UPDATE upstreams SET last_run = ?, last_run_at = ? WHERE id = ?", result, t.s.d.timeArg(t.Now()), id); err != nil {
		return err
	}
	if !schedule {
		return nil
	}
	_, err := t.exec(ctx, "UPDATE upstreams SET next_run_at = ? WHERE id = ? AND (next_run_at IS NULL OR next_run_at <= ?)",
		t.s.d.nullTimeArg(next), id, t.s.d.timeArg(started))
	return err
}

// Cells reads an upstream's cells, by (DuckDB version, platform, name).
func (t *Tx) Cells(ctx context.Context, upstreamID string) (map[[3]string]UpstreamCell, error) {
	rows, err := t.query(ctx, `SELECT upstream_id, duckdb_version, platform, name, etag, body_hash, outcome, detail, fetched_at
FROM upstream_cells WHERE upstream_id = ?`, upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[3]string]UpstreamCell{}
	for rows.Next() {
		c, err := scanCell(rows)
		if err != nil {
			return nil, err
		}
		out[[3]string{c.DuckDBVersion, c.Platform, c.Name}] = c
	}
	return out, rows.Err()
}

func scanCell(r interface{ Scan(...any) error }) (UpstreamCell, error) {
	var c UpstreamCell
	var etag, hash, detail sql.NullString
	err := r.Scan(&c.UpstreamID, &c.DuckDBVersion, &c.Platform, &c.Name, &etag, &hash, &c.Outcome, &detail, scanTime{&c.FetchedAt})
	c.ETag, c.BodyHash, c.Detail = etag.String, hash.String, detail.String
	return c, err
}

// PutCell records a cell's outcome.
func (t *Tx) PutCell(ctx context.Context, c UpstreamCell) error {
	if len(c.ETag) > 256 {
		c.ETag = ""
	}
	if len(c.Detail) > 1000 {
		c.Detail = strings.ToValidUTF8(c.Detail[:1000], "")
	}
	if _, err := t.exec(ctx, "DELETE FROM upstream_cells WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?",
		c.UpstreamID, c.DuckDBVersion, c.Platform, c.Name); err != nil {
		return err
	}
	_, err := t.exec(ctx, `INSERT INTO upstream_cells (upstream_id, duckdb_version, platform, name, etag, body_hash, outcome, detail, fetched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.UpstreamID, c.DuckDBVersion, c.Platform, c.Name, nullable(c.ETag), nullable(c.BodyHash),
		c.Outcome, nullable(c.Detail), t.s.d.timeArg(c.FetchedAt))
	return err
}

// DeleteCell removes a cell (one that left the matrix).
func (t *Tx) DeleteCell(ctx context.Context, upstreamID, duckdbVersion, platform, name string) error {
	_, err := t.exec(ctx, "DELETE FROM upstream_cells WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?",
		upstreamID, duckdbVersion, platform, name)
	return err
}

// ListCells pages an upstream's cells by (DuckDB version, platform, name) after a cursor key,
// optionally of one outcome.
func (t *Tx) ListCells(ctx context.Context, upstreamID, outcome string, after [3]string, limit int) ([]UpstreamCell, error) {
	q := `SELECT upstream_id, duckdb_version, platform, name, etag, body_hash, outcome, detail, fetched_at FROM upstream_cells
WHERE upstream_id = ? AND (duckdb_version > ? OR duckdb_version = ? AND (platform > ? OR platform = ? AND name > ?))`
	args := []any{upstreamID, after[0], after[0], after[1], after[1], after[2]}
	if outcome != "" {
		q += " AND outcome = ?"
		args = append(args, outcome)
	}
	q += " ORDER BY duckdb_version, platform, name"
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamCell
	for len(out) < limit && rows.Next() {
		c, err := scanCell(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ServeChannelByID reads a channel and its tenant by the channel's id.
func (s *Store) ServeChannelByID(ctx context.Context, channelID string) (ServeChannel, error) {
	var tenant, channel string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT t.name, c.name FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE c.id = ?"),
		channelID).Scan(&tenant, &channel)
	if err != nil {
		return ServeChannel{}, notFound(err, "channel")
	}
	return s.GetServeChannel(ctx, tenant, channel)
}

// TenantChannelIDs lists the ids of a tenant's channels.
func (t *Tx) TenantChannelIDs(ctx context.Context, tenantID string) ([]string, error) {
	return t.strings(ctx, "SELECT id FROM channels WHERE tenant_id = ? ORDER BY id", tenantID)
}

// PullThrough is what serving needs about a channel's pull-through upstreams (spec 0009 phase 2)
// and lazy mirrors (spec 0032): which cells a miss may offer, decided without the database (it is
// part of the channel's snapshot).
type PullThrough struct {
	UpstreamID, TenantID string
	Kind                 string // the upstream's (spec 0030: "*" on community takes community names)
	Platforms, Names     map[string]bool
	Any                  bool // "*": any name no other upstream of the channel lists, and no reserved name
	Lazy                 bool // a lazy mirror: a miss is an ask
}

// ChannelPullThrough reads a channel's active pull-through upstreams and lazy mirrors, and every
// name an upstream of the channel lists (an explicit entry wins over "*").
func (s *Store) ChannelPullThrough(ctx context.Context, channelID string) ([]PullThrough, map[string]bool, error) {
	var out []PullThrough
	listed := map[string]bool{}
	err := s.InTx(ctx, "", func(tx *Tx) error {
		out, listed = nil, map[string]bool{}
		names, err := tx.strings(ctx, `SELECT DISTINCT e.name FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
WHERE u.channel_id = ?`, channelID)
		if err != nil {
			return err
		}
		for _, n := range names {
			if n != "*" {
				listed[n] = true
			}
		}
		rows, err := tx.query(ctx, "SELECT "+upstreamCols+" FROM upstreams WHERE channel_id = ? AND (mode = ? OR mode = ? AND fetch_mode = ?) AND state = ? ORDER BY name",
			channelID, ModePullThrough, ModeMirror, FetchLazy, UpstreamActive)
		if err != nil {
			return err
		}
		var us []Upstream
		for rows.Next() {
			u, err := scanUpstream(rows)
			if err != nil {
				rows.Close()
				return err
			}
			us = append(us, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range us {
			if err := tx.upstreamChildren(ctx, &us[i]); err != nil {
				return err
			}
			p := PullThrough{UpstreamID: us[i].ID, TenantID: us[i].TenantID, Kind: us[i].Kind, Platforms: map[string]bool{}, Names: map[string]bool{},
				Lazy: us[i].Mode == ModeMirror}
			for _, x := range us[i].Platforms {
				p.Platforms[x] = true
			}
			for _, e := range us[i].Entries {
				if e.Name == "*" {
					p.Any = true
				} else {
					p.Names[e.Name] = true
				}
			}
			out = append(out, p)
		}
		return nil
	})
	return out, listed, err
}

// GetCell reads one cell (ErrNotFound for none).
func (t *Tx) GetCell(ctx context.Context, upstreamID, duckdbVersion, platform, name string) (UpstreamCell, error) {
	c, err := scanCell(t.queryRow(ctx, `SELECT upstream_id, duckdb_version, platform, name, etag, body_hash, outcome, detail, fetched_at
FROM upstream_cells WHERE upstream_id = ? AND duckdb_version = ? AND platform = ? AND name = ?`, upstreamID, duckdbVersion, platform, name))
	return c, notFound(err, "cell")
}

// CellOutcomes counts the upstream cells per outcome (metrics).
func (s *Store) CellOutcomes(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.eachRow(ctx, "SELECT outcome, COUNT(*) FROM upstream_cells GROUP BY outcome", nil, func(sc func(...any) error) error {
		var o string
		var n int64
		if err := sc(&o, &n); err != nil {
			return err
		}
		out[o] = n
		return nil
	})
	return out, err
}
