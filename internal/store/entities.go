package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Tenant states.
const (
	TenantActive    = "active"
	TenantSuspended = "suspended"
	// spec 0019: phases, not values of the state column (a deleting tenant stays suspended)
	TenantDeleting = "deleting"
	TenantDeleted  = "deleted"
)

// Channel kinds.
const (
	ChannelSigned      = "signed"
	ChannelPassthrough = "passthrough"
)

// Key states.
const (
	KeyTrusted = "trusted"
	KeyActive  = "active"
	KeyRetired = "retired"
)

// Tenant is a namespace.
type Tenant struct {
	ID, Name, DisplayName, State string
	StorageDomain                string // set at creation, never changed (spec 0005)
	AuthVersion                  int64  // bumped by every issuer, audience and grant change (spec 0006)
	CreatedAt                    time.Time
	Version                      int64
	// spec 0019: when it was suspended (zero while active, and for one an older binary suspended),
	// when its deletion started, and when its tombstone was written
	SuspendedAt, DeletingAt, DeletedAt time.Time
	// HolderID is the holder its licences must name, their sub (spec 0028); empty, none
	HolderID string
	// DistributorHold is its minimum hold for distributors' publishers (spec 0037): an entry from one
	// is held for the larger of the publisher's hold and this
	DistributorHold time.Duration
	// Limits are its own values of the limits (spec 0038): LimitDefault where the server's default
	// applies
	Limits Limits
}

// Phase is the tenant's state as the API shows it: active, suspended, deleting or deleted (spec
// 0019: a deleting tenant's state column stays suspended).
func (t Tenant) Phase() string {
	switch {
	case !t.DeletedAt.IsZero():
		return TenantDeleted
	case !t.DeletingAt.IsZero():
		return TenantDeleting
	}
	return t.State
}

// closed is the error for a tenant whose deletion started: nil while it is not.
func (t Tenant) closed() error {
	switch {
	case !t.DeletedAt.IsZero():
		return ErrDeleted
	case !t.DeletingAt.IsZero():
		return ErrDeleting
	}
	return nil
}

// DuckDBVersion is a DuckDB engine version: a release tag or a dev source id.
type DuckDBVersion struct {
	ID, Name, Kind string
	CAPIVersion    string // the legacy single value (spec 0003); spec 0006 keeps C APIs in CAPIs
	CAPIs          []CAPI // the maximum C API per major this version accepts
	CreatedAt      time.Time
}

// Channel is one DuckDB repository inside a tenant.
type Channel struct {
	ID, TenantID, Name, Kind string
	// PackageType is the type of every package the channel serves (spec 0034): set at creation,
	// never changed; a passthrough channel is duckdb_extension.
	PackageType    string
	CreatedAt      time.Time
	Version        int64
	ServingKeyID   string // the key whose signatures are served; empty before the first release
	ReleaseVersion int64  // bumped by every release change and serving-key move (spec 0006)
	// Hold is the channel's hold (spec 0033): an automatic placement into it waits until an entry's
	// arrival plus the hold, if that is later than the entry's own; 0 for none.
	Hold time.Duration
}

// Key is a channel signing key. Only its public key and signer reference are stored.
type Key struct {
	ID, TenantID, ChannelID, Fingerprint, SignerRef string
	PublicKey                                       []byte // SPKI DER
	State                                           string
	TrustedSince, StateChangedAt, CreatedAt         time.Time
	CreatedBy, StateChangedBy                       string
	Version                                         int64
}

// KeyEvent is one transition of a key, append-only.
type KeyEvent struct {
	ID, KeyID, From, To, Actor string
	Forced                     bool
	At                         time.Time
}

// --- reads (outside transactions) ---

const tenantCols = "id, name, display_name, state, storage_domain, auth_version, created_at, version, suspended_at, deleting_at, deleted_at, holder_id, distributor_hold_seconds, " +
	"limit_bytes, limit_channels, limit_upstreams, limit_publishers"

