package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Storage garbage collection (spec 0016): commits claim their stream before uploading it and write
// their row after; the collector marks streams no row references and deletes them a grace later.
// Every change to a stream's claims and tombstone holds the stream's lock.

// ErrStreamBusy is a commit meeting a stream the collector is deleting: it waits and tries again.
var ErrStreamBusy = errors.New("store: the stream is being deleted; try again")

// ErrClaimLost is a commit whose claim expired or was removed before its row was written: it claims
// and uploads again.
var ErrClaimLost = errors.New("store: the stream's claim was lost")

// ErrBuildGone is a release insert whose Build was collected meanwhile: the intake starts again
// from its commit.
var ErrBuildGone = errors.New("store: the release's build is gone")

// StreamLock is the store lock of a stream in a domain.
func StreamLock(domain, stream string) string { return "kista/blob/" + domain + "/" + stream }

// ClaimStream claims a stream for a commit until its deadline (commit step 1): it removes the
// stream's tombstone, or answers ErrStreamBusy while the collector is deleting it.
func (s *Store) ClaimStream(ctx context.Context, domain, stream, claimID string, until time.Time) error {
	return s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		var state string
		err := tx.queryRow(ctx, "SELECT state FROM blob_tombstones WHERE domain = ? AND stream_hash = ?", domain, stream).Scan(&state)
		switch {
		case err == nil && state == "deleting":
			return ErrStreamBusy
		case err == nil:
			if _, err := tx.exec(ctx, "DELETE FROM blob_tombstones WHERE domain = ? AND stream_hash = ?", domain, stream); err != nil {
				return err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.exec(ctx, "INSERT INTO blob_claims (domain, stream_hash, claim_id, expires_at) VALUES (?, ?, ?, ?)",
			domain, stream, claimID, tx.s.d.timeArg(until))
		return err
	})
}

// ReleaseClaim removes a commit's claim (a commit giving up), under the stream's lock.
func (s *Store) ReleaseClaim(ctx context.Context, domain, stream, claimID string) error {
	return s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		_, err := tx.exec(ctx, "DELETE FROM blob_claims WHERE domain = ? AND stream_hash = ? AND claim_id = ?", domain, stream, claimID)
		return err
	})
}

// CommitBlob writes a committed stream's row (commit step 3), holding the stream's lock and, when
// the row names another stream (spec 0005's replace), that one's too, taken in order: it checks the
// commit's claim is still live (ErrClaimLost otherwise), writes the row as PutBlob does, removes the
// claim and any tombstone of the stream, and marks a replaced stream.
func (s *Store) CommitBlob(ctx context.Context, b *Blob, claimID string) error {
	for range 3 { // the row's stream read before the locks; read again under them
		var old string
		err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT stream_hash FROM blobs WHERE domain = ? AND body_hash = ?"),
			b.Domain, b.BodyHash).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		locks := []string{StreamLock(b.Domain, b.StreamHash)}
		if old != "" && old != b.StreamHash {
			locks = append(locks, StreamLock(b.Domain, old))
		}
		slices.Sort(locks)
		again := false
		err = s.InTxLocks(ctx, locks, func(tx *Tx) error {
			again = false
			var cur string
			if err := tx.queryRow(ctx, "SELECT stream_hash FROM blobs WHERE domain = ? AND body_hash = ?", b.Domain, b.BodyHash).
				Scan(&cur); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if cur != old {
				again = true
				return nil
			}
			var n int
			if err := tx.queryRow(ctx, "SELECT COUNT(*) FROM blob_claims WHERE domain = ? AND stream_hash = ? AND claim_id = ? AND expires_at > ?",
				b.Domain, b.StreamHash, claimID, tx.s.d.timeArg(tx.Now())).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrClaimLost
			}
			if err := tx.PutBlob(ctx, b); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, "DELETE FROM blob_claims WHERE domain = ? AND stream_hash = ? AND claim_id = ?",
				b.Domain, b.StreamHash, claimID); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, "DELETE FROM blob_tombstones WHERE domain = ? AND stream_hash = ?", b.Domain, b.StreamHash); err != nil {
				return err
			}
			if old != "" && old != b.StreamHash { // replaced: readers may hold it from now on
				_, err := tx.markStream(ctx, b.Domain, old, true)
				return err
			}
			return nil
		})
		if err != nil || !again {
			return err
		}
	}
	return fmt.Errorf("%w: the body's stream kept changing", ErrBusy)
}

