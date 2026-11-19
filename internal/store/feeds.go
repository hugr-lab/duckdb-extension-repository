package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
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
AND b.withdrawn_at IS NULL AND r.channel_id IN (` + marks(len(channelIDs)) + `) AND b.body_hash IN (` + marks(len(page)) + `)`
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

	OfferUpstreams = "upstreams" // the bodies of the catalogue builds that came from outside (the default; spec 0045)
	OfferChannels  = "channels"  // the bodies released in named channels

	FeedOnline = "online" // the feed's mode: run against its source (the default)
	FeedBundle = "bundle" // the feed's mode: statements arrive in bundles (spec 0014)

	FeedNew          = "new"
	FeedOK           = "ok"
	FeedUnauthorized = "unauthorized"
	FeedFailing      = "failing"
)

// Limits on feeds: ceilings the configuration sets (SetCeilings).
var (
	MaxFeeds    = 50
	MaxFeedKeys = 10
)

// MaxFeedKeysFile caps the source's attestation keys file an export feed keeps (spec 0014).
const MaxFeedKeysFile = 64 << 10

// Feed is a source a tenant subscribes to for documents about the bodies it holds.
type Feed struct {
	ID, TenantID, Name, URL, Credential string
	Kinds                               []string // accepted kinds; none: every kind
	Readers                             bool     // statements whose audience is readers may be shown to readers
	Offer, State                        string
	Mode                                string // FeedOnline or FeedBundle; set at creation
	// Via is the relaying tenant's URL (spec 0045), set at creation and never changed: a run asks
	// it for the statements of the issuer URL names, which is then never contacted. "": none
	Via string
	// RelayStoppedAt is when a relayed answer of the feed last ended early (spec 0045): at a
	// statement that does not verify under its pins, or for a keys file that leaves no room for a
	// statement in an answer. Zero: none since its pins or its export last changed
	RelayStoppedAt time.Time
	// Export keeps what a bundle needs to answer a site's plan (spec 0014): the keys file, permanent
	// refusals, statements about documents not held
	Export                            bool
	ExportEpoch                       int64 // counts the changes of Export: a run writes only in its own
	Version                           int64
	RequestedAt, NextRunAt, LastRunAt time.Time
	LastRun                           string // JSON
	Status, LastError                 string
	LastSuccessAt, CreatedAt          time.Time
	CreatedBy                         string
	// the last bundle imported into a bundle-mode feed (spec 0014)
	BundleID, BundleManifestSHA256 string
	BundleCreatedAt                time.Time
	KeysFileAt                     time.Time // when an export feed last kept its source's keys file

	Keys     []string // pinned attestation key fingerprints
	Channels []string // channel ids, for OfferChannels
	// Waiting counts, by each body's last reason, what the feed passed over because this binary
	// could not read it (spec 0046); WaitingSource what arrived without content because the
	// exporting kista could not. Filled where a feed is shown, not by every read
	Waiting, WaitingSource map[string]int
}

// FeedLock orders a tenant's feed changes.
func FeedLock(tenantID string) string { return "kista/feeds/" + tenantID }

const feedCols = `id, tenant_id, name, url, credential, kinds, readers, offer, state, version, requested_at, next_run_at,
last_run_at, last_run, status, last_success_at, last_error, created_at, created_by, mode, export, bundle_id, bundle_created_at,
bundle_manifest_sha256, attestation_keys_at, export_epoch, via, relay_stopped_at`

func scanFeed(scan func(...any) error) (Feed, error) {
	var f Feed
	var cred, lastRun, lastErr, bundleID, bundleSHA, via sql.NullString
	var kinds string
	err := scan(&f.ID, &f.TenantID, &f.Name, &f.URL, &cred, &kinds, &f.Readers, &f.Offer, &f.State, &f.Version,
		scanTime{&f.RequestedAt}, scanTime{&f.NextRunAt}, scanTime{&f.LastRunAt}, &lastRun, &f.Status, scanTime{&f.LastSuccessAt},
		&lastErr, scanTime{&f.CreatedAt}, &f.CreatedBy, &f.Mode, &f.Export, &bundleID, scanTime{&f.BundleCreatedAt}, &bundleSHA,
		scanTime{&f.KeysFileAt}, &f.ExportEpoch, &via, scanTime{&f.RelayStoppedAt})
	f.Credential, f.LastRun, f.LastError, f.Via = cred.String, lastRun.String, lastErr.String, via.String
	f.BundleID, f.BundleManifestSHA256 = bundleID.String, bundleSHA.String
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
	if f.Mode == "" {
		f.Mode = FeedOnline
	}
	if f.Mode != FeedOnline && f.Mode != FeedBundle || f.Mode == FeedBundle && (f.Credential != "" || f.Export || f.Via != "") {
		return fmt.Errorf("%w: a feed's mode is online, or bundle without a credential, export or via", ErrInvalid)
	}
	if err := t.oneExport(ctx, f); err != nil {
		return err
	}
	_, err := t.exec(ctx, `INSERT INTO feeds (id, tenant_id, name, url, credential, kinds, readers, offer, state, version, status,
created_at, created_by, mode, export, via) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.ID, f.TenantID, f.Name, f.URL, nullable(f.Credential),
		strings.Join(f.Kinds, ","), f.Readers, f.Offer, f.State, f.Version, f.Status, t.s.d.timeArg(f.CreatedAt), f.CreatedBy, f.Mode, f.Export,
		nullable(f.Via))
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

