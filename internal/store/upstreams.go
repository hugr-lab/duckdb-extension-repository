package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Upstream kinds, modes, states and cell outcomes (spec 0009).
const (
	UpstreamCore      = "duckdb-core"
	UpstreamCommunity = "duckdb-community"
	UpstreamRepo      = "repository"

	ModeMirror      = "mirror"
	ModePullThrough = "pull-through"

	UpstreamActive = "active"
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
	CellFailed    = "failed" // the fetch failed (network, size, rate): retried by the next run
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

	Keys      []string // fingerprints
	Platforms []string
	Entries   []UpstreamEntry
}

// UpstreamEntry is an allowlist entry: a name and the extension versions accepted (none: any).
type UpstreamEntry struct {
	Name          string
	AllowReserved bool
	Versions      []string
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
request_dry_run, next_run_at, last_run_at, last_run, created_at, created_by`

func scanUpstream(r interface{ Scan(...any) error }) (Upstream, error) {
	var u Upstream
	var prefix, lastRun sql.NullString
	err := r.Scan(&u.ID, &u.TenantID, &u.Name, &u.Kind, &prefix, &u.ChannelID, &u.Mode, &u.Visibility, &u.State, &u.Version,
		scanTime{&u.RequestedAt}, &u.RequestDryRun, scanTime{&u.NextRunAt}, scanTime{&u.LastRunAt}, &lastRun,
		scanTime{&u.CreatedAt}, &u.CreatedBy)
	u.Prefix, u.LastRun = prefix.String, lastRun.String
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
	_, err := t.exec(ctx, "INSERT INTO upstreams ("+upstreamCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.TenantID, u.Name, u.Kind, nullable(u.Prefix), u.ChannelID, u.Mode, u.Visibility, u.State, u.Version,
		t.s.d.nullTimeArg(u.RequestedAt), u.RequestDryRun, t.s.d.nullTimeArg(u.NextRunAt), nil, nil,
		t.s.d.timeArg(u.CreatedAt), u.CreatedBy)
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
	rows, err := t.query(ctx, "SELECT name, allow_reserved FROM upstream_entries WHERE upstream_id = ? ORDER BY name", u.ID)
	if err != nil {
		return err
	}
	u.Entries = nil
	for rows.Next() {
		var e UpstreamEntry
		if err := rows.Scan(&e.Name, &e.AllowReserved); err != nil {
			rows.Close()
			return err
		}
		u.Entries = append(u.Entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	vrows, err := t.query(ctx, "SELECT name, ext_version FROM upstream_versions WHERE upstream_id = ? ORDER BY name, ext_version", u.ID)
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
	for _, c := range []string{"upstream_cells", "upstream_versions", "upstream_entries", "upstream_platforms", "upstream_keys"} {
		if _, err := t.exec(ctx, "DELETE FROM "+c+" WHERE upstream_id = ?", u.ID); err != nil {
			return err
		}
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
	return t.deleteOne(ctx, "DELETE FROM upstream_platforms WHERE upstream_id = ? AND platform = ?", "platform "+platform, upstreamID, platform)
}

// PutUpstreamEntry adds an allowlist entry or replaces its versions and allow_reserved. It reports
// nothing about whether it existed; callers read first when they need to.
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
	if _, err := t.exec(ctx, "INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES (?, ?, ?)",
		upstreamID, e.Name, e.AllowReserved); err != nil {
		return t.s.mapErr(err, "extension "+e.Name)
	}
	for _, v := range e.Versions {
		if _, err := t.exec(ctx, "INSERT INTO upstream_versions (upstream_id, name, ext_version) VALUES (?, ?, ?)",
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
	return t.deleteOne(ctx, "DELETE FROM upstream_entries WHERE upstream_id = ? AND name = ?", "extension "+name, upstreamID, name)
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

// UpstreamProvides reports whether an upstream of the tenant lists name (spec 0009: the name is
// reserved in the tenant).
func (t *Tx) UpstreamProvides(ctx context.Context, tenantID, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
WHERE u.tenant_id = ? AND e.name = ?`, tenantID, name).Scan(&n)
	return n > 0, err
}

// UpstreamProvides is Tx.UpstreamProvides outside a transaction.
func (s *Store) UpstreamProvides(ctx context.Context, tenantID, name string) (bool, error) {
	var ok bool
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		ok, err = tx.UpstreamProvides(ctx, tenantID, name)
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

// LiveReplacement reports whether a channel holds a live replacement of name (spec 0009): a release
// that is not yanked, whose origin is not upstream, and whose Build's origin is not upstream.
func (t *Tx) LiveReplacement(ctx context.Context, channelID, name string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM releases r JOIN builds b ON b.id = r.build_id
WHERE r.channel_id = ? AND r.name = ? AND r.state <> ? AND r.origin <> ? AND b.origin <> ?`,
		channelID, name, ReleaseYanked, OriginUpstream, OriginUpstream).Scan(&n)
	return n > 0, err
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
			out, err = tx.strings(ctx, "SELECT id FROM upstreams WHERE state = ? AND requested_at IS NOT NULL ORDER BY id", UpstreamActive)
			return err
		}
		// requested runs first, then the longest due
		out, err = tx.strings(ctx, `SELECT id FROM upstreams WHERE state = ? AND (requested_at IS NOT NULL OR
(mode = ? AND (next_run_at IS NULL OR next_run_at <= ?)))
ORDER BY CASE WHEN requested_at IS NULL THEN 1 ELSE 0 END, CASE WHEN next_run_at IS NULL THEN 0 ELSE 1 END, next_run_at, id`,
			UpstreamActive, ModeMirror, s.d.timeArg(now))
		return err
	})
	return out, err
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
JOIN channels c ON c.id = r.channel_id WHERE r.tenant_id = ? AND c.kind = ? AND r.state <> ?`, tenantID, ChannelSigned, ReleaseYanked)
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
	if u.State != UpstreamActive || !requested && !due {
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