func scanTenant(r interface{ Scan(...any) error }) (Tenant, error) {
	var t Tenant
	var holder sql.NullString
	var hold int64
	var lb, lc, lu, lp sql.NullInt64
	err := r.Scan(&t.ID, &t.Name, &t.DisplayName, &t.State, &t.StorageDomain, &t.AuthVersion, scanTime{&t.CreatedAt}, &t.Version,
		scanTime{&t.SuspendedAt}, scanTime{&t.DeletingAt}, scanTime{&t.DeletedAt}, &holder, &hold, &lb, &lc, &lu, &lp)
	t.HolderID, t.DistributorHold = holder.String, time.Duration(hold)*time.Second
	t.Limits = Limits{scanLimit(lb), scanLimit(lc), scanLimit(lu), scanLimit(lp)}
	return t, err
}

func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

// GetTenant returns a tenant by name: ErrDeleting or ErrDeleted (both an ErrNotFound) once its
// deletion started (spec 0019), so everything that resolves a tenant by name treats it as gone.
func (s *Store) GetTenant(ctx context.Context, name string) (Tenant, error) {
	t, err := s.GetTenantAny(ctx, name)
	if err == nil {
		err = t.closed()
	}
	return t, err
}

// GetTenantAny returns a tenant by name in any phase, a deleting or deleted one included: for the
// few readers spec 0019 lets see one (a server administrator's view, its events, the reaper).
func (s *Store) GetTenantAny(ctx context.Context, name string) (Tenant, error) {
	t, err := scanTenant(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+tenantCols+" FROM tenants WHERE name = ?"), name))
	return t, notFound(err, "tenant "+name)
}

// ListTenants returns all tenants by name, in every phase: callers filter by Phase.
func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+tenantCols+" FROM tenants ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const versionCols = "id, name, kind, c_api_version, created_at"

func scanVersion(r interface{ Scan(...any) error }) (DuckDBVersion, error) {
	var v DuckDBVersion
	var capi sql.NullString
	err := r.Scan(&v.ID, &v.Name, &v.Kind, &capi, scanTime{&v.CreatedAt})
	v.CAPIVersion = capi.String
	return v, err
}

