package store

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// Publisher is an identity of a tenant that only publishes and promotes (spec 0008), authenticated
// by its keys (spec 0023).
type Publisher struct {
	ID, TenantID, Name string
	CreatedAt          time.Time
	CreatedBy          string
	// LicenseKeys marks it a trusted source of licence keys (spec 0021), a server administrator's act.
	LicenseKeys bool
	// Hold is its hold (spec 0033): an entry it publishes is held until its arrival plus the hold,
	// and it publishes into the catalogue only; 0 for none.
	Hold time.Duration
}

// MaxPublishers is the most publishers a tenant may hold.
const MaxPublishers = 200

var publisherName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// InsertPublisher adds a publisher.
func (t *Tx) InsertPublisher(ctx context.Context, p *Publisher) error {
	if !publisherName.MatchString(p.Name) {
		return fmt.Errorf("%w: publisher name %q must match [a-z][a-z0-9-]{0,63}", ErrInvalid, p.Name)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM publishers WHERE tenant_id = ?", p.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPublishers {
		return fmt.Errorf("%w: a tenant holds at most %d publishers", ErrInvalid, MaxPublishers)
	}
	p.ID, p.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, "INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, ?, ?, ?)",
		p.ID, p.TenantID, p.Name, t.s.d.timeArg(p.CreatedAt), p.CreatedBy)
	if err := t.s.mapErr(err, "publisher "+p.Name); err != nil {
		return err
	}
	return t.BumpAuth(ctx, p.TenantID)
}

// GetPublisher reads a tenant's publisher by name.
func (t *Tx) GetPublisher(ctx context.Context, tenantID, name string) (Publisher, error) {
	var p Publisher
	var hold int64
	err := t.queryRow(ctx, "SELECT id, tenant_id, name, created_at, created_by, license_keys, hold_seconds FROM publishers WHERE tenant_id = ? AND name = ?",
		tenantID, name).Scan(&p.ID, &p.TenantID, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy, &p.LicenseKeys, &hold)
	p.Hold = time.Duration(hold) * time.Second
	return p, notFound(err, "publisher "+name)
}

// PublisherCredentials counts a publisher's keys.
func (t *Tx) PublisherCredentials(ctx context.Context, publisherID string) (int, error) {
	var n int
	err := t.queryRow(ctx, "SELECT COUNT(*) FROM publisher_keys WHERE publisher_id = ?", publisherID).Scan(&n)
	return n, err
}

// PublisherGrants counts a publisher's grants, those on extension groups included.
func (t *Tx) PublisherGrants(ctx context.Context, publisherID string) (int, error) {
	var n int
	err := t.queryRow(ctx, `SELECT (SELECT COUNT(*) FROM grants WHERE publisher_id = ?) +
(SELECT COUNT(*) FROM package_group_grants WHERE publisher_id = ?)`, publisherID, publisherID).Scan(&n)
	return n, err
}

// SetPublisherLicenseKeys marks a publisher a trusted source of licence keys, or unmarks it
// (spec 0021). Unmarking keeps the keys it brought; removing the publisher drops them.
func (t *Tx) SetPublisherLicenseKeys(ctx context.Context, tenantID, name string, on bool) (Publisher, error) {
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err != nil {
		return p, err
	}
	if _, err := t.exec(ctx, "UPDATE publishers SET license_keys = ? WHERE id = ?", on, p.ID); err != nil {
		return p, err
	}
	p.LicenseKeys = on
	return p, t.BumpAuth(ctx, tenantID)
}

// DeletePublisher removes a publisher, its credentials and its grants.
func (t *Tx) DeletePublisher(ctx context.Context, tenantID, name string) error {
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err != nil {
		return err
	}
	for _, q := range []string{"DELETE FROM grants WHERE publisher_id = ?",
		"DELETE FROM package_group_grants WHERE publisher_id = ?", "DELETE FROM publisher_keys WHERE publisher_id = ?",
		"DELETE FROM publishers WHERE id = ?"} {
		if _, err := t.exec(ctx, q, p.ID); err != nil {
			return err
		}
	}
	if err := t.DeleteLicenseKeysOf(ctx, KeySourcePublisher, p.ID); err != nil {
		return err
	}
	if err := t.dropFollows(ctx, PublisherSource(p.ID)); err != nil { // spec 0033: nothing follows a gone source
		return err
	}
	return t.BumpAuth(ctx, tenantID)
}

