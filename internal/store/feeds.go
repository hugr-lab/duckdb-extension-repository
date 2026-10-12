package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// The feed (spec 0011 phase 1b): a tenant's attestation keys and the statements it issues (the
// source's side), and the feeds a tenant subscribes to with their cursors (the subscriber's side).

func encodeSig(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeSig(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// AttestationLock orders a tenant's attestation key changes.
func AttestationLock(tenantID string) string { return "kista/attestation-keys/" + tenantID }

const attestationKeyCols = `id, tenant_id, '' AS channel_id, fingerprint, signer_ref, public_key, state, trusted_since,
state_changed_at, created_at, created_by, state_changed_by, version`

// InsertAttestationKey inserts a tenant's attestation key and registers its fingerprint server-wide
// (a key is never a channel key and an attestation key at once, nor two tenants').
func (t *Tx) InsertAttestationKey(ctx context.Context, k *Key, actor string) error {
	now := t.Now()
	k.ID, k.CreatedAt, k.StateChangedAt, k.TrustedSince, k.Version, k.ChannelID = NewID(), now, now, now, 1, ""
	k.CreatedBy, k.StateChangedBy = actor, actor
	if _, err := t.exec(ctx, "INSERT INTO key_fingerprints (fingerprint, key_id) VALUES (?, ?)", k.Fingerprint, k.ID); err != nil {
		return t.s.mapErr(err, "key fingerprint")
	}
	_, err := t.exec(ctx, `INSERT INTO attestation_keys (id, tenant_id, fingerprint, signer_ref, public_key, state, trusted_since,
state_changed_at, created_at, created_by, state_changed_by, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.TenantID, k.Fingerprint, k.SignerRef, k.PublicKey, k.State, t.s.d.timeArg(k.TrustedSince),
		t.s.d.timeArg(k.StateChangedAt), t.s.d.timeArg(k.CreatedAt), k.CreatedBy, k.StateChangedBy, k.Version)
	return t.s.mapErr(err, "attestation key")
}

// AttestationKeys reads a tenant's attestation keys in a transaction, oldest first.
func (t *Tx) AttestationKeys(ctx context.Context, tenantID string) ([]Key, error) {
	return collectKeys(t.query(ctx, "SELECT "+attestationKeyCols+" FROM attestation_keys WHERE tenant_id = ? ORDER BY created_at, id", tenantID))
}

// AttestationKeys reads a tenant's attestation keys, oldest first.
func (s *Store) AttestationKeys(ctx context.Context, tenantID string) ([]Key, error) {
	return collectKeys(s.db.QueryContext(ctx, s.d.rebind("SELECT "+attestationKeyCols+
		" FROM attestation_keys WHERE tenant_id = ? ORDER BY created_at, id"), tenantID))
}

// SetAttestationKeyState moves an attestation key to a new state (compare-and-set on its version);
// becoming trusted never resets trusted_since.
func (t *Tx) SetAttestationKeyState(ctx context.Context, k *Key, state, actor string) error {
	now := t.Now()
	err := t.cas(ctx, "UPDATE attestation_keys SET state = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{state, t.s.d.timeArg(now), actor, k.ID, k.Version},
		func() { k.State, k.StateChangedAt, k.StateChangedBy = state, now, actor; k.Version++ })
	return t.s.mapErr(err, "attestation key state")
}

// IssuedStatement is a statement a tenant issued (signed and numbered): a document offered, or its
// withdrawal. It outlives the document's record, so a withdrawal is served after a removal.
type IssuedStatement struct {
	TenantID  string
	Seq       int64
	IssuerID  string
	BodyHash  string
	Withdrawn bool
	Statement []byte // the exact signed bytes
	Signature []byte
	IssuedAt  time.Time
}

// NextStatementSeq takes the tenant's next statement number: the counter row stays locked until the
// transaction ends, so a statement numbered later never commits before one numbered earlier.
func (t *Tx) NextStatementSeq(ctx context.Context, tenantID string) (int64, error) {
	res, err := t.exec(ctx, "UPDATE statement_counters SET seq = seq + 1 WHERE tenant_id = ?", tenantID)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		if _, err := t.exec(ctx, "INSERT INTO statement_counters (tenant_id, seq) VALUES (?, ?)", tenantID, 1); err != nil {
			return 0, t.s.mapErr(err, "statement counter")
		}
		return 1, nil
	}
	var seq int64
	err = t.queryRow(ctx, "SELECT seq FROM statement_counters WHERE tenant_id = ?", tenantID).Scan(&seq)
	return seq, err
}

// InsertStatement records an issued statement.
func (t *Tx) InsertStatement(ctx context.Context, st IssuedStatement) error {
	_, err := t.exec(ctx, `INSERT INTO statements (tenant_id, seq, issuer_id, body_hash, withdrawn, statement, signature, issued_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, st.TenantID, st.Seq, st.IssuerID, st.BodyHash, st.Withdrawn, string(st.Statement),
		encodeSig(st.Signature), t.s.d.timeArg(st.IssuedAt))
	return t.s.mapErr(err, "statement")
}

const statementCols = "tenant_id, seq, issuer_id, body_hash, withdrawn, statement, signature, issued_at"

func scanStatement(scan func(...any) error) (IssuedStatement, error) {
	var st IssuedStatement
	var stmt, sig string
	err := scan(&st.TenantID, &st.Seq, &st.IssuerID, &st.BodyHash, &st.Withdrawn, &stmt, &sig, scanTime{&st.IssuedAt})
	if err != nil {
		return st, err
	}
	st.Statement = []byte(stmt)
	st.Signature, err = decodeSig(sig)
	return st, err
}

// LatestStatement reads the last statement issued about a document (ErrNotFound: none).
func (t *Tx) LatestStatement(ctx context.Context, tenantID, issuerID string) (IssuedStatement, error) {
	q := "SELECT " + statementCols + " FROM statements WHERE tenant_id = ? AND issuer_id = ? ORDER BY seq DESC" + t.s.limit(1)
	st, err := scanStatement(t.queryRow(ctx, q, tenantID, issuerID).Scan)
	return st, notFound(err, "statement")
}

// LatestStatement reads the last statement issued about a document (ErrNotFound: none).
func (s *Store) LatestStatement(ctx context.Context, tenantID, issuerID string) (IssuedStatement, error) {
	var out IssuedStatement
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		out, err = tx.LatestStatement(ctx, tenantID, issuerID)
		return err
	})
	return out, err
}

// BodyStatements lists up to limit statements a tenant issued about a body after a seq, in order.
func (s *Store) BodyStatements(ctx context.Context, tenantID, bodyHash string, after int64, limit int) ([]IssuedStatement, error) {
	var out []IssuedStatement
	err := s.eachRow(ctx, "SELECT "+statementCols+" FROM statements WHERE tenant_id = ? AND body_hash = ? AND seq > ? ORDER BY seq"+s.limit(limit),
		[]any{tenantID, bodyHash, after}, func(sc func(...any) error) error {
			st, err := scanStatement(sc)
			out = append(out, st)
			return err
		})
	return out, err
}

// DeleteStatements deletes the statements about a document (the collector, with its record).
func (t *Tx) DeleteStatements(ctx context.Context, tenantID, issuerID string) error {
	_, err := t.exec(ctx, "DELETE FROM statements WHERE tenant_id = ? AND issuer_id = ?", tenantID, issuerID)
	return err
}

// ReleasedIn answers which of the bodies a tenant has released (not yanked or purged) in one of
// the channels.
func (s *Store) ReleasedIn(ctx context.Context, tenantID string, channelIDs, bodies []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(channelIDs) == 0 || len(bodies) == 0 {
		return out, nil
	}
	for start := 0; start < len(bodies); start += 500 {
		page := bodies[start:min(start+500, len(bodies))]
		q := `SELECT DISTINCT b.body_hash FROM releases r JOIN builds b ON b.id = r.build_id WHERE r.tenant_id = ? AND r.state IN (?, ?)
AND r.channel_id IN (` + marks(len(channelIDs)) + `) AND b.body_hash IN (` + marks(len(page)) + `)`
		args := []any{tenantID, ReleaseActive, ReleaseDeprecated}
		for _, c := range channelIDs {
			args = append(args, c)
		}
		for _, b := range page {
			args = append(args, b)
		}
		err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
			var h string
			if err := sc(&h); err != nil {
				return err
			}
			out[h] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// Feed states, offers and statuses (spec 0011).
const (
	FeedActive = "active"
	FeedPaused = "paused"

	OfferUpstreams = "upstreams" // the bodies whose Builds came from upstreams (the default)
	OfferChannels  = "channels"  // the bodies released in named channels

	FeedNew          = "new"
	FeedOK           = "ok"
	FeedUnauthorized = "unauthorized"
	FeedFailing      = "failing"
)

// Limits on feeds.
const (
	MaxFeeds    = 50
	MaxFeedKeys = 10
)

// Feed is a source a tenant subscribes to for documents about the bodies it holds.
type Feed struct {
	ID, TenantID, Name, URL, Credential string
	Kinds                               []string // accepted kinds; none: every kind
	Readers                             bool     // statements whose audience is readers may be shown to readers
	Offer, State                        string
	Version                             int64
	RequestedAt, NextRunAt, LastRunAt   time.Time
	LastRun                             string // JSON
	Status, LastError                   string
	LastSuccessAt, CreatedAt            time.Time
	CreatedBy                           string

	Keys     []string // pinned attestation key fingerprints
	Channels []string // channel ids, for OfferChannels
}

// FeedLock orders a tenant's feed changes.
func FeedLock(tenantID string) string { return "kista/feeds/" + tenantID }

const feedCols = `id, tenant_id, name, url, credential, kinds, readers, offer, state, version, requested_at, next_run_at,
last_run_at, last_run, status, last_success_at, last_error, created_at, created_by`

func scanFeed(scan func(...any) error) (Feed, error) {
	var f Feed
	var cred, lastRun, lastErr sql.NullString
	var kinds string
	err := scan(&f.ID, &f.TenantID, &f.Name, &f.URL, &cred, &kinds, &f.Readers, &f.Offer, &f.State, &f.Version,
		scanTime{&f.RequestedAt}, scanTime{&f.NextRunAt}, scanTime{&f.LastRunAt}, &lastRun, &f.Status, scanTime{&f.LastSuccessAt},
		&lastErr, scanTime{&f.CreatedAt}, &f.CreatedBy)
	f.Credential, f.LastRun, f.LastError = cred.String, lastRun.String, lastErr.String
	if kinds != "" {
		f.Kinds = strings.Split(kinds, ",")
	}
	return f, err
}

// InsertFeed adds a feed with its keys and channels; the caller holds the tenant's feed lock and
// has validated the fields.
func (t *Tx) InsertFeed(ctx context.Context, f *Feed) error {
	if err := ValidName(f.Name); err != nil {
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feeds WHERE tenant_id = ?", f.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxFeeds {
		return fmt.Errorf("%w: a tenant subscribes to at most %d feeds", ErrInvalid, MaxFeeds)
	}
	if len(f.Keys) == 0 || len(f.Keys) > MaxFeedKeys {
		return fmt.Errorf("%w: a feed pins 1 to %d keys", ErrInvalid, MaxFeedKeys)
	}
	f.ID, f.Version, f.CreatedAt, f.Status = NewID(), 1, t.Now(), FeedNew
	if f.State == "" {
		f.State = FeedActive
	}
	_, err := t.exec(ctx, `INSERT INTO feeds (id, tenant_id, name, url, credential, kinds, readers, offer, state, version, status,
created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.ID, f.TenantID, f.Name, f.URL, nullable(f.Credential),
		strings.Join(f.Kinds, ","), f.Readers, f.Offer, f.State, f.Version, f.Status, t.s.d.timeArg(f.CreatedAt), f.CreatedBy)
	if err != nil {
		return t.s.mapErr(err, "feed")
	}
	for _, k := range f.Keys {
		if _, err := t.exec(ctx, "INSERT INTO feed_keys (feed_id, fingerprint) VALUES (?, ?)", f.ID, k); err != nil {
			return t.s.mapErr(err, "feed key")
		}
	}
	for _, c := range f.Channels {
		if _, err := t.exec(ctx, "INSERT INTO feed_channels (feed_id, channel_id) VALUES (?, ?)", f.ID, c); err != nil {
			return t.s.mapErr(err, "feed channel")
		}
	}
	return nil
}

func (t *Tx) feedChildren(ctx context.Context, f *Feed) error {
	var err error
	if f.Keys, err = t.strings(ctx, "SELECT fingerprint FROM feed_keys WHERE feed_id = ? ORDER BY fingerprint", f.ID); err != nil {
		return err
	}
	f.Channels, err = t.strings(ctx, "SELECT channel_id FROM feed_channels WHERE feed_id = ? ORDER BY channel_id", f.ID)
	return err
}

func (t *Tx) oneFeed(ctx context.Context, q string, args ...any) (Feed, error) {
	f, err := scanFeed(t.queryRow(ctx, "SELECT "+feedCols+" FROM feeds WHERE "+q, args...).Scan)
	if err != nil {
		return f, notFound(err, "feed")
	}
	return f, t.feedChildren(ctx, &f)
}

// GetFeed reads a tenant's feed by name, with its keys and channels.
func (t *Tx) GetFeed(ctx context.Context, tenantID, name string) (Feed, error) {
	return t.oneFeed(ctx, "tenant_id = ? AND name = ?", tenantID, name)
}

// GetFeedByID reads a feed by id.
func (t *Tx) GetFeedByID(ctx context.Context, id string) (Feed, error) {
	return t.oneFeed(ctx, "id = ?", id)
}

// ListFeeds lists a tenant's feeds by name, with their keys and channels.
func (t *Tx) ListFeeds(ctx context.Context, tenantID string) ([]Feed, error) {
	rows, err := t.query(ctx, "SELECT "+feedCols+" FROM feeds WHERE tenant_id = ? ORDER BY name", tenantID)
	if err != nil {
		return nil, err
	}
	var out []Feed
	for rows.Next() {
		f, err := scanFeed(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := t.feedChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetFeed changes a feed's settings (compare-and-set on its version; expected 0 takes the record's):
// its state, credential, accepted kinds, readers and pinned keys.
func (t *Tx) SetFeed(ctx context.Context, f *Feed, expected int64) error {
	if expected == 0 {
		expected = f.Version
	}
	if len(f.Keys) == 0 || len(f.Keys) > MaxFeedKeys {
		return fmt.Errorf("%w: a feed pins 1 to %d keys", ErrInvalid, MaxFeedKeys)
	}
	err := t.cas(ctx, "UPDATE feeds SET state = ?, credential = ?, kinds = ?, readers = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{f.State, nullable(f.Credential), strings.Join(f.Kinds, ","), f.Readers, f.ID, expected}, func() { f.Version = expected + 1 })
	if err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM feed_keys WHERE feed_id = ?", f.ID); err != nil {
		return err
	}
	for _, k := range f.Keys {
		if _, err := t.exec(ctx, "INSERT INTO feed_keys (feed_id, fingerprint) VALUES (?, ?)", f.ID, k); err != nil {
			return t.s.mapErr(err, "feed key")
		}
	}
	return nil
}

// DeleteFeed deletes a feed (compare-and-set on its version) with its keys, channels and cursors;
// its documents stay unless the caller removes them.
func (t *Tx) DeleteFeed(ctx context.Context, f Feed, expected int64) error {
	if expected == 0 {
		expected = f.Version
	}
	return t.cas(ctx, "DELETE FROM feeds WHERE id = ? AND version = ?", []any{f.ID, expected}, func() {})
}

// RemoveFeedDocuments removes the records of a feed's documents (their objects are collected as
// objects without a record), and answers how many.
func (t *Tx) RemoveFeedDocuments(ctx context.Context, f Feed) (int64, error) {
	res, err := t.exec(ctx, "DELETE FROM attachments WHERE tenant_id = ? AND origin = ? AND attached_by = ?", f.TenantID, AttachedByFeed, f.ID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequestFeedRun asks for a run of a feed.
func (t *Tx) RequestFeedRun(ctx context.Context, f *Feed) error {
	if f.RequestedAt.IsZero() {
		f.RequestedAt = t.Now()
	}
	_, err := t.exec(ctx, "UPDATE feeds SET requested_at = ? WHERE id = ?", t.s.d.timeArg(f.RequestedAt), f.ID)
	return err
}

// DueFeeds lists the ids of active feeds with a run requested or, when scheduled, due by their
// schedule (a feed never run is due); requested ones first.
func (s *Store) DueFeeds(ctx context.Context, now time.Time, scheduled bool) ([]string, error) {
	var out []string
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		if !scheduled {
			out, err = tx.strings(ctx, "SELECT id FROM feeds WHERE state = ? AND requested_at IS NOT NULL ORDER BY id", FeedActive)
			return err
		}
		out, err = tx.strings(ctx, `SELECT id FROM feeds WHERE state = ? AND (requested_at IS NOT NULL OR next_run_at IS NULL OR next_run_at <= ?)
ORDER BY CASE WHEN requested_at IS NULL THEN 1 ELSE 0 END, CASE WHEN next_run_at IS NULL THEN 0 ELSE 1 END, next_run_at, id`,
			FeedActive, s.d.timeArg(now))
		return err
	})
	return out, err
}

// StartFeedRun takes a feed's pending request if it is active and requested or due (runnable
// false otherwise), and clears the request, so one arriving during the run is served by the next.
func (t *Tx) StartFeedRun(ctx context.Context, id string, scheduled bool) (Feed, bool, error) {
	f, err := t.GetFeedByID(ctx, id)
	if err != nil {
		return f, false, err
	}
	due := scheduled && !f.NextRunAt.After(t.Now())
	if f.State != FeedActive || f.RequestedAt.IsZero() && !due {
		return f, false, nil
	}
	_, err = t.exec(ctx, "UPDATE feeds SET requested_at = NULL WHERE id = ?", id)
	return f, true, err
}

// FinishFeedRun records a run's result, the feed's status and the next scheduled run (zero: none),
// unless the feed was made due during the run (after started).
func (t *Tx) FinishFeedRun(ctx context.Context, id, result, status, lastError string, started, next time.Time) error {
	now := t.s.d.timeArg(t.Now())
	q := "UPDATE feeds SET last_run = ?, last_run_at = ?, status = ?, last_error = ? WHERE id = ?"
	args := []any{result, now, status, nullable(lastError), id}
	if status == FeedOK {
		q = "UPDATE feeds SET last_run = ?, last_run_at = ?, status = ?, last_error = ?, last_success_at = ? WHERE id = ?"
		args = []any{result, now, status, nullable(lastError), now, id}
	}
	if _, err := t.exec(ctx, q, args...); err != nil {
		return err
	}
	_, err := t.exec(ctx, "UPDATE feeds SET next_run_at = ? WHERE id = ? AND (next_run_at IS NULL OR next_run_at <= ?)",
		t.s.d.nullTimeArg(next), id, t.s.d.timeArg(started))
	return err
}

// OfferedBodies lists the bodies a feed offers its source, in order: by default those released from
// upstreams and never by the tenant itself (a publication or an administrator's release), or those
// released in its channels.
func (s *Store) OfferedBodies(ctx context.Context, f Feed) ([]string, error) {
	var q string
	args := []any{f.TenantID}
	if f.Offer == OfferChannels {
		q = `SELECT DISTINCT b.body_hash FROM releases r JOIN builds b ON b.id = r.build_id JOIN feed_channels c ON c.channel_id = r.channel_id
WHERE r.tenant_id = ? AND c.feed_id = ? AND r.state IN (?, ?) ORDER BY b.body_hash`
		args = append(args, f.ID, ReleaseActive, ReleaseDeprecated)
	} else {
		q = `SELECT DISTINCT b.body_hash FROM releases r JOIN builds b ON b.id = r.build_id WHERE r.tenant_id = ? AND r.origin = ?
AND NOT EXISTS (SELECT 1 FROM releases p JOIN builds pb ON pb.id = p.build_id WHERE pb.tenant_id = b.tenant_id AND pb.body_hash = b.body_hash
AND p.origin IN (?, ?)) ORDER BY b.body_hash`
		args = append(args, OriginUpstream, OriginPublication, OriginAdmin)
	}
	var out []string
	err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
		var h string
		if err := sc(&h); err != nil {
			return err
		}
		out = append(out, h)
		return nil
	})
	return out, err
}

// FeedCursors reads a feed's last seq received per body.
func (s *Store) FeedCursors(ctx context.Context, feedID string) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.eachRow(ctx, "SELECT body_hash, seq FROM feed_cursors WHERE feed_id = ?", []any{feedID}, func(sc func(...any) error) error {
		var h string
		var seq int64
		if err := sc(&h, &seq); err != nil {
			return err
		}
		out[h] = seq
		return nil
	})
	return out, err
}

// SetFeedCursor moves a feed's cursor for a body forward to seq.
func (t *Tx) SetFeedCursor(ctx context.Context, feedID, bodyHash string, seq int64) error {
	res, err := t.exec(ctx, "UPDATE feed_cursors SET seq = ? WHERE feed_id = ? AND body_hash = ? AND seq < ?", seq, feedID, bodyHash, seq)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var have int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM feed_cursors WHERE feed_id = ? AND body_hash = ?", feedID, bodyHash).Scan(&have); err != nil || have > 0 {
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO feed_cursors (feed_id, body_hash, seq) VALUES (?, ?, ?)", feedID, bodyHash, seq)
	return t.s.mapErr(err, "feed cursor")
}

// DeleteGoneFeedCursors deletes the cursors of bodies a domain's tenants no longer hold and have not
// blocked (spec 0011: a body's feed state goes with its documents, so a body held again receives its
// history again), and answers how many.
func (s *Store) DeleteGoneFeedCursors(ctx context.Context, domain string) (int64, error) {
	var n int64
	err := s.tx(ctx, nil, func(t *Tx) error {
		res, err := t.exec(ctx, `DELETE FROM feed_cursors WHERE feed_id IN (SELECT f.id FROM feeds f JOIN tenants t ON t.id = f.tenant_id
WHERE t.storage_domain = ?) AND NOT EXISTS (SELECT 1 FROM feeds f JOIN builds b ON b.tenant_id = f.tenant_id WHERE f.id = feed_cursors.feed_id
AND b.body_hash = feed_cursors.body_hash) AND NOT EXISTS (SELECT 1 FROM feeds f JOIN blocks k ON k.tenant_id = f.tenant_id
WHERE f.id = feed_cursors.feed_id AND k.body_hash = feed_cursors.body_hash)`, domain)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// TouchBody reports whether the tenant holds a Build of the body (under any name) and refreshes
// their used_at in the transaction (spec 0016's intake rule).
func (t *Tx) TouchBody(ctx context.Context, tenantID, bodyHash string) (bool, error) {
	res, err := t.exec(ctx, "UPDATE builds SET used_at = ? WHERE tenant_id = ? AND body_hash = ?", t.s.d.timeArg(t.Now()), tenantID, bodyHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FeedAttachment reads a document of a source by its issuer id (a tombstone too).
func (t *Tx) FeedAttachment(ctx context.Context, tenantID, issuer, issuerID string) (Attachment, error) {
	return t.attachmentByIssuer(ctx, tenantID, issuer, issuerID)
}

// ReissueFeedAttachment records a newer statement about a document already received: its seq,
// statement, signature, key, validity and visibility; a withdrawn one is offered again.
func (t *Tx) ReissueFeedAttachment(ctx context.Context, a *Attachment) error {
	return t.cas(ctx, `UPDATE attachments SET seq = ?, statement = ?, signature = ?, key_fp = ?, issued_at = ?, valid_until = ?,
visibility = ?, withdrawn_at = NULL, version = version + 1 WHERE id = ? AND removed_at IS NULL AND (seq IS NULL OR seq < ?)`,
		[]any{a.Seq, nullable(a.Statement), nullable(a.Signature), nullable(a.KeyFP), t.s.d.timeArg(a.IssuedAt), t.s.d.nullTimeArg(a.ValidUntil),
			a.Visibility, a.ID, a.Seq}, func() { a.WithdrawnAt = time.Time{}; a.Version++ })
}

// WithdrawFeedAttachment marks a received document withdrawn by a statement numbered seq (a newer
// one than what it holds), with the withdrawal's own statement and signature.
func (t *Tx) WithdrawFeedAttachment(ctx context.Context, a *Attachment, seq int64, statement, signature string) error {
	now := t.Now()
	return t.cas(ctx, `UPDATE attachments SET seq = ?, statement = ?, signature = ?, withdrawn_at = ?, version = version + 1
WHERE id = ? AND (seq IS NULL OR seq < ?)`, []any{seq, statement, signature, t.s.d.timeArg(now), a.ID, seq},
		func() { a.Seq, a.Statement, a.Signature, a.WithdrawnAt = seq, statement, signature, now; a.Version++ })
}

// StatementOffered reports whether an offer is still what the tenant says about its document: the
// document exists, is feedable and not removed, and no later statement was issued about it.
func (s *Store) StatementOffered(ctx context.Context, tenantID, issuerID string, seq int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*) FROM attachments a WHERE a.tenant_id = ? AND a.issuer = '' AND a.issuer_id = ?
AND a.feedable = ? AND a.removed_at IS NULL AND NOT EXISTS (SELECT 1 FROM statements s WHERE s.tenant_id = a.tenant_id
AND s.issuer_id = a.issuer_id AND s.seq > ?)`), tenantID, issuerID, true, seq).Scan(&n)
	return n > 0, err
}

// GetTenantByID reads a tenant by id.
func (t *Tx) GetTenantByID(ctx context.Context, id string) (Tenant, error) {
	tn, err := scanTenant(t.queryRow(ctx, "SELECT "+tenantCols+" FROM tenants WHERE id = ?", id))
	return tn, notFound(err, "tenant")
}