// referenced reports whether a row references a stream or a live claim holds it.
func (t *Tx) referenced(ctx context.Context, domain, stream string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT (SELECT COUNT(*) FROM blobs WHERE domain = ? AND stream_hash = ?) + "+
		"(SELECT COUNT(*) FROM blob_claims WHERE domain = ? AND stream_hash = ? AND expires_at > ?)",
		domain, stream, domain, stream, t.s.d.timeArg(t.Now())).Scan(&n)
	return n > 0, err
}

// markStream writes a stream's tombstone (the stream's lock held): only where none is, or with
// since reset when reset (a commit's replace). A stream a row references or a live claim holds is
// not marked. It reports whether it wrote a new tombstone.
func (t *Tx) markStream(ctx context.Context, domain, stream string, reset bool) (bool, error) {
	if ref, err := t.referenced(ctx, domain, stream); err != nil || ref {
		return false, err
	}
	var state string
	err := t.queryRow(ctx, "SELECT state FROM blob_tombstones WHERE domain = ? AND stream_hash = ?", domain, stream).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = t.exec(ctx, "INSERT INTO blob_tombstones (domain, stream_hash, since, state) VALUES (?, ?, ?, 'marked')",
			domain, stream, t.s.d.timeArg(t.Now()))
		return err == nil, err
	case err != nil:
		return false, err
	case reset && state == "marked":
		_, err = t.exec(ctx, "UPDATE blob_tombstones SET since = ? WHERE domain = ? AND stream_hash = ?",
			t.s.d.timeArg(t.Now()), domain, stream)
		return false, err
	}
	return false, nil
}

// MarkStream marks a stream found in a domain's listing (where unmarked, unreferenced, unclaimed).
// It reports whether it marked it.
func (s *Store) MarkStream(ctx context.Context, domain, stream string) (bool, error) {
	marked := false
	err := s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		var err error
		marked, err = tx.markStream(ctx, domain, stream, false)
		return err
	})
	return marked, err
}

// buildDead is the condition of a Build no release references and not used since the cutoff (?).
// Migration 0013 set used_at of the Builds before it, every Build since is inserted with it, and
// FillUsedAt sets it on the few an older replica inserted after.
const buildDead = "b.used_at < ? AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id)"

// FillUsedAt sets used_at of the domain's Builds that have none (inserted by a replica older than
// migration 0013 that was still running) to their created_at, so the collector can age them.
func (s *Store) FillUsedAt(ctx context.Context, domain string) (int, error) {
	n := 0
	err := s.tx(ctx, nil, func(t *Tx) error {
		res, err := t.exec(ctx, "UPDATE builds SET used_at = created_at WHERE used_at IS NULL AND "+
			"tenant_id IN (SELECT id FROM tenants WHERE storage_domain = ?)", domain)
		if err != nil {
			return err
		}
		m, err := res.RowsAffected()
		n = int(m)
		return err
	})
	return n, err
}

// DeadBuilds lists up to limit Builds of a domain's tenants, after the id after, that no release
// references and that were not used since cutoff.
func (s *Store) DeadBuilds(ctx context.Context, domain string, cutoff time.Time, after string, limit int) ([]string, error) {
	q := "SELECT b.id FROM builds b JOIN tenants t ON t.id = b.tenant_id WHERE t.storage_domain = ? AND b.id > ? AND " + buildDead + " ORDER BY b.id"
	var out []string
	err := s.eachRow(ctx, q+s.limit(limit), []any{domain, after, s.d.timeArg(cutoff)}, func(sc func(...any) error) error {
		var id string
		if err := sc(&id); err != nil {
			return err
		}
		out = append(out, id)
		return nil
	})
	return out, err
}

// limit is the clause that ends an ordered query at n rows.
func (s *Store) limit(n int) string {
	if s.d.Name == "sqlserver" {
		return fmt.Sprintf(" OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", n)
	}
	return fmt.Sprintf(" LIMIT %d", n)
}

