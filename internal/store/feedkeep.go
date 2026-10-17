package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// What an export feed keeps for a site's plan (spec 0014 phase 1b): its source's attestation keys
// file, the statements it refused for good, and statements about documents the tenant does not
// hold (a withdrawal of one never stored, any statement about a removed one).

// MaxFeedKept caps what a tenant's export feeds keep (refusals and statements together), apart from
// the documents quota, so a source flooding it never costs another source a document (spec 0014).
const MaxFeedKept = 100000

// ErrQuota is MaxFeedKept reached; it wraps into ErrInvalid.
var ErrQuota = errors.New("store: what export feeds keep is at its cap")

// FeedRefusal is a statement an export feed refused for good, with its bytes and signature.
type FeedRefusal struct {
	FeedID, BodyHash string
	Seq              int64
	Reason           string
	Statement        string
	Signature        string // base64
	RefusedAt        time.Time
}

// FeedKept is a statement about a document the tenant does not hold, kept for export.
type FeedKept struct {
	TenantID, Issuer, IssuerID, BodyHash string
	Seq                                  int64
	Statement                            string
	Signature                            string // base64
	KeptAt                               time.Time
}

// feedKept counts what the tenant's export feeds keep: refusals and statements.
func (t *Tx) feedKept(ctx context.Context, tenantID string) (int, error) {
	var refused, kept int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feed_refusals WHERE feed_id IN (SELECT id FROM feeds WHERE tenant_id = ?)",
		tenantID).Scan(&refused); err != nil {
		return 0, err
	}
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feed_kept WHERE tenant_id = ?", tenantID).Scan(&kept); err != nil {
		return 0, err
	}
	return refused + kept, nil
}

func quotaErr() error {
	return fmt.Errorf("%w: %w: export feeds keep at most %d refusals and statements per tenant", ErrInvalid, ErrQuota, MaxFeedKept)
}

// FeedEpochIs reports whether a feed's export setting is still the one a run started with: a run
// writes cursors and keeps statements only then (the caller holds the tenant's feed lock, which a
// change of export takes too).
func (t *Tx) FeedEpochIs(ctx context.Context, feedID string, epoch int64) (bool, error) {
	var cur int64
	if err := t.queryRow(ctx, "SELECT export_epoch FROM feeds WHERE id = ?", feedID).Scan(&cur); err != nil {
		return false, notFound(err, "feed")
	}
	return cur == epoch, nil
}

// SetFeedKeysFile keeps an export feed's copy of its source's attestation keys file.
func (t *Tx) SetFeedKeysFile(ctx context.Context, feedID string, b []byte) error {
	if len(b) > MaxFeedKeysFile {
		return fmt.Errorf("%w: the attestation keys file is over %d bytes", ErrInvalid, MaxFeedKeysFile)
	}
	_, err := t.exec(ctx, "UPDATE feeds SET attestation_keys = ?, attestation_keys_at = ? WHERE id = ? AND export = ?",
		string(b), t.s.d.timeArg(t.Now()), feedID, true)
	return err
}

// FeedKeysFile reads an export feed's copy of its source's keys file (nil: none kept).
func (t *Tx) FeedKeysFile(ctx context.Context, feedID string) ([]byte, time.Time, error) {
	var b *string
	var at time.Time
	if err := t.queryRow(ctx, "SELECT attestation_keys, attestation_keys_at FROM feeds WHERE id = ?", feedID).Scan(&b, scanTime{&at}); err != nil {
		return nil, at, notFound(err, "feed")
	}
	if b == nil {
		return nil, at, nil
	}
	return []byte(*b), at, nil
}

// ResetFeedCursors deletes a feed's cursors, so its next run reads the whole history again.
func (t *Tx) ResetFeedCursors(ctx context.Context, feedID string) error {
	_, err := t.exec(ctx, "DELETE FROM feed_cursors WHERE feed_id = ?", feedID)
	return err
}

// KeepFeedRefusal keeps a statement an export feed refused for good. The caller holds the tenant's
// attachment lock: the quota is counted here.
func (t *Tx) KeepFeedRefusal(ctx context.Context, tenantID string, r FeedRefusal) error {
	var have int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feed_refusals WHERE feed_id = ? AND body_hash = ? AND seq = ?",
		r.FeedID, r.BodyHash, r.Seq).Scan(&have); err != nil || have > 0 {
		return err
	}
	n, err := t.feedKept(ctx, tenantID)
	if err != nil {
		return err
	}
	if n >= MaxFeedKept {
		return quotaErr()
	}
	_, err = t.exec(ctx, "INSERT INTO feed_refusals (feed_id, body_hash, seq, reason, statement, signature, refused_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		r.FeedID, r.BodyHash, r.Seq, r.Reason, r.Statement, r.Signature, t.s.d.timeArg(t.Now()))
	return t.s.mapErr(err, "feed refusal")
}

