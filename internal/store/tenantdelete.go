package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Deleting a tenant (spec 0019): the transition that closes it, the deletion record, and the
// reaper's steps, which remove its rows in batches before the tombstone.

// A tenant whose deletion started answers like a missing one: both are an ErrNotFound.
var (
	ErrDeleting = fmt.Errorf("%w: the tenant is being deleted", ErrNotFound)
	ErrDeleted  = fmt.Errorf("%w: the tenant was deleted", ErrNotFound)
)

// ReapBatch is the most rows a reaper batch deletes in one transaction.
const ReapBatch = 1000

// WritableTenant checks, inside a writing transaction, that the tenant's deletion has not started,
// holding its row until commit: a shared lock, or an update lock when the transaction then updates
// the tenant row itself (so two such transactions never deadlock upgrading a shared one). The
// deletion transition's update waits for the writers holding it, and they for it, so no write
// commits for a tenant after its deletion started.
func (t *Tx) WritableTenant(ctx context.Context, id string, forUpdate bool) error {
	var at time.Time
	if err := t.queryRow(ctx, t.lockedTenantQuery("SELECT deleting_at FROM tenants", "id = ?", forUpdate), id).Scan(scanTime{&at}); err != nil {
		return notFound(err, "tenant")
	}
	if !at.IsZero() {
		return ErrDeleting
	}
	return nil
}

// lockedTenantQuery reads the tenants row matching where with a lock held until commit: shared, or
// an update lock. SQLite needs none: every transaction is BEGIN IMMEDIATE, writers are serialised.
func (t *Tx) lockedTenantQuery(selectFrom, where string, forUpdate bool) string {
	return t.lockedRowQuery(selectFrom, where, forUpdate)
}

// lockedRowQuery reads the rows of one table matching where with a lock held until commit: shared,
// or an update lock (lockedTenantQuery's, for any table).
func (t *Tx) lockedRowQuery(selectFrom, where string, forUpdate bool) string {
	switch t.s.d.Name {
	case "postgres":
		if forUpdate {
			return selectFrom + " WHERE " + where + " FOR NO KEY UPDATE"
		}
		return selectFrom + " WHERE " + where + " FOR SHARE"
	case "sqlserver":
		if forUpdate {
			return selectFrom + " WITH (UPDLOCK, HOLDLOCK, ROWLOCK) WHERE " + where
		}
		return selectFrom + " WITH (HOLDLOCK, ROWLOCK) WHERE " + where
	}
	return selectFrom + " WHERE " + where
}

// InTenantTx is InTxLocks for a change to a tenant's rows: it first checks the tenant is writable
// (WritableTenant, a shared lock on its row).
func (s *Store) InTenantTx(ctx context.Context, tenantID string, lockKeys []string, fn func(*Tx) error) error {
	return s.tx(ctx, lockKeys, func(tx *Tx) error {
		if err := tx.WritableTenant(ctx, tenantID, false); err != nil {
			return err
		}
		return fn(tx)
	})
}

// InTenantTxForUpdate is InTenantTx for a transaction that updates the tenant row itself (its auth
// version, its shadow backfill, its state).
func (s *Store) InTenantTxForUpdate(ctx context.Context, tenantID string, lockKeys []string, fn func(*Tx) error) error {
	return s.tx(ctx, lockKeys, func(tx *Tx) error {
		if err := tx.WritableTenant(ctx, tenantID, true); err != nil {
			return err
		}
		return fn(tx)
	})
}

// DeletionKey is a key a deleted tenant leaves in its key source, for the operator to retire.
type DeletionKey struct {
	Kind        string `json:"kind"` // channel | attestation
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	SignerRef   string `json:"signer_ref"`
}

