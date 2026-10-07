package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tenant states.
const (
	TenantActive    = "active"
	TenantSuspended = "suspended"
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
	CreatedAt                    time.Time
	Version                      int64
}

// DuckDBVersion is a DuckDB engine version: a release tag or a dev source id.
type DuckDBVersion struct {
	ID, Name, Kind string
	CAPIVersion    string // empty if unknown
	CreatedAt      time.Time
}

// Channel is one DuckDB repository inside a tenant.
type Channel struct {
	ID, TenantID, Name, Kind string
	CreatedAt                time.Time
	Version                  int64
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

const tenantCols = "id, name, display_name, state, storage_domain, created_at, version"

func scanTenant(r interface{ Scan(...any) error }) (Tenant, error) {
	var t Tenant
	err := r.Scan(&t.ID, &t.Name, &t.DisplayName, &t.State, &t.StorageDomain, scanTime{&t.CreatedAt}, &t.Version)
	return t, err
}

func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

// GetTenant returns a tenant by name.
func (s *Store) GetTenant(ctx context.Context, name string) (Tenant, error) {
	t, err := scanTenant(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+tenantCols+" FROM tenants WHERE name = ?"), name))
	return t, notFound(err, "tenant "+name)
}

// ListTenants returns all tenants by name.
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

// ListDuckDBVersions returns all known DuckDB versions by name.
func (s *Store) ListDuckDBVersions(ctx context.Context) ([]DuckDBVersion, error) {
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
		out = append(out, v)
	}
	return out, rows.Err()
}

const channelCols = "c.id, c.tenant_id, c.name, c.kind, c.created_at, c.version"

func scanChannel(r interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	err := r.Scan(&c.ID, &c.TenantID, &c.Name, &c.Kind, scanTime{&c.CreatedAt}, &c.Version)
	return c, err
}

// GetChannel returns a channel by tenant and channel name.
func (s *Store) GetChannel(ctx context.Context, tenant, channel string) (Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? AND c.name = ?"), tenant, channel))
	return c, notFound(err, "channel "+tenant+"/"+channel)
}

// ListChannels returns a tenant's channels by name.
func (s *Store) ListChannels(ctx context.Context, tenant string) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? ORDER BY c.name"), tenant)
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
	if tn.State == "" {
		tn.State = TenantActive
	}
	if tn.StorageDomain == "" {
		tn.StorageDomain = DefaultDomain
	}
	if err := ValidDomain(tn.StorageDomain); err != nil {
		return err
	}
	tn.ID, tn.CreatedAt, tn.Version = NewID(), t.Now(), 1
	_, err := t.exec(ctx, "INSERT INTO tenants (id, name, display_name, state, storage_domain, created_at, version) VALUES (?, ?, ?, ?, ?, ?, ?)",
		tn.ID, tn.Name, tn.DisplayName, tn.State, tn.StorageDomain, t.s.d.timeArg(tn.CreatedAt), tn.Version)
	return t.s.mapErr(err, "tenant "+tn.Name)
}

// GetTenant reads a tenant inside the transaction.
func (t *Tx) GetTenant(ctx context.Context, name string) (Tenant, error) {
	tn, err := scanTenant(t.queryRow(ctx, "SELECT "+tenantCols+" FROM tenants WHERE name = ?", name))
	return tn, notFound(err, "tenant "+name)
}

// SetTenantState changes a tenant's state if its version is still tn.Version.
func (t *Tx) SetTenantState(ctx context.Context, tn *Tenant, state string) error {
	return t.cas(ctx, "UPDATE tenants SET state = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{state, tn.ID, tn.Version}, func() { tn.State = state; tn.Version++ })
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

// CreateChannel inserts a channel.
func (t *Tx) CreateChannel(ctx context.Context, c *Channel) error {
	if err := ValidName(c.Name); err != nil {
		return err
	}
	c.ID, c.CreatedAt, c.Version = NewID(), t.Now(), 1
	_, err := t.exec(ctx, "INSERT INTO channels (id, tenant_id, name, kind, created_at, version) VALUES (?, ?, ?, ?, ?, ?)",
		c.ID, c.TenantID, c.Name, c.Kind, t.s.d.timeArg(c.CreatedAt), c.Version)
	return t.s.mapErr(err, "channel "+c.Name)
}

// GetChannel reads a channel by tenant and channel name inside the transaction.
func (t *Tx) GetChannel(ctx context.Context, tenant, channel string) (Channel, error) {
	c, err := scanChannel(t.queryRow(ctx, "SELECT "+channelCols+
		" FROM channels c JOIN tenants t ON t.id = c.tenant_id WHERE t.name = ? AND c.name = ?", tenant, channel))
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
	return t.s.mapErr(err, "channel version")
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
	return nil
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