// DeleteBuild deletes a Build if it is still dead (the checks in the statement); a release inserted
// meanwhile (the foreign key) or a lock the store could not get leaves it. It reports whether it was
// deleted.
func (s *Store) DeleteBuild(ctx context.Context, id string, cutoff time.Time) (bool, error) {
	deleted := false
	err := s.tx(ctx, nil, func(t *Tx) error {
		deleted = false
		res, err := t.exec(ctx, "DELETE FROM builds WHERE id = ? AND used_at < ? AND "+
			"NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)", id, t.s.d.timeArg(cutoff))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		deleted = n == 1
		return err
	})
	if s.d.foreignKey(err) || errors.Is(err, ErrBusy) {
		return false, nil
	}
	return deleted, err
}

// noBuild is the condition of a body row no Build of the domain's tenants references.
const noBuild = "NOT EXISTS (SELECT 1 FROM builds b JOIN tenants t ON t.id = b.tenant_id WHERE b.body_hash = bl.body_hash AND t.storage_domain = bl.domain)"

// DeadBody is a body row the collector may delete.
type DeadBody struct{ BodyHash, StreamHash string }

// DeadBodies lists up to limit body rows of a domain, after the body hash after, that no Build
// references, committed before cutoff.
func (s *Store) DeadBodies(ctx context.Context, domain string, cutoff time.Time, after string, limit int) ([]DeadBody, error) {
	q := "SELECT bl.body_hash, bl.stream_hash FROM blobs bl WHERE bl.domain = ? AND bl.committed_at < ? AND bl.body_hash > ? AND " + noBuild +
		" ORDER BY bl.body_hash" + s.limit(limit)
	var out []DeadBody
	err := s.eachRow(ctx, q, []any{domain, s.d.timeArg(cutoff), after}, func(sc func(...any) error) error {
		var d DeadBody
		if err := sc(&d.BodyHash, &d.StreamHash); err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	return out, err
}

// DeleteBody deletes a dead body row (the checks repeated in the statement, under its stream's
// lock; a stream a commit claims keeps its row) and marks its stream. It reports whether it was
// deleted.
func (s *Store) DeleteBody(ctx context.Context, domain string, d DeadBody, cutoff time.Time) (bool, error) {
	deleted := false
	err := s.InTx(ctx, StreamLock(domain, d.StreamHash), func(tx *Tx) error {
		deleted = false
		now := tx.s.d.timeArg(tx.Now())
		res, err := tx.exec(ctx, "DELETE FROM blobs WHERE domain = ? AND body_hash = ? AND stream_hash = ? AND committed_at < ? AND "+
			"NOT EXISTS (SELECT 1 FROM builds b JOIN tenants t ON t.id = b.tenant_id WHERE b.body_hash = blobs.body_hash AND t.storage_domain = blobs.domain) AND "+
			"NOT EXISTS (SELECT 1 FROM blob_claims c WHERE c.domain = blobs.domain AND c.stream_hash = blobs.stream_hash AND c.expires_at > ?)",
			domain, d.BodyHash, d.StreamHash, tx.s.d.timeArg(cutoff), now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		deleted = true
		_, err = tx.markStream(ctx, domain, d.StreamHash, false)
		return err
	})
	return deleted, err
}

// UnreferencedStreams returns the streams among hashes that no row of the domain references and no
// tombstone names: what the listing marks (MarkStream skips the ones a live claim holds).
func (s *Store) UnreferencedStreams(ctx context.Context, domain string, hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	for start := 0; start < len(hashes); start += 200 {
		batch := hashes[start:min(start+200, len(hashes))]
		ph := strings.TrimSuffix(strings.Repeat("?, ", len(batch)), ", ")
		args := []any{domain}
		for _, h := range batch {
			args = append(args, h)
		}
		for _, q := range []string{
			"SELECT stream_hash FROM blobs WHERE domain = ? AND stream_hash IN (" + ph + ")",
			"SELECT stream_hash FROM blob_tombstones WHERE domain = ? AND stream_hash IN (" + ph + ")",
		} {
			if err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
				var h string
				if err := sc(&h); err != nil {
					return err
				}
				known[h] = true
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	var out []string
	for _, h := range hashes {
		if !known[h] {
			out = append(out, h)
		}
	}
	return out, nil
}

// Tombstone is a stream the collector marked.
type Tombstone struct {
	StreamHash string
	Since      time.Time
	State      string
	DeletingAt time.Time
}

// DueTombstones lists up to limit tombstones of a domain, after the stream after, to act on: marked
// before cutoff, or deleting since before stale.
func (s *Store) DueTombstones(ctx context.Context, domain string, cutoff, stale time.Time, after string, limit int) ([]Tombstone, error) {
	q := "SELECT stream_hash, since, state, deleting_at FROM blob_tombstones WHERE domain = ? AND stream_hash > ? AND " +
		"(state = 'marked' AND since < ? OR state = 'deleting' AND deleting_at < ?) ORDER BY stream_hash" + s.limit(limit)
	var out []Tombstone
	err := s.eachRow(ctx, q, []any{domain, after, s.d.timeArg(cutoff), s.d.timeArg(stale)}, func(sc func(...any) error) error {
		var t Tombstone
		if err := sc(&t.StreamHash, scanTime{&t.Since}, &t.State, scanTime{&t.DeletingAt}); err != nil {
			return err
		}
		out = append(out, t)
		return nil
	})
	return out, err
}

// BeginDelete moves a due tombstone to deleting (the checks repeated under its stream's lock: still
// due, no row, no live claim). A tombstone whose stream has a row or a live claim is removed. It
// returns the delete's stamp (its deleting_at), zero when the object may not be deleted now.
func (s *Store) BeginDelete(ctx context.Context, domain, stream string, cutoff, stale time.Time) (time.Time, error) {
	var stamp time.Time
	err := s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		stamp = time.Time{}
		if ref, err := tx.referenced(ctx, domain, stream); err != nil || ref {
			if err == nil {
				_, err = tx.exec(ctx, "DELETE FROM blob_tombstones WHERE domain = ? AND stream_hash = ?", domain, stream)
			}
			return err
		}
		now := tx.Now().Truncate(time.Microsecond) // the stamp is compared as stored
		res, err := tx.exec(ctx, "UPDATE blob_tombstones SET state = 'deleting', deleting_at = ? WHERE domain = ? AND stream_hash = ? AND "+
			"(state = 'marked' AND since < ? OR state = 'deleting' AND deleting_at < ?)",
			tx.s.d.timeArg(now), domain, stream, tx.s.d.timeArg(cutoff), tx.s.d.timeArg(stale))
		if err != nil {
			return err
		}
		if m, err := res.RowsAffected(); err != nil || m != 1 {
			return err
		}
		stamp = now
		return nil
	})
	return stamp, err
}

// StillDeleting reports whether a delete begun at stamp is still this collector's: no other
// collector took the tombstone again since, nor finished it.
func (s *Store) StillDeleting(ctx context.Context, domain, stream string, stamp time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM blob_tombstones WHERE domain = ? AND stream_hash = ? AND state = 'deleting' AND deleting_at = ?"),
		domain, stream, s.d.timeArg(stamp)).Scan(&n)
	return n == 1, err
}

// EndDelete removes the tombstone of a delete begun at stamp once its object is deleted.
func (s *Store) EndDelete(ctx context.Context, domain, stream string, stamp time.Time) error {
	return s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		_, err := tx.exec(ctx, "DELETE FROM blob_tombstones WHERE domain = ? AND stream_hash = ? AND state = 'deleting' AND deleting_at = ?",
			domain, stream, tx.s.d.timeArg(stamp))
		return err
	})
}

// AbortDelete returns the tombstone of a delete begun at stamp that the store refused to marked (its
// since kept): commits of the stream go on, and a later pass tries again.
func (s *Store) AbortDelete(ctx context.Context, domain, stream string, stamp time.Time) error {
	return s.InTx(ctx, StreamLock(domain, stream), func(tx *Tx) error {
		_, err := tx.exec(ctx, "UPDATE blob_tombstones SET state = 'marked', deleting_at = NULL WHERE domain = ? AND stream_hash = ? AND "+
			"state = 'deleting' AND deleting_at = ?", domain, stream, tx.s.d.timeArg(stamp))
		return err
	})
}

// RemoveExpiredClaims removes a domain's claims expired before now (a commit that never finished).
// It takes no stream lock: an expired claim holds nothing already (every check of a claim is of a
// live one), so removing it changes no decision.
func (s *Store) RemoveExpiredClaims(ctx context.Context, domain string) (int, error) {
	res, err := s.db.ExecContext(ctx, s.d.rebind("DELETE FROM blob_claims WHERE domain = ? AND expires_at < ?"), domain, s.d.timeArg(s.Now()))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