// Deletion is a tenant's deletion record: who started it, what it leaves to retire, and the
// reaper's progress.
type Deletion struct {
	TenantID      string
	StartedAt     time.Time
	StartedBy     string
	Keys          []DeletionKey
	ChannelIDs    []string
	RunLeases     []string // its upstreams' and feeds' run leases, removed with the tombstone
	Passes        int64
	Failures      int64  // failed passes in a row, for the backoff
	Table         string // the table the last pass reached
	DeletedRows   int64
	LastError     string // the last failed pass's error class
	NextAttemptAt time.Time
	FinishedAt    time.Time // the tombstone
	LastCheckedAt time.Time // the late sweep
}

const deletionCols = "tenant_id, started_at, started_by, keys, channel_ids, run_leases, passes, failures, current_table, deleted_rows, last_error, next_attempt_at, finished_at, last_checked_at"

func scanDeletion(r interface{ Scan(...any) error }) (Deletion, error) {
	var d Deletion
	var keys, chans, runs string
	err := r.Scan(&d.TenantID, scanTime{&d.StartedAt}, &d.StartedBy, &keys, &chans, &runs, &d.Passes, &d.Failures, &d.Table, &d.DeletedRows,
		&d.LastError, scanTime{&d.NextAttemptAt}, scanTime{&d.FinishedAt}, scanTime{&d.LastCheckedAt})
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(keys), &d.Keys); err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(runs), &d.RunLeases); err != nil {
		return d, err
	}
	return d, json.Unmarshal([]byte(chans), &d.ChannelIDs)
}

// GetDeletion returns a tenant's deletion record.
func (s *Store) GetDeletion(ctx context.Context, tenantID string) (Deletion, error) {
	d, err := scanDeletion(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+deletionCols+" FROM tenant_deletions WHERE tenant_id = ?"), tenantID))
	return d, notFound(err, "tenant deletion")
}

// StartDeletion closes a suspended tenant (spec 0019) if its version is still tn.Version: the
// update first, so it waits for every write in flight (WritableTenant), then the deletion record
// with what those writes left: the keys to retire and the channel ids.
func (t *Tx) StartDeletion(ctx context.Context, tn *Tenant, by string) (Deletion, error) {
	now := t.Now()
	if err := t.cas(ctx, "UPDATE tenants SET deleting_at = ?, version = version + 1 WHERE id = ? AND version = ? AND state = ? AND deleting_at IS NULL",
		[]any{t.s.d.timeArg(now), tn.ID, tn.Version, TenantSuspended}, func() { tn.DeletingAt = now; tn.Version++ }); err != nil {
		return Deletion{}, err
	}
	d := Deletion{TenantID: tn.ID, StartedAt: now, StartedBy: by, Keys: []DeletionKey{}, ChannelIDs: []string{}, RunLeases: []string{},
		NextAttemptAt: now}
	for _, q := range []struct{ kind, sql string }{
		{"channel", "SELECT id, fingerprint, signer_ref FROM channel_keys WHERE tenant_id = ? ORDER BY created_at, id"},
		{"attestation", "SELECT id, fingerprint, signer_ref FROM attestation_keys WHERE tenant_id = ? ORDER BY created_at, id"},
	} {
		if err := t.each(ctx, q.sql, []any{tn.ID}, func(r *sql.Rows) error {
			k := DeletionKey{Kind: q.kind}
			if err := r.Scan(&k.ID, &k.Fingerprint, &k.SignerRef); err != nil {
				return err
			}
			d.Keys = append(d.Keys, k)
			return nil
		}); err != nil {
			return Deletion{}, err
		}
	}
	if err := t.each(ctx, "SELECT id FROM channels WHERE tenant_id = ? ORDER BY id", []any{tn.ID}, func(r *sql.Rows) error {
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		d.ChannelIDs = append(d.ChannelIDs, id)
		return nil
	}); err != nil {
		return Deletion{}, err
	}
	// no upstream or feed can be added from now on: their run leases are known for good
	for _, q := range []struct{ prefix, sql string }{
		{"kista/upstream/", "SELECT id FROM upstreams WHERE tenant_id = ? ORDER BY id"},
		{"kista/feed/", "SELECT id FROM feeds WHERE tenant_id = ? ORDER BY id"},
		{"kista/licenses/upstream/", "SELECT id FROM upstreams WHERE tenant_id = ? ORDER BY id"},
	} {
		if err := t.each(ctx, q.sql, []any{tn.ID}, func(r *sql.Rows) error {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			d.RunLeases = append(d.RunLeases, q.prefix+id)
			return nil
		}); err != nil {
			return Deletion{}, err
		}
	}
	keys, _ := json.Marshal(d.Keys)
	chans, _ := json.Marshal(d.ChannelIDs)
	runs, _ := json.Marshal(d.RunLeases)
	_, err := t.exec(ctx, "INSERT INTO tenant_deletions ("+deletionCols+") VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', 0, '', ?, NULL, NULL)",
		tn.ID, t.s.d.timeArg(now), clip(by, 400), string(keys), string(chans), string(runs), t.s.d.timeArg(now))
	return d, err
}