// ErrHandedOn is export asked for a feed whose issuer another feed of the tenant hands on already
// (spec 0045); it wraps into ErrInvalid.
var ErrHandedOn = errors.New("store: the issuer is handed on already")

// oneExport holds the rule that a tenant hands on at most one feed per issuer (spec 0045): export
// is refused for a feed while another feed of the tenant with the same URL has it, naming that
// feed. The caller holds the tenant's feed lock, which every creation and change of a feed takes,
// so two cannot get around each other.
func (t *Tx) oneExport(ctx context.Context, f *Feed) error {
	if !f.Export {
		return nil
	}
	names, err := t.strings(ctx, "SELECT name FROM feeds WHERE tenant_id = ? AND url = ? AND export = ? AND id <> ? ORDER BY name", f.TenantID, f.URL, true, f.ID)
	if err != nil || len(names) == 0 {
		return err
	}
	return fmt.Errorf("%w: %w: the feed %q hands %s on already, and a tenant hands on at most one feed per issuer: clear its export first, or remove it",
		ErrInvalid, ErrHandedOn, names[0], f.URL)
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
// its state, credential, accepted kinds, readers, export and pinned keys. Its mode and its via never
// change.
func (t *Tx) SetFeed(ctx context.Context, f *Feed, expected int64) error {
	if expected == 0 {
		expected = f.Version
	}
	if len(f.Keys) == 0 || len(f.Keys) > MaxFeedKeys {
		return fmt.Errorf("%w: a feed pins 1 to %d keys", ErrInvalid, MaxFeedKeys)
	}
	if f.Mode == FeedBundle && (f.Credential != "" || f.Export) {
		return fmt.Errorf("%w: a bundle-mode feed takes no credential and no export", ErrInvalid)
	}
	if err := t.oneExport(ctx, f); err != nil {
		return err
	}
	err := t.cas(ctx, `UPDATE feeds SET state = ?, credential = ?, kinds = ?, readers = ?, export = ?,
export_epoch = CASE WHEN export = ? THEN export_epoch ELSE export_epoch + 1 END, version = version + 1 WHERE id = ? AND version = ? AND mode = ?`,
		[]any{f.State, nullable(f.Credential), strings.Join(f.Kinds, ","), f.Readers, f.Export, f.Export, f.ID, expected, f.Mode}, func() { f.Version = expected + 1 })
	if err != nil {
		return err
	}
	if err := t.queryRow(ctx, "SELECT export_epoch FROM feeds WHERE id = ?", f.ID).Scan(&f.ExportEpoch); err != nil {
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
			out, err = tx.strings(ctx, "SELECT id FROM feeds WHERE state = ? AND mode = ? AND requested_at IS NOT NULL AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL) ORDER BY id", FeedActive, FeedOnline)
			return err
		}
		out, err = tx.strings(ctx, `SELECT id FROM feeds WHERE state = ? AND mode = ? AND tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL) AND (requested_at IS NOT NULL OR next_run_at IS NULL OR next_run_at <= ?)
ORDER BY CASE WHEN requested_at IS NULL THEN 1 ELSE 0 END, CASE WHEN next_run_at IS NULL THEN 0 ELSE 1 END, next_run_at, id`,
			FeedActive, FeedOnline, s.d.timeArg(now))
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
	if f.State != FeedActive || f.Mode != FeedOnline || f.RequestedAt.IsZero() && !due {
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

// fromDistributor is the condition of a build x of a distributor's publisher, live or removed (spec
// 0033 phase B: a removed one's record is kept).
func (s *Store) fromDistributor(x string) string {
	id := "substr(" + x + ".source, 11)"
	if s.d.Name == "sqlserver" {
		id = "SUBSTRING(" + x + ".source, 11, 64)"
	}
	return `(` + x + `.source LIKE 'publisher:%' AND (
	EXISTS (SELECT 1 FROM publishers p WHERE p.id = ` + id + ` AND p.tenant_id = ` + x + `.tenant_id AND p.distributor IS NOT NULL)
	OR EXISTS (SELECT 1 FROM removed_distributor_publishers rd WHERE rd.tenant_id = ` + x + `.tenant_id AND rd.publisher_id = ` + id + `)))`
}

// OfferedBodies lists the bodies a feed offers its source, in order. By default (spec 0045) those
// of the tenant's catalogue builds that came from outside: of an upstream, or of a distributor's
// publisher (live or removed), not withdrawn, released or not (a build the catalogue keeps: landed,
// or released); and never a body that a build of the tenant's own has (the tenant's, or a publisher's
// that is not a distributor's, a removed one included). A revoked entry that remains is one a
// channel still serves: it is offered. With the channels offer: the bodies released in its channels.
// A bundle's plan takes its feeds' bodies from here too (spec 0014).
func (s *Store) OfferedBodies(ctx context.Context, f Feed) ([]string, error) {
	var q string
	args := []any{f.TenantID}
	if f.Offer == OfferChannels {
		q = `SELECT DISTINCT b.body_hash FROM releases r JOIN builds b ON b.id = r.build_id JOIN feed_channels c ON c.channel_id = r.channel_id
WHERE r.tenant_id = ? AND c.feed_id = ? AND r.state IN (?, ?) AND b.withdrawn_at IS NULL ORDER BY b.body_hash`
		args = append(args, f.ID, ReleaseActive, ReleaseDeprecated)
	} else {
		q = `SELECT DISTINCT b.body_hash FROM builds b WHERE b.tenant_id = ? AND b.withdrawn_at IS NULL
AND (b.landed = ? OR EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id))
AND (b.source LIKE 'upstream:%' OR ` + s.fromDistributor("b") + `)
AND NOT EXISTS (SELECT 1 FROM builds o WHERE o.tenant_id = b.tenant_id AND o.body_hash = b.body_hash
	AND (o.source = ? OR o.source LIKE 'publisher:%' AND NOT ` + s.fromDistributor("o") + `))
ORDER BY b.body_hash`
		args = append(args, true, SourceTenant)
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

// FeedCursorsOf reads a feed's last seq received for the bodies named (at most a feed request's),
// in one statement: a relay's answer reads only what it was asked about (spec 0045).
func (s *Store) FeedCursorsOf(ctx context.Context, feedID string, bodies []string) (map[string]int64, error) {
	out := map[string]int64{}
	for start := 0; start < len(bodies); start += 500 {
		page := bodies[start:min(start+500, len(bodies))]
		args := []any{feedID}
		for _, b := range page {
			args = append(args, b)
		}
		err := s.eachRow(ctx, "SELECT body_hash, seq FROM feed_cursors WHERE feed_id = ? AND body_hash IN ("+marks(len(page))+")", args,
			func(sc func(...any) error) error {
				var h string
				var seq int64
				if err := sc(&h, &seq); err != nil {
					return err
				}
				out[h] = seq
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MarkFeedRelayStopped records that a relayed answer of a feed ended early (spec 0045), at most
// once a minute whatever the requests: one conditional update. It writes only while the feed is as
// the answer read it (its version: a change of its pins or its export since clears the record, and
// must not have it written back), and reports whether it wrote, so its caller logs no more often.
func (s *Store) MarkFeedRelayStopped(ctx context.Context, f Feed) (bool, error) {
	wrote := false
	err := s.InTx(ctx, "", func(t *Tx) error {
		now := t.Now()
		res, err := t.exec(ctx, "UPDATE feeds SET relay_stopped_at = ? WHERE id = ? AND version = ? AND (relay_stopped_at IS NULL OR relay_stopped_at < ?)",
			t.s.d.timeArg(now), f.ID, f.Version, t.s.d.timeArg(now.Add(-time.Minute)))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		wrote = n > 0
		return err
	})
	return wrote, err
}

// ClearFeedRelayStopped forgets what MarkFeedRelayStopped recorded: the feed's pins or its export
// changed, so the next relayed answer says anew.
func (t *Tx) ClearFeedRelayStopped(ctx context.Context, feedID string) error {
	_, err := t.exec(ctx, "UPDATE feeds SET relay_stopped_at = NULL WHERE id = ?", feedID)
	return err
}

// FeedCursors reads a feed's last seq received per body, in a transaction.
func (t *Tx) FeedCursors(ctx context.Context, feedID string) (map[string]int64, error) {
	rows, err := t.query(ctx, "SELECT body_hash, seq FROM feed_cursors WHERE feed_id = ?", feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var h string
		var seq int64
		if err := rows.Scan(&h, &seq); err != nil {
			return nil, err
		}
		out[h] = seq
	}
	return out, rows.Err()
}

// SetFeedCursor moves a feed's cursor for a body forward to seq. A waiting state left for display
// by a rewind (RetakeFeedCursors: its lowest seq is above the cursor) is cleared once the cursor
// reaches that seq again: the range was judged again, and what still waits is recorded anew.
func (t *Tx) SetFeedCursor(ctx context.Context, feedID, bodyHash string, seq int64) error {
	const again = "waiting_from > seq AND waiting_from <= ?"
	res, err := t.exec(ctx, `UPDATE feed_cursors SET
waiting = CASE WHEN `+again+` THEN 0 ELSE waiting END,
waiting_why = CASE WHEN `+again+` THEN NULL ELSE waiting_why END,
reader_level = CASE WHEN `+again+` THEN 0 ELSE reader_level END,
waiting_from = CASE WHEN `+again+` THEN NULL ELSE waiting_from END,
seq = ? WHERE feed_id = ? AND body_hash = ? AND seq < ?`, seq, seq, seq, seq, seq, feedID, bodyHash, seq)
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

// FeedWait is what a feed passed over for a body because its binary could not read it (spec 0046):
// the lowest such seq, how many, the last reason, and the reader level of the binary that passed
// over them (0: asked for again at every retake, whatever the binary).
type FeedWait struct {
	BodyHash string
	From     int64
	Count    int
	Why      string
	Level    int
	at       int64 // the cursor, where a retake reads it
}

// AddFeedWait records what a feed passed over for a body, on its cursor's row (the cursor is past
// it already): the lowest seq is kept, the count raised, the reason the last one. A row has one
// level: one that has waiting state already keeps the lower.
func (t *Tx) AddFeedWait(ctx context.Context, feedID string, w FeedWait) error {
	_, err := t.exec(ctx, `UPDATE feed_cursors SET
reader_level = CASE WHEN waiting_from IS NOT NULL AND reader_level < ? THEN reader_level ELSE ? END,
waiting_from = CASE WHEN waiting_from IS NOT NULL AND waiting_from < ? THEN waiting_from ELSE ? END,
waiting = waiting + ?, waiting_why = ? WHERE feed_id = ? AND body_hash = ?`,
		w.Level, w.Level, w.From, w.From, w.Count, w.Why, feedID, w.BodyHash)
	return err
}

// FeedWaits reads a feed's waiting state per body.
func (s *Store) FeedWaits(ctx context.Context, feedID string) (map[string]FeedWait, error) {
	var out map[string]FeedWait
	err := s.InTx(ctx, "", func(tx *Tx) (err error) { out, err = tx.FeedWaits(ctx, feedID); return })
	return out, err
}

// FeedWaits reads a feed's waiting state per body, in a transaction.
func (t *Tx) FeedWaits(ctx context.Context, feedID string) (map[string]FeedWait, error) {
	rows, err := t.query(ctx, `SELECT body_hash, waiting_from, waiting, waiting_why, reader_level FROM feed_cursors
WHERE feed_id = ? AND waiting_from IS NOT NULL`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FeedWait{}
	for rows.Next() {
		var w FeedWait
		var why sql.NullString
		if err := rows.Scan(&w.BodyHash, &w.From, &w.Count, &why, &w.Level); err != nil {
			return nil, err
		}
		w.Why = why.String
		out[w.BodyHash] = w
	}
	return out, rows.Err()
}

// FeedWaiting counts what a feed passed over, by each body's last reason (for display: the count
// may overstate, spec 0046): Own, what this kista is too old to read; Source, what arrived without
// content because the exporting kista could not read it (level 0: a body's row has one level, the
// lower).
type FeedWaiting struct {
	Own, Source map[string]int
}

// FeedsWaiting counts what a tenant's feeds passed over, per feed.
func (t *Tx) FeedsWaiting(ctx context.Context, tenantID string) (map[string]FeedWaiting, error) {
	rows, err := t.query(ctx, `SELECT c.feed_id, c.waiting_why, c.reader_level, c.waiting FROM feed_cursors c JOIN feeds f ON f.id = c.feed_id
WHERE f.tenant_id = ? AND c.waiting_from IS NOT NULL`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FeedWaiting{}
	for rows.Next() {
		var id string
		var why sql.NullString
		var level, n int
		if err := rows.Scan(&id, &why, &level, &n); err != nil {
			return nil, err
		}
		w := out[id]
		into := &w.Own
		if level == 0 {
			into = &w.Source
		}
		if *into == nil {
			*into = map[string]int{}
		}
		(*into)[why.String] += n
		out[id] = w
	}
	return out, rows.Err()
}

// RetakeFeedCursors takes again what a feed passed over at a reader level below level (spec 0046):
// each such cursor goes back to just below the lowest seq it passed over, and the refusals the feed
// kept for export of that body from that seq on are deleted (the replay keeps again what is still
// refused or waits). The waiting state of a level this binary outgrew is cleared. That of level 0
// (statements that arrived without content) stays for display until the range is judged again
// (SetFeedCursor clears it then): the exporting kista is still the older one. It answers the bodies
// rewound. This is the one operation that moves a cursor back; the caller holds the tenant's feed
// lock.
func (t *Tx) RetakeFeedCursors(ctx context.Context, feedID string, level int) (int, error) {
	rows, err := t.query(ctx, `SELECT body_hash, waiting_from, reader_level, seq FROM feed_cursors
WHERE feed_id = ? AND waiting_from IS NOT NULL AND reader_level < ?`, feedID, level)
	if err != nil {
		return 0, err
	}
	var waits []FeedWait
	for rows.Next() {
		var w FeedWait
		if err := rows.Scan(&w.BodyHash, &w.From, &w.Level, &w.at); err != nil {
			_ = rows.Close()
			return 0, err
		}
		waits = append(waits, w)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, w := range waits {
		q := `UPDATE feed_cursors SET seq = ?, waiting_from = NULL, waiting = 0, waiting_why = NULL, reader_level = 0
WHERE feed_id = ? AND body_hash = ?`
		if w.Level == 0 {
			if w.at < w.From {
				continue // rewound already, not judged again yet
			}
			q = "UPDATE feed_cursors SET seq = ? WHERE feed_id = ? AND body_hash = ?"
		}
		n++
		if _, err := t.exec(ctx, q, w.From-1, feedID, w.BodyHash); err != nil {
			return 0, err
		}
		if _, err := t.exec(ctx, "DELETE FROM feed_refusals WHERE feed_id = ? AND body_hash = ? AND seq >= ?", feedID, w.BodyHash, w.From); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// NarrowFeedAttachment narrows a received document to administrators and marks it narrowed (spec
// 0046: a statement in a newer format about it whose audience is not readers). The mark holds until
// a statement this kista reads, newer than the document's, is stored (ReissueFeedAttachment): until
// then its audience is administrators, whatever its stored statement says, and no administrator
// widens it. It answers whether the document changed.
func (t *Tx) NarrowFeedAttachment(ctx context.Context, a *Attachment) (bool, error) {
	res, err := t.exec(ctx, "UPDATE attachments SET visibility = ?, narrowed = ?, version = version + 1 WHERE id = ? AND removed_at IS NULL AND narrowed = ?",
		VisibleAdministrators, true, a.ID, false)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		a.Visibility, a.Narrowed = VisibleAdministrators, true
		a.Version++
	}
	return n > 0, nil
}

// DeleteGoneFeedCursors deletes the cursors of bodies a domain's tenants no longer hold and have not
// blocked (spec 0011: a body's feed state goes with its documents, so a body held again receives its
// history again), with what export feeds kept for them, and answers how many cursors.
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
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		// what export feeds keep goes with the cursors (spec 0014)
		if _, err := t.exec(ctx, `DELETE FROM feed_refusals WHERE feed_id IN (SELECT f.id FROM feeds f JOIN tenants t ON t.id = f.tenant_id
WHERE t.storage_domain = ?) AND NOT EXISTS (SELECT 1 FROM feeds f JOIN builds b ON b.tenant_id = f.tenant_id WHERE f.id = feed_refusals.feed_id
AND b.body_hash = feed_refusals.body_hash) AND NOT EXISTS (SELECT 1 FROM feeds f JOIN blocks k ON k.tenant_id = f.tenant_id
WHERE f.id = feed_refusals.feed_id AND k.body_hash = feed_refusals.body_hash)`, domain); err != nil {
			return err
		}
		_, err = t.exec(ctx, `DELETE FROM feed_kept WHERE tenant_id IN (SELECT id FROM tenants WHERE storage_domain = ?)
AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = feed_kept.tenant_id AND b.body_hash = feed_kept.body_hash)
AND NOT EXISTS (SELECT 1 FROM blocks k WHERE k.tenant_id = feed_kept.tenant_id AND k.body_hash = feed_kept.body_hash)`, domain)
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
// statement, signature, key and validity; a withdrawn one is offered again, and a mark left by a
// statement in a newer format (NarrowFeedAttachment) is cleared: this statement says who may see
// it. Its visibility only
// narrows (to administrators, when a.Visibility says so): what an administrator here set meanwhile
// is never widened from a stale read.
func (t *Tx) ReissueFeedAttachment(ctx context.Context, a *Attachment) error {
	return t.cas(ctx, `UPDATE attachments SET seq = ?, statement = ?, signature = ?, key_fp = ?, issued_at = ?, valid_until = ?,
visibility = CASE WHEN ? = ? THEN ? ELSE visibility END, narrowed = ?, withdrawn_at = NULL, version = version + 1
WHERE id = ? AND removed_at IS NULL AND (seq IS NULL OR seq < ?)`,
		[]any{a.Seq, nullable(a.Statement), nullable(a.Signature), nullable(a.KeyFP), t.s.d.timeArg(a.IssuedAt), t.s.d.nullTimeArg(a.ValidUntil),
			a.Visibility, VisibleAdministrators, VisibleAdministrators, false, a.ID, a.Seq},
		func() { a.WithdrawnAt, a.Narrowed = time.Time{}, false; a.Version++ })
}

// ReattributeFeedDocuments hands a feed's documents to another feed of the same source (removing a
// feed with its documents while another still subscribes: that one's cursors are past them).
func (t *Tx) ReattributeFeedDocuments(ctx context.Context, from, to Feed) error {
	_, err := t.exec(ctx, "UPDATE attachments SET attached_by = ? WHERE tenant_id = ? AND origin = ? AND attached_by = ?",
		to.ID, from.TenantID, AttachedByFeed, from.ID)
	return err
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

// GetTenantByID reads a tenant by id, as Tx.GetTenant: locked, and closed once its deletion started.
func (t *Tx) GetTenantByID(ctx context.Context, id string) (Tenant, error) {
	return t.getTenant(ctx, "id = ?", id, false)
}