// ListDuckDBVersions returns all known DuckDB versions by name, with their C APIs.
func (s *Store) ListDuckDBVersions(ctx context.Context) ([]DuckDBVersion, error) {
	capis := map[string][]CAPI{}
	crows, err := s.db.QueryContext(ctx, "SELECT duckdb_version_id, major, max_minor, max_patch FROM duckdb_version_c_apis ORDER BY major")
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id string
		var c CAPI
		if err := crows.Scan(&id, &c.Major, &c.Minor, &c.Patch); err != nil {
			crows.Close()
			return nil, err
		}
		capis[id] = append(capis[id], c)
	}
	err = crows.Err()
	crows.Close()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+versionCols+" FROM duckdb_versions ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DuckDBVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		v.CAPIs = capis[v.ID]
		if len(v.CAPIs) == 0 {
			if c, err := ParseCAPI(strings.TrimSpace(v.CAPIVersion)); err == nil {
				v.CAPIs = []CAPI{c}
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const channelCols = "c.id, c.tenant_id, c.name, c.kind, c.created_at, c.version, c.serving_key_id, c.release_version, c.package_type, c.hold_seconds"

func scanChannel(r interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var serving sql.NullString
	var hold int64
	err := r.Scan(&c.ID, &c.TenantID, &c.Name, &c.Kind, scanTime{&c.CreatedAt}, &c.Version, &serving, &c.ReleaseVersion, &c.PackageType, &hold)
	c.ServingKeyID, c.Hold = serving.String, time.Duration(hold)*time.Second
	return c, err
}

// GetChannel returns a channel by tenant and channel name; none of a tenant whose deletion started.
func (s *Store) GetChannel(ctx context.Context, tenant, channel string) (Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? AND c.name = ? AND t.deleting_at IS NULL"), tenant, channel))
	return c, s.closedOr(ctx, tenant, notFound(err, "channel "+tenant+"/"+channel))
}

// closedOr is err, or for a missing row of a tenant whose deletion started, ErrDeleting or
// ErrDeleted (both an ErrNotFound): what the caller is told is the tenant's phase.
func (s *Store) closedOr(ctx context.Context, tenant string, err error) error {
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if t, terr := s.GetTenantAny(ctx, tenant); terr == nil && t.closed() != nil {
		return t.closed()
	}
	return err
}

// ChannelType reads the package type of a tenant's channel, by the tenant's id and the channel's
// name (spec 0034: what an authorization on the channel compares).
func (s *Store) ChannelType(ctx context.Context, tenantID, channel string) (string, error) {
	var t string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT package_type FROM channels WHERE tenant_id = ? AND name = ?"), tenantID, channel).Scan(&t)
	return t, notFound(err, "channel "+channel)
}

// UpstreamChannelType reads the package type of a tenant's upstream, by the tenant's id and the
// upstream's name: its home channel's (spec 0034).
func (s *Store) UpstreamChannelType(ctx context.Context, tenantID, upstream string) (string, error) {
	var t string
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT c.package_type FROM upstreams u JOIN channels c ON c.id = u.channel_id
WHERE u.tenant_id = ? AND u.name = ?`), tenantID, upstream).Scan(&t)
	return t, notFound(err, "upstream "+upstream)
}

// KnownChannels keeps the channels of the types this binary registers (spec 0034: a channel of
// another type, written by a newer binary, is read as nothing where it would be exported, counted,
// fed or listed).
func KnownChannels(chs []Channel) []Channel {
	return slices.DeleteFunc(chs, func(c Channel) bool { return !pkgtype.Known(c.PackageType) })
}

// ListChannels returns a tenant's channels by name, of every type (KnownChannels narrows them).
func (s *Store) ListChannels(ctx context.Context, tenant string) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? AND t.deleting_at IS NULL ORDER BY c.name"), tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChannelVersions returns the names of the DuckDB versions a channel serves.
func (s *Store) ChannelVersions(ctx context.Context, channelID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT v.name FROM channel_duckdb_versions cv
JOIN duckdb_versions v ON v.id = cv.duckdb_version_id WHERE cv.channel_id = ? ORDER BY v.name`), channelID)
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

const keyCols = `id, tenant_id, channel_id, fingerprint, signer_ref, public_key, state, trusted_since,
state_changed_at, created_at, created_by, state_changed_by, version`

func scanKey(r interface{ Scan(...any) error }) (Key, error) {
	var k Key
	err := r.Scan(&k.ID, &k.TenantID, &k.ChannelID, &k.Fingerprint, &k.SignerRef, &k.PublicKey, &k.State,
		scanTime{&k.TrustedSince}, scanTime{&k.StateChangedAt}, scanTime{&k.CreatedAt}, &k.CreatedBy,
		&k.StateChangedBy, &k.Version)
	return k, err
}

func collectKeys(rows *sql.Rows, err error) ([]Key, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListKeys returns a channel's keys, oldest first.
func (s *Store) ListKeys(ctx context.Context, channelID string) ([]Key, error) {
	return collectKeys(s.db.QueryContext(ctx, s.d.rebind("SELECT "+keyCols+
		" FROM channel_keys WHERE channel_id = ? ORDER BY created_at, id"), channelID))
}

// KeyEvents returns the transitions of a channel's keys, oldest first.
func (s *Store) KeyEvents(ctx context.Context, channelID string) ([]KeyEvent, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT e.id, e.key_id, e.from_state, e.to_state, e.actor, e.forced, e.at
FROM key_events e JOIN channel_keys k ON k.id = e.key_id WHERE k.channel_id = ? ORDER BY e.at, e.id`), channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyEvent
	for rows.Next() {
		var e KeyEvent
		var from sql.NullString
		if err := rows.Scan(&e.ID, &e.KeyID, &from, &e.To, &e.Actor, &e.Forced, scanTime{&e.At}); err != nil {
			return nil, err
		}
		e.From = from.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- writes (inside transactions) ---

// CreateTenant inserts a tenant (ID, CreatedAt and Version are set here).
func (t *Tx) CreateTenant(ctx context.Context, tn *Tenant) error {
	if err := ValidName(tn.Name); err != nil {
		return err
	}
	if len(tn.DisplayName) > 200 || strings.ContainsFunc(tn.DisplayName, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) || !utf8.ValidString(tn.DisplayName) {
		return fmt.Errorf("%w: a display name is at most 200 bytes of printable text", ErrInvalid)
	}
	if tn.State == "" {
		tn.State = TenantActive
	}
	if tn.StorageDomain == "" {
		tn.StorageDomain = DefaultDomain
	}
	if err := ValidDomain(tn.StorageDomain); err != nil {
		return err
	}
	if tn.HolderID != "" {
		if err := ValidHolderID(tn.HolderID); err != nil {
			return err
		}
	}
	tn.ID, tn.CreatedAt, tn.Version, tn.DistributorHold = NewID(), t.Now(), 1, DefaultDistributorHold // the column's default
	tn.Limits = DefaultLimits()                                                                       // spec 0038: set apart (SetTenantLimits)
	_, err := t.exec(ctx, "INSERT INTO tenants (id, name, display_name, state, storage_domain, created_at, version, holder_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		tn.ID, tn.Name, tn.DisplayName, tn.State, tn.StorageDomain, t.s.d.timeArg(tn.CreatedAt), tn.Version, nullable(tn.HolderID))
	return t.s.mapErr(err, "tenant "+tn.Name)
}

// GetTenant reads a tenant inside the transaction: ErrDeleting or ErrDeleted once its deletion
// started, as Store.GetTenant.
// It holds the row with a shared lock until commit (spec 0019: the deletion transition waits for
// the transaction, so a write it makes never lands after the tenant closed).
func (t *Tx) GetTenant(ctx context.Context, name string) (Tenant, error) {
	return t.getTenant(ctx, "name = ?", name, false)
}

// GetTenantForUpdate is GetTenant for a transaction that then updates the tenant row: an update
// lock, so two such transactions never deadlock upgrading a shared one.
func (t *Tx) GetTenantForUpdate(ctx context.Context, name string) (Tenant, error) {
	return t.getTenant(ctx, "name = ?", name, true)
}

// GetTenantAnyForUpdate reads a tenant by name in any phase, under an update lock: for the
// deletion's own transition.
func (t *Tx) GetTenantAnyForUpdate(ctx context.Context, name string) (Tenant, error) {
	tn, err := scanTenant(t.queryRow(ctx, t.lockedTenantQuery("SELECT "+tenantCols+" FROM tenants", "name = ?", true), name))
	return tn, notFound(err, "tenant "+name)
}

func (t *Tx) getTenant(ctx context.Context, where string, arg any, forUpdate bool) (Tenant, error) {
	tn, err := scanTenant(t.queryRow(ctx, t.lockedTenantQuery("SELECT "+tenantCols+" FROM tenants", where, forUpdate), arg))
	if err == nil {
		err = tn.closed()
	}
	return tn, notFound(err, "tenant")
}

// SetTenantState changes a tenant's state if its version is still tn.Version and its deletion has
// not started: suspending sets suspended_at, resuming clears it (spec 0019).
func (t *Tx) SetTenantState(ctx context.Context, tn *Tenant, state string) error {
	var at time.Time
	if state == TenantSuspended {
		at = t.Now()
	}
	return t.cas(ctx, "UPDATE tenants SET state = ?, suspended_at = ?, version = version + 1 WHERE id = ? AND version = ? AND deleting_at IS NULL",
		[]any{state, t.s.d.nullTimeArg(at), tn.ID, tn.Version}, func() { tn.State = state; tn.SuspendedAt = at; tn.Version++ })
}

// cas runs a compare-and-set update: exactly one row must change.
func (t *Tx) cas(ctx context.Context, q string, args []any, applied func()) error {
	res, err := t.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	applied()
	return nil
}

// AddDuckDBVersion inserts a DuckDB version.
func (t *Tx) AddDuckDBVersion(ctx context.Context, v *DuckDBVersion) error {
	if err := ValidDuckDBVersion(v.Name); err != nil {
		return err
	}
	v.ID, v.CreatedAt = NewID(), t.Now()
	var capi any
	if v.CAPIVersion != "" {
		capi = v.CAPIVersion
	}
	_, err := t.exec(ctx, "INSERT INTO duckdb_versions (id, name, kind, c_api_version, created_at) VALUES (?, ?, ?, ?, ?)",
		v.ID, v.Name, v.Kind, capi, t.s.d.timeArg(v.CreatedAt))
	return t.s.mapErr(err, "DuckDB version "+v.Name)
}

// GetDuckDBVersion reads a DuckDB version by name.
func (t *Tx) GetDuckDBVersion(ctx context.Context, name string) (DuckDBVersion, error) {
	v, err := scanVersion(t.queryRow(ctx, "SELECT "+versionCols+" FROM duckdb_versions WHERE name = ?", name))
	return v, notFound(err, "DuckDB version "+name)
}

// CreateChannel inserts a channel: within the tenant's channels limit (spec 0038; the caller holds
// the tenant's row with an update lock, which orders its channel creates) and MaxChannels.
func (t *Tx) CreateChannel(ctx context.Context, c *Channel) error {
	if err := ValidName(c.Name); err != nil {
		return err
	}
	if c.PackageType == "" {
		c.PackageType = pkgtype.Default
	}
	if !pkgtype.Known(c.PackageType) {
		return fmt.Errorf("%w: package type %q", ErrInvalid, c.PackageType)
	}
	if c.Kind == ChannelPassthrough && c.PackageType != pkgtype.DuckDBExtension {
		return fmt.Errorf("%w: a passthrough channel serves DuckDB extensions only", ErrInvalid)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM channels WHERE tenant_id = ?", c.TenantID).Scan(&n); err != nil {
		return err
	}
	if err := t.underLimit(ctx, c.TenantID, LimitChannels, n); err != nil {
		return err
	}
	if n >= MaxChannels {
		return fmt.Errorf("%w: a tenant holds at most %d channels", ErrInvalid, MaxChannels)
	}
	c.ID, c.CreatedAt, c.Version = NewID(), t.Now(), 1
	_, err := t.exec(ctx, "INSERT INTO channels (id, tenant_id, name, kind, created_at, version, package_type) VALUES (?, ?, ?, ?, ?, ?, ?)",
		c.ID, c.TenantID, c.Name, c.Kind, t.s.d.timeArg(c.CreatedAt), c.Version, c.PackageType)
	return t.s.mapErr(err, "channel "+c.Name)
}

// GetChannel reads a channel by tenant and channel name inside the transaction.
func (t *Tx) GetChannel(ctx context.Context, tenant, channel string) (Channel, error) {
	c, err := scanChannel(t.queryRow(ctx, "SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? AND c.name = ?", tenant, channel))
	if err == nil {
		err = t.WritableTenant(ctx, c.TenantID, false) // spec 0019: the tenant held open until commit
	}
	return c, notFound(err, "channel "+tenant+"/"+channel)
}

// BumpChannel increments a channel's version (compare-and-set): every key or DuckDB-version change
// does it in the same transaction, so caches keyed by the version see the change.
func (t *Tx) BumpChannel(ctx context.Context, c *Channel) error {
	return t.cas(ctx, "UPDATE channels SET version = version + 1 WHERE id = ? AND version = ?",
		[]any{c.ID, c.Version}, func() { c.Version++ })
}

// AddChannelVersion makes a channel serve a DuckDB version.
func (t *Tx) AddChannelVersion(ctx context.Context, channelID, versionID string) error {
	_, err := t.exec(ctx, "INSERT INTO channel_duckdb_versions (channel_id, duckdb_version_id) VALUES (?, ?)", channelID, versionID)
	if err != nil {
		return t.s.mapErr(err, "channel version")
	}
	return t.RecheckRules(ctx, channelID) // spec 0033: the builds for the version are placed by the rules
}

// RemoveChannelVersion stops a channel serving a DuckDB version; ErrNotFound if it did not.
func (t *Tx) RemoveChannelVersion(ctx context.Context, channelID, versionID string) error {
	res, err := t.exec(ctx, "DELETE FROM channel_duckdb_versions WHERE channel_id = ? AND duckdb_version_id = ?", channelID, versionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: the channel does not serve that version", ErrNotFound)
	}
	// spec 0032: the asks for that version go with it
	_, err = t.exec(ctx, `DELETE FROM upstream_asks WHERE upstream_id IN (SELECT id FROM upstreams WHERE channel_id = ?)
AND duckdb_version = (SELECT name FROM duckdb_versions WHERE id = ?)`, channelID, versionID)
	return err
}

// ChannelKeys reads a channel's keys inside the transaction, oldest first.
func (t *Tx) ChannelKeys(ctx context.Context, channelID string) ([]Key, error) {
	return collectKeys(t.query(ctx, "SELECT "+keyCols+" FROM channel_keys WHERE channel_id = ? ORDER BY created_at, id", channelID))
}

// InsertKey inserts a key, registers its fingerprint server-wide, and records the event.
func (t *Tx) InsertKey(ctx context.Context, k *Key, actor string, forced bool) error {
	now := t.Now()
	k.ID, k.CreatedAt, k.StateChangedAt, k.TrustedSince, k.Version = NewID(), now, now, now, 1
	k.CreatedBy, k.StateChangedBy = actor, actor
	if _, err := t.exec(ctx, "INSERT INTO key_fingerprints (fingerprint, key_id) VALUES (?, ?)", k.Fingerprint, k.ID); err != nil {
		return t.s.mapErr(err, "key fingerprint")
	}
	_, err := t.exec(ctx, `INSERT INTO channel_keys (id, tenant_id, channel_id, fingerprint, signer_ref, public_key, state,
trusted_since, state_changed_at, created_at, created_by, state_changed_by, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.TenantID, k.ChannelID, k.Fingerprint, k.SignerRef, k.PublicKey, k.State, t.s.d.timeArg(k.TrustedSince),
		t.s.d.timeArg(k.StateChangedAt), t.s.d.timeArg(k.CreatedAt), k.CreatedBy, k.StateChangedBy, k.Version)
	if err != nil {
		return t.s.mapErr(err, "key")
	}
	return t.event(ctx, k.ID, "", k.State, actor, forced, now)
}

// SetKeyState moves a key to a new state (compare-and-set on its version) and records the event.
// Becoming trusted never resets trusted_since.
func (t *Tx) SetKeyState(ctx context.Context, k *Key, state, actor string, forced bool) error {
	now := t.Now()
	from := k.State
	err := t.cas(ctx, "UPDATE channel_keys SET state = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{state, t.s.d.timeArg(now), actor, k.ID, k.Version},
		func() { k.State, k.StateChangedAt, k.StateChangedBy = state, now, actor; k.Version++ })
	if err != nil {
		return t.s.mapErr(err, "key state")
	}
	return t.event(ctx, k.ID, from, state, actor, forced, now)
}

func (t *Tx) event(ctx context.Context, keyID, from, to, actor string, forced bool, at time.Time) error {
	var f any
	if from != "" {
		f = from
	}
	_, err := t.exec(ctx, "INSERT INTO key_events (id, key_id, from_state, to_state, actor, forced, at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		NewID(), keyID, f, to, actor, forced, t.s.d.timeArg(at))
	return err
}