// each runs a query in the transaction and calls fn per row.
func (t *Tx) each(ctx context.Context, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DueDeletions are the tenants whose deletion the reaper may take now: deleting since before
// settled, not yet tombstoned, their next attempt due.
func (s *Store) DueDeletions(ctx context.Context, settled time.Time) ([]string, error) {
	return s.ids(ctx, `SELECT t.id FROM tenants t JOIN tenant_deletions d ON d.tenant_id = t.id
		WHERE t.deleting_at IS NOT NULL AND t.deleting_at < ? AND t.deleted_at IS NULL AND d.next_attempt_at <= ? ORDER BY t.deleting_at`,
		s.d.timeArg(settled), s.d.timeArg(s.Now()))
}

// LateSweeps are the tombstones the reaper checks again: tombstoned since after since, not checked
// since checked.
func (s *Store) LateSweeps(ctx context.Context, since, checked time.Time) ([]string, error) {
	return s.ids(ctx, `SELECT t.id FROM tenants t JOIN tenant_deletions d ON d.tenant_id = t.id
		WHERE t.deleted_at IS NOT NULL AND t.deleted_at > ? AND (d.last_checked_at IS NULL OR d.last_checked_at < ?) ORDER BY t.deleted_at`,
		s.d.timeArg(since), s.d.timeArg(checked))
}

func (s *Store) ids(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.query(ctx, q, args...)
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

// GetTenantAnyByID returns a tenant by id in any phase.
func (s *Store) GetTenantAnyByID(ctx context.Context, id string) (Tenant, error) {
	t, err := scanTenant(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+tenantCols+" FROM tenants WHERE id = ?"), id))
	return t, notFound(err, "tenant")
}

// reapStep is one table of a tenant's rows, children before parents: where selects the tenant's
// rows with one ? for the tenant id, or, with byChannel, its channel ids from the deletion record
// (purged_slots has neither a tenant id nor a foreign key).
type reapStep struct {
	table, where string
	byChannel    bool
}

const (
	ofFeeds     = "feed_id IN (SELECT id FROM feeds WHERE tenant_id = ?)"
	ofUpstreams = "upstream_id IN (SELECT id FROM upstreams WHERE tenant_id = ?)"
	ofChannels  = "channel_id IN (SELECT id FROM channels WHERE tenant_id = ?)"
	ofTenant    = "tenant_id = ?"
)