// publishers reads a tenant's publishers, by name.
func publishers(ctx context.Context, query queryFn, tenantID string) ([]Publisher, error) {
	rows, err := query(ctx, `SELECT id, tenant_id, name, created_at, created_by, license_keys, hold_seconds FROM publishers WHERE tenant_id = ?
ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Publisher
	for rows.Next() {
		var p Publisher
		var hold int64
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy, &p.LicenseKeys, &hold); err != nil {
			return nil, err
		}
		p.Hold = time.Duration(hold) * time.Second
		out = append(out, p)
	}
	return out, rows.Err()
}

// IssuerURLs lists every tenant's issuer records as (tenant id, name, URL): kista serve warns about
// records at GitHub Actions' URL, which are never used (spec 0008, 0023).
func (s *Store) IssuerURLs(ctx context.Context) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT tenant_id, name, url FROM issuers ORDER BY tenant_id, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r [3]string
		if err := rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PublisherKey is a publisher's public SSH key (spec 0023): its signed requests authenticate the
// publisher. Only the public key is stored.
type PublisherKey struct {
	ID, TenantID, PublisherID string
	Type                      string // ssh-ed25519 or ecdsa-sha2-nistp256
	PublicKey                 string // the OpenSSH line without a comment
	Fingerprint               string // SHA256:<unpadded base64>
	Title                     string
	ExpiresAt                 time.Time // zero: none
	CreatedAt                 time.Time
	CreatedBy                 string
	LastUsedAt                time.Time // zero: never used
}

// MaxPublisherKeys is the most keys one publisher may hold, expired ones included.
const MaxPublisherKeys = 10

var fingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// InsertPublisherKey adds a key to a publisher: a fingerprint is unique in the tenant. Callers hold
// the tenant's auth lock, so the cap holds.
func (t *Tx) InsertPublisherKey(ctx context.Context, k *PublisherKey) error {
	if !fingerprint.MatchString(k.Fingerprint) || k.PublicKey == "" || len(k.PublicKey) > 400 || len(k.Title) > 100 {
		return fmt.Errorf("%w: a key", ErrInvalid)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM publisher_keys WHERE publisher_id = ?", k.PublisherID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPublisherKeys {
		return fmt.Errorf("%w: a publisher holds at most %d keys, expired ones included", ErrInvalid, MaxPublisherKeys)
	}
	k.ID, k.CreatedAt = NewID(), t.Now()
	var exp any
	if !k.ExpiresAt.IsZero() {
		exp = t.s.d.timeArg(k.ExpiresAt)
	}
	_, err := t.exec(ctx, `INSERT INTO publisher_keys (id, tenant_id, publisher_id, type, public_key, fingerprint, title, expires_at,
created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, k.ID, k.TenantID, k.PublisherID, k.Type, k.PublicKey, k.Fingerprint,
		k.Title, exp, t.s.d.timeArg(k.CreatedAt), k.CreatedBy)
	return t.s.mapErr(err, "key "+k.Fingerprint+" in this tenant")
}

// DeletePublisherKey removes a publisher's key and returns it. Keys are looked up on every request,
// never cached, so no cache is bumped.
func (t *Tx) DeletePublisherKey(ctx context.Context, publisherID, id string) (PublisherKey, error) {
	k, err := scanPublisherKey(t.queryRow(ctx, "SELECT "+publisherKeyCols+" FROM publisher_keys WHERE id = ? AND publisher_id = ?", id, publisherID))
	if err := notFound(err, "key "+id); err != nil {
		return k, err
	}
	_, err = t.exec(ctx, "DELETE FROM publisher_keys WHERE id = ? AND publisher_id = ?", id, publisherID)
	return k, err
}

const publisherKeyCols = "id, tenant_id, publisher_id, type, public_key, fingerprint, title, expires_at, created_at, created_by, last_used_at"

func scanPublisherKey(r interface{ Scan(...any) error }, extra ...any) (PublisherKey, error) {
	var k PublisherKey
	err := r.Scan(append([]any{&k.ID, &k.TenantID, &k.PublisherID, &k.Type, &k.PublicKey, &k.Fingerprint, &k.Title,
		scanTime{&k.ExpiresAt}, scanTime{&k.CreatedAt}, &k.CreatedBy, scanTime{&k.LastUsedAt}}, extra...)...)
	return k, err
}

// PublisherKeys lists a tenant's keys, of one publisher when publisherID is set, oldest first.
func (s *Store) PublisherKeys(ctx context.Context, tenantID, publisherID string) ([]PublisherKey, error) {
	q, args := "SELECT "+publisherKeyCols+" FROM publisher_keys WHERE tenant_id = ?", []any{tenantID}
	if publisherID != "" {
		q, args = q+" AND publisher_id = ?", append(args, publisherID)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q+" ORDER BY created_at, id"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublisherKey
	for rows.Next() {
		k, err := scanPublisherKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PublisherKeyByFingerprint finds a tenant's key by its fingerprint, with its publisher
// (ErrNotFound for none).
func (s *Store) PublisherKeyByFingerprint(ctx context.Context, tenantID, fp string) (PublisherKey, Publisher, error) {
	var p Publisher
	row := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT k.id, k.tenant_id, k.publisher_id, k.type, k.public_key, k.fingerprint,
k.title, k.expires_at, k.created_at, k.created_by, k.last_used_at, p.name, p.created_at, p.created_by, p.license_keys, p.hold_seconds
FROM publisher_keys k JOIN publishers p ON p.id = k.publisher_id WHERE k.tenant_id = ? AND k.fingerprint = ?`), tenantID, fp)
	var hold int64
	k, err := scanPublisherKey(row, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy, &p.LicenseKeys, &hold)
	p.ID, p.TenantID, p.Hold = k.PublisherID, k.TenantID, time.Duration(hold)*time.Second
	return k, p, notFound(err, "key")
}

// TouchPublisherKey records a key's use (callers do it at most once an hour).
func (s *Store) TouchPublisherKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.d.rebind("UPDATE publisher_keys SET last_used_at = ? WHERE id = ?"), s.d.timeArg(s.Now()), id)
	return err
}