// FeedRefusals lists a feed's refusals for a body with seq in (after, upTo], in seq order.
func (s *Store) FeedRefusals(ctx context.Context, feedID, bodyHash string, after, upTo int64) ([]FeedRefusal, error) {
	var out []FeedRefusal
	err := s.eachRow(ctx, `SELECT feed_id, body_hash, seq, reason, statement, signature, refused_at FROM feed_refusals
WHERE feed_id = ? AND body_hash = ? AND seq > ? AND seq <= ? ORDER BY seq`, []any{feedID, bodyHash, after, upTo}, func(sc func(...any) error) error {
		var r FeedRefusal
		if err := sc(&r.FeedID, &r.BodyHash, &r.Seq, &r.Reason, &r.Statement, &r.Signature, scanTime{&r.RefusedAt}); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// KeepFeedStatement keeps a statement about a document the tenant does not hold: per document, the
// highest seq. It answers whether the row was added or raised. The caller holds the tenant's
// attachment lock: the cap is counted here.
func (t *Tx) KeepFeedStatement(ctx context.Context, k FeedKept) (bool, error) {
	cur, ok, err := t.GetFeedKept(ctx, k.TenantID, k.Issuer, k.IssuerID)
	switch {
	case err != nil:
		return false, err
	case ok && cur.Seq >= k.Seq:
		return false, nil
	case ok:
		_, err := t.exec(ctx, `UPDATE feed_kept SET body_hash = ?, seq = ?, statement = ?, signature = ?, kept_at = ?
WHERE tenant_id = ? AND issuer = ? AND issuer_id = ? AND seq < ?`, k.BodyHash, k.Seq, k.Statement, k.Signature, t.s.d.timeArg(t.Now()),
			k.TenantID, k.Issuer, k.IssuerID, k.Seq)
		return err == nil, err
	}
	n, err := t.feedKept(ctx, k.TenantID)
	if err != nil {
		return false, err
	}
	if n >= MaxFeedKept {
		return false, quotaErr()
	}
	_, err = t.exec(ctx, `INSERT INTO feed_kept (tenant_id, issuer, issuer_id, body_hash, seq, statement, signature, kept_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, k.TenantID, k.Issuer, k.IssuerID, k.BodyHash, k.Seq, k.Statement, k.Signature, t.s.d.timeArg(t.Now()))
	return err == nil, t.s.mapErr(err, "kept statement")
}

// GetFeedKept reads the statement kept about a document.
func (t *Tx) GetFeedKept(ctx context.Context, tenantID, issuer, issuerID string) (FeedKept, bool, error) {
	var k FeedKept
	err := t.queryRow(ctx, `SELECT tenant_id, issuer, issuer_id, body_hash, seq, statement, signature, kept_at FROM feed_kept
WHERE tenant_id = ? AND issuer = ? AND issuer_id = ?`, tenantID, issuer, issuerID).Scan(&k.TenantID, &k.Issuer, &k.IssuerID, &k.BodyHash,
		&k.Seq, &k.Statement, &k.Signature, scanTime{&k.KeptAt})
	if err := notFound(err, "kept statement"); errors.Is(err, ErrNotFound) {
		return k, false, nil
	} else if err != nil {
		return k, false, err
	}
	return k, true, nil
}

// DeleteFeedKept deletes the statement kept about a document (it is held now).
func (t *Tx) DeleteFeedKept(ctx context.Context, tenantID, issuer, issuerID string) error {
	_, err := t.exec(ctx, "DELETE FROM feed_kept WHERE tenant_id = ? AND issuer = ? AND issuer_id = ?", tenantID, issuer, issuerID)
	return err
}

// FeedKeptOf lists the statements kept about a body's documents of an issuer with seq in
// (after, upTo], in seq order.
func (s *Store) FeedKeptOf(ctx context.Context, tenantID, issuer, bodyHash string, after, upTo int64) ([]FeedKept, error) {
	var out []FeedKept
	err := s.eachRow(ctx, `SELECT tenant_id, issuer, issuer_id, body_hash, seq, statement, signature, kept_at FROM feed_kept
WHERE tenant_id = ? AND issuer = ? AND body_hash = ? AND seq > ? AND seq <= ? ORDER BY seq`, []any{tenantID, issuer, bodyHash, after, upTo},
		func(sc func(...any) error) error {
			var k FeedKept
			if err := sc(&k.TenantID, &k.Issuer, &k.IssuerID, &k.BodyHash, &k.Seq, &k.Statement, &k.Signature, scanTime{&k.KeptAt}); err != nil {
				return err
			}
			out = append(out, k)
			return nil
		})
	return out, err
}

// DropFeedExport deletes what a feed kept for export: its refusals and keys file, and the
// statements kept about its issuer's documents once no other feed of its URL has export set.
func (t *Tx) DropFeedExport(ctx context.Context, f Feed) error {
	if _, err := t.exec(ctx, "DELETE FROM feed_refusals WHERE feed_id = ?", f.ID); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "UPDATE feeds SET attestation_keys = NULL, attestation_keys_at = NULL WHERE id = ?", f.ID); err != nil {
		return err
	}
	var others int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feeds WHERE tenant_id = ? AND url = ? AND id <> ? AND export = ?",
		f.TenantID, f.URL, f.ID, true).Scan(&others); err != nil || others > 0 {
		return err
	}
	_, err := t.exec(ctx, "DELETE FROM feed_kept WHERE tenant_id = ? AND issuer = ?", f.TenantID, f.URL)
	return err
}