// reapSteps is the order the reaper deletes a tenant's rows in (spec 0019). TestReapCompleteness
// classifies every table of the schema against it.
var reapSteps = []reapStep{
	{"feed_refusals", ofFeeds, false}, {"feed_cursors", ofFeeds, false}, {"feed_channels", ofFeeds, false}, {"feed_keys", ofFeeds, false},
	{"feeds", ofTenant, false}, {"feed_kept", ofTenant, false},
	{"statements", ofTenant, false}, {"statement_counters", ofTenant, false},
	{"attachments", ofTenant, false},
	{"attestation_keys", ofTenant, false},
	{"package_group_grants", ofTenant, false}, {"package_group_members", ofTenant, false}, {"package_groups", ofTenant, false},
	{"grants", ofTenant, false}, {"issuer_console_clients", "issuer_id IN (SELECT id FROM issuers WHERE tenant_id = ?)", false},
	{"issuers", ofTenant, false},
	{"publisher_keys", ofTenant, false}, {"publishers", ofTenant, false}, {"removed_distributor_publishers", ofTenant, false},
	{"withdrawn_versions", ofTenant, false}, {"entry_meta", ofTenant, false}, // spec 0036: its icons go with the collector
	{"license_syncs", ofTenant, false}, {"licenses", ofTenant, false}, {"usage_forwards", ofTenant, false},
	{"upstream_revocations", ofTenant, false}, {"upstream_blocks", ofTenant, false}, {"upstream_yanks", ofTenant, false}, // spec 0039
	{"upstream_names", ofUpstreams, false}, {"upstream_asks", ofUpstreams, false}, {"upstream_cells", ofUpstreams, false}, {"upstream_versions", ofUpstreams, false}, {"upstream_entries", ofUpstreams, false},
	{"upstream_platforms", ofUpstreams, false}, {"upstream_keys", ofUpstreams, false}, {"upstreams", ofTenant, false},
	{"bundle_upload_chunks", "upload_id IN (SELECT id FROM bundle_uploads WHERE tenant_id = ?)", false}, {"bundle_uploads", ofTenant, false},
	{"bundle_imports", ofTenant, false}, {"shadows", ofTenant, false}, {"blocks", ofTenant, false},
	{"channel_sources", ofChannels, false}, {"channel_follows", ofChannels, false}, {"follow_skips", ofChannels, false},
	// by indexed columns: release_signatures by key, releases by channel
	{"release_signatures", "key_id IN (SELECT id FROM channel_keys WHERE tenant_id = ?)", false}, {"releases", ofChannels, false},
	{"purged_slots", "", true}, {"build_license_keys", ofTenant, false}, {"builds", ofTenant, false},
	{"key_events", "key_id IN (SELECT id FROM channel_keys WHERE tenant_id = ?)", false}, {"channel_keys", ofTenant, false},
	{"channel_duckdb_versions", ofChannels, false}, {"channels", ofTenant, false},
	{"download_counts", ofTenant, false}, {"download_installers", ofTenant, false},
	{"usage_counts", ofTenant, false}, {"usage_users", ofTenant, false}, {"usage_received", ofTenant, false},
}

// KeptTables hold a deleted tenant's rows for good (spec 0019): its tombstone, its deletion record,
// its audiences and key fingerprints (never reused), and its events (until retention).
var KeptTables = []string{"tenants", "tenant_deletions", "tenant_audiences", "key_fingerprints", "events"}

// GlobalTables are the server's, of no tenant.
var GlobalTables = []string{"blobs", "blob_claims", "blob_tombstones", "icon_claims", "icon_tombstones", "storage_domains", "duckdb_versions", "duckdb_version_c_apis",
	"deployment", "leases", "event_sinks", "statistics_days", "schema_migrations",
	"bootstrap",                                                   // spec 0020: the bootstrap names its tenant, and outlives it
	"reserved_names", "reserved_name_sources", "reserved_version", // spec 0030
	// spec 0036: weekly sums over tenants, naming none; a row may be one public tenant's figure alone,
	// and a deleted tenant's share stays in them up to 106 weeks
	"distributor_weeks", "distributor_weeks_done"}

// TenantRows counts a tenant's rows in every table the reaper deletes from (purged_slots by the
// given channel ids).
func TenantRows(ctx context.Context, s *Store, tenantID string, channelIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, st := range reapSteps {
		for _, p := range st.predicates(tenantID, channelIDs) {
			var n int64
			if err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM "+st.table+" WHERE "+p[0].(string)), p[1].([]any)...).Scan(&n); err != nil {
				return nil, err
			}
			out[st.table] += n
		}
	}
	return out, nil
}

