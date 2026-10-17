package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Bootstrap is the record of the configuration's bootstrap, applied once (spec 0020 phase 4a): its
// state (pending from before its first write, then applied), its tenant by name, the digest of the
// section applied and of the last one seen, and the section.
type Bootstrap struct {
	State, Tenant, Digest, SeenDigest, Section string
	AppliedAt                                  time.Time // when it was applied, or began (pending)
	Actor                                      string
}

// The bootstrap's states.
const (
	BootstrapPending = "pending"
	BootstrapApplied = "applied"
)

// BootstrapLease is the lease the bootstrap holds while it applies.
const BootstrapLease = "kista/bootstrap"

// GetBootstrap reads the bootstrap's record; ErrNotFound when it was never applied.
func (s *Store) GetBootstrap(ctx context.Context) (Bootstrap, error) {
	var b Bootstrap
	err := s.db.QueryRowContext(ctx, "SELECT state, tenant, digest, seen_digest, section, applied_at, actor FROM bootstrap WHERE id = 1").
		Scan(&b.State, &b.Tenant, &b.Digest, &b.SeenDigest, &b.Section, scanTime{&b.AppliedAt}, &b.Actor)
	return b, notFound(err, "bootstrap")
}

// InsertBootstrap records a bootstrap begun (pending); ErrExists when there is a record.
func (s *Store) InsertBootstrap(ctx context.Context, b *Bootstrap) error {
	b.State, b.AppliedAt = BootstrapPending, s.Now()
	_, err := s.db.ExecContext(ctx, s.d.rebind(`INSERT INTO bootstrap (id, state, tenant, digest, seen_digest, section, applied_at, actor)
VALUES (1, ?, ?, ?, ?, ?, ?, ?)`), b.State, b.Tenant, b.Digest, b.SeenDigest, b.Section, s.d.timeArg(b.AppliedAt), b.Actor)
	return s.mapErr(err, "bootstrap")
}

// InsertPendingBootstrap is InsertBootstrap in a transaction.
func (t *Tx) InsertPendingBootstrap(ctx context.Context, b *Bootstrap) error {
	b.State, b.AppliedAt = BootstrapPending, t.Now()
	_, err := t.exec(ctx, `INSERT INTO bootstrap (id, state, tenant, digest, seen_digest, section, applied_at, actor)
VALUES (1, ?, ?, ?, ?, ?, ?, ?)`, b.State, b.Tenant, b.Digest, b.SeenDigest, b.Section, t.s.d.timeArg(b.AppliedAt), b.Actor)
	return t.s.mapErr(err, "bootstrap")
}

// ApplyBootstrap records the pending bootstrap applied, with the section it applied; ErrExists when
// it is not pending (another replica recorded it).
func (t *Tx) ApplyBootstrap(ctx context.Context, b *Bootstrap) error {
	b.State, b.AppliedAt = BootstrapApplied, t.Now()
	res, err := t.exec(ctx, `UPDATE bootstrap SET state = ?, tenant = ?, digest = ?, seen_digest = ?, section = ?, applied_at = ?, actor = ?
WHERE id = 1 AND state = ?`, b.State, b.Tenant, b.Digest, b.SeenDigest, b.Section, t.s.d.timeArg(b.AppliedAt), b.Actor, BootstrapPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: bootstrap: not pending", ErrExists)
	}
	return nil
}

// SeeBootstrap records the digest of the section last seen (a changed section is warned about once).
func (s *Store) SeeBootstrap(ctx context.Context, digest string) error {
	_, err := s.db.ExecContext(ctx, s.d.rebind("UPDATE bootstrap SET seen_digest = ? WHERE id = 1"), digest)
	return err
}

// AcceptBootstrap takes a changed section as the applied one, without applying it.
func (t *Tx) AcceptBootstrap(ctx context.Context, digest, section string) error {
	_, err := t.exec(ctx, "UPDATE bootstrap SET digest = ?, seen_digest = ?, section = ? WHERE id = 1", digest, digest, section)
	return err
}

// DeleteBootstrap removes the record, so the next start applies the section again.
func (t *Tx) DeleteBootstrap(ctx context.Context) (bool, error) {
	res, err := t.exec(ctx, "DELETE FROM bootstrap WHERE id = 1")
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FingerprintKey answers the key (a channel's or a tenant's attestation key) a fingerprint is
// registered to, server-wide; "" when none.
func (s *Store) FingerprintKey(ctx context.Context, fingerprint string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT key_id FROM key_fingerprints WHERE fingerprint = ?"), fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// AudienceTenant answers the tenant an audience is assigned to; "" when none.
func (s *Store) AudienceTenant(ctx context.Context, audience string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT tenant_id FROM tenant_audiences WHERE audience = ?"), audience).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