// ReapTables are the tables the reaper deletes from, in order.
func ReapTables() []string {
	out := make([]string, len(reapSteps))
	for i, s := range reapSteps {
		out[i] = s.table
	}
	return out
}

// channelChunk bounds the channel ids one statement names (SQL Server takes 2,100 parameters).
const channelChunk = 500

// predicate is a step's condition and its arguments; for byChannel, one per chunk of channel ids.
func (st reapStep) predicates(tenantID string, channelIDs []string) [][2]any {
	if !st.byChannel {
		return [][2]any{{st.where, []any{tenantID}}}
	}
	var out [][2]any
	for i := 0; i < len(channelIDs); i += channelChunk {
		chunk := channelIDs[i:min(i+channelChunk, len(channelIDs))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		out = append(out, [2]any{"channel_id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ") + ")", args})
	}
	return out
}

// deleteBatch deletes at most ReapBatch rows of table matching where, through a bounded subquery
// rather than a list of keys (composite keys would exceed SQL Server's parameters).
func (t *Tx) deleteBatch(ctx context.Context, table, where string, args []any) (int64, error) {
	var q string
	switch t.s.d.Name {
	case "postgres":
		q = fmt.Sprintf("DELETE FROM %s WHERE ctid IN (SELECT ctid FROM %s WHERE %s LIMIT %d)", table, table, where, ReapBatch)
	case "sqlserver":
		q = fmt.Sprintf("DELETE TOP (%d) FROM %s WHERE %s", ReapBatch, table, where)
	default:
		q = fmt.Sprintf("DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s LIMIT %d)", table, table, where, ReapBatch)
	}
	res, err := t.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReapProgress is called after every batch with the table and the rows it deleted; an error (a
// lost lease) stops the pass.
type ReapProgress func(table string, deleted int64) error

// ReapTenant deletes a deleting tenant's rows in batches, step by step, and then writes its
// tombstone with a tenant.purge event; it starts again from the first table that still has a row.
// It is resumable: nothing is held between batches. It returns the rows deleted.
func (s *Store) ReapTenant(ctx context.Context, tn Tenant, d Deletion, progress ReapProgress) (int64, error) {
	if tn.DeletingAt.IsZero() || !tn.DeletedAt.IsZero() {
		return 0, fmt.Errorf("%w: the tenant is not being deleted", ErrInvalid)
	}
	var total int64
	for range 100 { // a writer that keeps adding rows is a bug; do not loop forever
		n, err := s.reapRows(ctx, tn.ID, d.ChannelIDs, progress)
		total += n
		if err != nil {
			return total, err
		}
		done := false
		err = s.tx(ctx, nil, func(tx *Tx) error {
			done = false
			left, err := tx.rowsLeft(ctx, tn.ID, d.ChannelIDs)
			if err != nil || left != "" {
				return err
			}
			now := tx.Now()
			res, err := tx.exec(ctx, "UPDATE tenants SET deleted_at = ?, display_name = '', holder_id = NULL, version = version + 1 WHERE id = ? AND deleted_at IS NULL",
				tx.s.d.timeArg(now), tn.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				done = true // another reaper wrote it
				return err
			}
			if _, err := tx.exec(ctx, "UPDATE tenant_deletions SET finished_at = ?, last_checked_at = ?, current_table = '', last_error = '', passes = passes + 1, deleted_rows = deleted_rows + ? WHERE tenant_id = ?",
				tx.s.d.timeArg(now), tx.s.d.timeArg(now), total, tn.ID); err != nil {
				return err
			}
			for _, l := range d.RunLeases { // held or not: their upstream or feed is gone
				if _, err := tx.exec(ctx, "DELETE FROM leases WHERE name = ? OR name LIKE ?", l, l+"/%"); err != nil {
					return err
				}
			}
			done = true
			return tx.Event(ctx, tn.ID, "system:tenant-delete", "tenant.purge", "tenant:"+tn.Name,
				map[string]any{"rows": d.DeletedRows + total, "passes": d.Passes + 1, "late": false})
		})
		if err != nil || done {
			return total, err
		}
	}
	return total, fmt.Errorf("%w: rows keep appearing under a deleting tenant", ErrConflict)
}

// reapRows runs every step until its table has no row of the tenant.
func (s *Store) reapRows(ctx context.Context, tenantID string, channelIDs []string, progress ReapProgress) (int64, error) {
	var total int64
	for _, st := range reapSteps {
		if st.table == "release_signatures" {
			// channels.serving_key_id points at channel_keys: cleared before the keys go
			if err := s.tx(ctx, nil, func(tx *Tx) error {
				_, err := tx.exec(ctx, "UPDATE channels SET serving_key_id = NULL WHERE tenant_id = ? AND serving_key_id IS NOT NULL", tenantID)
				return err
			}); err != nil {
				return total, err
			}
		}
		for _, p := range st.predicates(tenantID, channelIDs) {
			for {
				var n int64
				if err := s.tx(ctx, nil, func(tx *Tx) error {
					var err error
					n, err = tx.deleteBatch(ctx, st.table, p[0].(string), p[1].([]any))
					return err
				}); err != nil {
					return total, fmt.Errorf("store: reaping %s: %w", st.table, err)
				}
				total += n
				if progress != nil {
					if err := progress(st.table, n); err != nil {
						return total, err
					}
				}
				if n < ReapBatch {
					break
				}
			}
		}
	}
	return total, nil
}

// rowsLeft names the first table in the list that still has a row of the tenant, or "".
func (t *Tx) rowsLeft(ctx context.Context, tenantID string, channelIDs []string) (string, error) {
	for _, st := range reapSteps {
		for _, p := range st.predicates(tenantID, channelIDs) {
			var found int
			if err := t.queryRow(ctx, "SELECT CASE WHEN EXISTS (SELECT 1 FROM "+st.table+" WHERE "+p[0].(string)+") THEN 1 ELSE 0 END",
				p[1].([]any)...).Scan(&found); err != nil {
				return "", err
			}
			if found == 1 {
				return st.table, nil
			}
		}
	}
	return "", nil
}

// SweepTombstone checks a tombstone again (spec 0019's late sweep): rows found under it are deleted
// and reported with a tenant.purge marked late. It returns the rows deleted.
func (s *Store) SweepTombstone(ctx context.Context, tn Tenant, d Deletion) (int64, error) {
	var left string
	if err := s.tx(ctx, nil, func(tx *Tx) error {
		var err error
		left, err = tx.rowsLeft(ctx, tn.ID, d.ChannelIDs)
		return err
	}); err != nil {
		return 0, err
	}
	var n int64
	if left != "" {
		var err error
		if n, err = s.reapRows(ctx, tn.ID, d.ChannelIDs, nil); err != nil {
			return n, err
		}
	}
	return n, s.tx(ctx, nil, func(tx *Tx) error {
		if _, err := tx.exec(ctx, "UPDATE tenant_deletions SET last_checked_at = ?, deleted_rows = deleted_rows + ? WHERE tenant_id = ?",
			tx.s.d.timeArg(tx.Now()), n, tn.ID); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return tx.Event(ctx, tn.ID, "system:tenant-delete", "tenant.purge", "tenant:"+tn.Name,
			map[string]any{"rows": n, "passes": d.Passes, "late": true})
	})
}

// DeletionFailed records a failed pass: its error class, and the next attempt after a backoff.
// It reports whether the class changed (so the caller writes a failed tenant.purge).
// It reports nothing for a deletion another reaper finished meanwhile. backoff gives the wait
// after the given number of failed passes in a row.
func (s *Store) DeletionFailed(ctx context.Context, tenantID, class string, backoff func(failures int64) time.Duration, table string,
	deleted int64) (bool, error) {
	changed := false
	err := s.tx(ctx, nil, func(tx *Tx) error {
		changed = false
		var last string
		var failures int64
		err := tx.queryRow(ctx, "SELECT last_error, failures FROM tenant_deletions WHERE tenant_id = ? AND finished_at IS NULL", tenantID).
			Scan(&last, &failures)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		changed = last != class
		if table == "" { // failed before a table: the one reached so far stays
			if err := tx.queryRow(ctx, "SELECT current_table FROM tenant_deletions WHERE tenant_id = ?", tenantID).Scan(&table); err != nil {
				return err
			}
		}
		_, err = tx.exec(ctx, "UPDATE tenant_deletions SET last_error = ?, failures = failures + 1, next_attempt_at = ?, current_table = ?, "+
			"passes = passes + 1, deleted_rows = deleted_rows + ? WHERE tenant_id = ?",
			class, tx.s.d.timeArg(tx.Now().Add(backoff(failures+1))), table, deleted, tenantID)
		return err
	})
	return changed, err
}

// DeletionPaused records a pass that stopped without an error (runs still holding their leases):
// the next attempt, and the rows deleted so far.
// A pause is not a pass, and ends a run of failures.
func (s *Store) DeletionPaused(ctx context.Context, tenantID string, next time.Time, table string, deleted int64) error {
	return s.tx(ctx, nil, func(tx *Tx) error {
		q := "UPDATE tenant_deletions SET next_attempt_at = ?, failures = 0, deleted_rows = deleted_rows + ? WHERE tenant_id = ? AND finished_at IS NULL"
		args := []any{tx.s.d.timeArg(next), deleted, tenantID}
		if table != "" {
			q = "UPDATE tenant_deletions SET next_attempt_at = ?, failures = 0, deleted_rows = deleted_rows + ?, current_table = ? WHERE tenant_id = ? AND finished_at IS NULL"
			args = []any{tx.s.d.timeArg(next), deleted, table, tenantID}
		}
		_, err := tx.exec(ctx, q, args...)
		return err
	})
}

// RunLeases are the leases of a tenant's upstream, licence relay and feed runs, and its channels'
// re-signers.
func (s *Store) RunLeases(ctx context.Context, tenantID string, channelIDs []string) ([]string, error) {
	var names []string
	ups, err := s.ids(ctx, "SELECT id FROM upstreams WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, err
	}
	for _, id := range ups {
		names = append(names, "kista/upstream/"+id, LicenseRelayLease(id))
	}
	feeds, err := s.ids(ctx, "SELECT id FROM feeds WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, err
	}
	for _, id := range feeds {
		names = append(names, "kista/feed/"+id)
	}
	for _, id := range channelIDs {
		names = append(names, "resign/"+id)
	}
	return names, nil
}

// HeldLeases returns which of names are held now (not expired).
func (s *Store) HeldLeases(ctx context.Context, names []string) ([]string, error) {
	var held []string
	now := s.Now()
	for _, n := range names {
		_, until, found, err := s.LeaseState(ctx, n)
		if err != nil {
			return nil, err
		}
		if found && until.After(now) {
			held = append(held, n)
		}
	}
	return held, nil
}

// DeleteExpiredLeases removes the rows of the named leases, and of the pull-through cell leases
// under an upstream's, that have expired.
func (s *Store) DeleteExpiredLeases(ctx context.Context, names []string) error {
	return s.tx(ctx, nil, func(tx *Tx) error {
		now := tx.s.d.timeArg(tx.Now())
		for _, n := range names {
			if _, err := tx.exec(ctx, "DELETE FROM leases WHERE name = ? AND expires_at < ?", n, now); err != nil {
				return err
			}
			if strings.HasPrefix(n, "kista/upstream/") {
				if _, err := tx.exec(ctx, "DELETE FROM leases WHERE name LIKE ? AND expires_at < ?", n+"/%", now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
