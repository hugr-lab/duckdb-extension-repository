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

// ErrStreamBusy is a write meeting an object the collector is deleting, a stream or an icon (spec
// 0036): it waits and tries again. (Named for streams, the first kind.)
var ErrStreamBusy = errors.New("store: the stream is being deleted; try again")

// ErrClaimLost is a write whose claim expired or was removed before its row was written, a stream's
// or an icon's: it claims and uploads again.
var ErrClaimLost = errors.New("store: the stream's claim was lost")

// ErrBuildGone is a release insert whose Build was collected meanwhile: the intake starts again
// from its commit.
var ErrBuildGone = errors.New("store: the release's build is gone")

// StreamLock is the store lock of a stream in a domain.
func StreamLock(domain, stream string) string { return "kista/blob/" + domain + "/" + stream }

// objects are what spec 0016's protocol claims, marks and deletes in a storage domain, by their
// hash: streams, and entries' icons (spec 0036), each kind in tables of its own.
type objects struct {
	claims, tombstones, col string
	lock                    func(domain, hash string) string
	// count counts the rows that reference an object (the domain and the hash its arguments); refs
	// selects the hashes the rows reference among a list (the domain, then the list: %s)
	count, refs string
}

var (
	streamObjects = objects{claims: "blob_claims", tombstones: "blob_tombstones", col: "stream_hash", lock: StreamLock,
		count: "SELECT COUNT(*) FROM blobs WHERE domain = ? AND stream_hash = ?",
		refs:  "SELECT stream_hash FROM blobs WHERE domain = ? AND stream_hash IN (%s)"}
	// an icon is referenced by the entries' metadata of the domain's tenants
	iconObjects = objects{claims: "icon_claims", tombstones: "icon_tombstones", col: "sha256", lock: IconLock,
		count: "SELECT COUNT(*) FROM entry_meta m JOIN tenants t ON t.id = m.tenant_id WHERE t.storage_domain = ? AND m.icon_sha256 = ?",
		refs: "SELECT m.icon_sha256 FROM entry_meta m JOIN tenants t ON t.id = m.tenant_id " +
			"WHERE t.storage_domain = ? AND m.icon_sha256 IN (%s)"}
)

// IconLock is the store lock of an icon in a domain (spec 0036).
func IconLock(domain, sha256 string) string { return "kista/blob/" + domain + "/icon/" + sha256 }

// Objects are one kind of a storage domain's objects under spec 0016's protocol: claimed before
// they are uploaded, marked when nothing references them, deleted a grace later.
type Objects struct {
	s *Store
	k objects
}

// Streams are the bodies' streams (spec 0016).
func (s *Store) Streams() Objects { return Objects{s, streamObjects} }

// Icons are the entries' icons (spec 0036), icons/<sha256>.
func (s *Store) Icons() Objects { return Objects{s, iconObjects} }

// Claim claims an object for a write until its deadline (step 1): it removes the object's
// tombstone, or answers ErrStreamBusy while the collector is deleting it.
func (o Objects) Claim(ctx context.Context, domain, hash, claimID string, until time.Time) error {
	k := o.k
	return o.s.InTx(ctx, k.lock(domain, hash), func(tx *Tx) error {
		var state string
		err := tx.queryRow(ctx, "SELECT state FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ?", domain, hash).Scan(&state)
		switch {
		case err == nil && state == "deleting":
			return ErrStreamBusy
		case err == nil:
			if _, err := tx.exec(ctx, "DELETE FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ?", domain, hash); err != nil {
				return err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.exec(ctx, "INSERT INTO "+k.claims+" (domain, "+k.col+", claim_id, expires_at) VALUES (?, ?, ?, ?)",
			domain, hash, claimID, tx.s.d.timeArg(until))
		return err
	})
}

// ReleaseClaim removes a write's claim (a write giving up), under the object's lock.
func (o Objects) ReleaseClaim(ctx context.Context, domain, hash, claimID string) error {
	k := o.k
	return o.s.InTx(ctx, k.lock(domain, hash), func(tx *Tx) error {
		_, err := tx.exec(ctx, "DELETE FROM "+k.claims+" WHERE domain = ? AND "+k.col+" = ? AND claim_id = ?", domain, hash, claimID)
		return err
	})
}

// CommitIcon ends an icon's write (spec 0036, step 4) in the transaction that writes the row
// referencing it, which holds the icon's lock: it checks the write's claim is still live
// (ErrClaimLost otherwise), and removes the claim and any tombstone of the icon.
func (t *Tx) CommitIcon(ctx context.Context, domain, sha256, claimID string) error {
	k := iconObjects
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM "+k.claims+" WHERE domain = ? AND "+k.col+" = ? AND claim_id = ? AND expires_at > ?",
		domain, sha256, claimID, t.s.d.timeArg(t.Now())).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrClaimLost
	}
	if _, err := t.exec(ctx, "DELETE FROM "+k.claims+" WHERE domain = ? AND "+k.col+" = ? AND claim_id = ?", domain, sha256, claimID); err != nil {
		return err
	}
	// defensive, as CommitBlob's: the claim removed the tombstone, and a live claim keeps the listing
	// from marking it again, so none should be here
	_, err := t.exec(ctx, "DELETE FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ?", domain, sha256)
	return err
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
				_, err := tx.mark(ctx, streamObjects, b.Domain, old, true)
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

// referenced reports whether a row references an object or a live claim holds it.
func (t *Tx) referenced(ctx context.Context, k objects, domain, hash string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT ("+k.count+") + (SELECT COUNT(*) FROM "+k.claims+" WHERE domain = ? AND "+k.col+" = ? AND expires_at > ?)",
		domain, hash, domain, hash, t.s.d.timeArg(t.Now())).Scan(&n)
	return n > 0, err
}

// mark writes an object's tombstone (the object's lock held): only where none is, or with since
// reset when reset (a commit's replace). An object a row references or a live claim holds is not
// marked. It reports whether it wrote a new tombstone.
func (t *Tx) mark(ctx context.Context, k objects, domain, hash string, reset bool) (bool, error) {
	if ref, err := t.referenced(ctx, k, domain, hash); err != nil || ref {
		return false, err
	}
	var state string
	err := t.queryRow(ctx, "SELECT state FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ?", domain, hash).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = t.exec(ctx, "INSERT INTO "+k.tombstones+" (domain, "+k.col+", since, state) VALUES (?, ?, ?, 'marked')",
			domain, hash, t.s.d.timeArg(t.Now()))
		return err == nil, err
	case err != nil:
		return false, err
	case reset && state == "marked":
		_, err = t.exec(ctx, "UPDATE "+k.tombstones+" SET since = ? WHERE domain = ? AND "+k.col+" = ?",
			t.s.d.timeArg(t.Now()), domain, hash)
		return false, err
	}
	return false, nil
}

// Mark marks an object found in a domain's listing (where unmarked, unreferenced, unclaimed). It
// reports whether it marked it.
func (o Objects) Mark(ctx context.Context, domain, hash string) (bool, error) {
	marked := false
	err := o.s.InTx(ctx, o.k.lock(domain, hash), func(tx *Tx) error {
		var err error
		marked, err = tx.mark(ctx, o.k, domain, hash, false)
		return err
	})
	return marked, err
}

// buildDead is the condition of a Build no release references and not used since the cutoff (?).
// Migration 0013 set used_at of the Builds before it, every Build since is inserted with it, and
// FillUsedAt sets it on the few an older replica inserted after. A build landed in the catalogue
// without a release (spec 0033: a manual rule's new version) is kept until a release takes it, and a
// withdrawn one for good (spec 0033 phase B: its record keeps its body from arriving again).
const buildDead = "b.used_at < ? AND b.landed = ? AND b.withdrawn_at IS NULL AND NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id)"

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

// FillBodyLen sets body_len of the domain's Builds that have none (inserted by a replica older than
// migration 0047 that was still running) from their blobs (spec 0038: a tenant's bytes count such a
// row as 0 until then); a row whose blob is gone keeps none. Domain "" is every domain's (kista
// serve's start, for a server whose collector does not run).
func (s *Store) FillBodyLen(ctx context.Context, domain string) (int, error) {
	n := 0
	blob := "SELECT bl.body_len FROM blobs bl JOIN tenants t ON bl.domain = t.storage_domain WHERE t.id = builds.tenant_id AND bl.body_hash = builds.body_hash"
	q, args := "UPDATE builds SET body_len = ("+blob+") WHERE body_len IS NULL AND EXISTS ("+blob+")", []any{}
	if domain != "" {
		q, args = q+" AND tenant_id IN (SELECT id FROM tenants WHERE storage_domain = ?)", []any{domain}
	}
	err := s.tx(ctx, nil, func(t *Tx) error {
		res, err := t.exec(ctx, q, args...)
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
	err := s.eachRow(ctx, q+s.limit(limit), []any{domain, after, s.d.timeArg(cutoff), false}, func(sc func(...any) error) error {
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
		res, err := t.exec(ctx, "DELETE FROM builds WHERE id = ? AND used_at < ? AND landed = ? AND withdrawn_at IS NULL AND "+
			"NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)", id, t.s.d.timeArg(cutoff), false)
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
		_, err = tx.mark(ctx, streamObjects, domain, d.StreamHash, false)
		return err
	})
	return deleted, err
}

// Unreferenced returns the objects among hashes that no row of the domain references and no
// tombstone names: what the listing marks (Mark skips the ones a live claim holds).
func (o Objects) Unreferenced(ctx context.Context, domain string, hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	k := o.k
	known := map[string]bool{}
	for start := 0; start < len(hashes); start += 200 {
		batch := hashes[start:min(start+200, len(hashes))]
		ph := strings.TrimSuffix(strings.Repeat("?, ", len(batch)), ", ")
		args := []any{domain}
		for _, h := range batch {
			args = append(args, h)
		}
		for _, q := range []string{
			fmt.Sprintf(k.refs, ph),
			"SELECT " + k.col + " FROM " + k.tombstones + " WHERE domain = ? AND " + k.col + " IN (" + ph + ")",
		} {
			if err := o.s.eachRow(ctx, q, args, func(sc func(...any) error) error {
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

// Tombstone is an object the collector marked.
type Tombstone struct {
	Hash       string // the object's: a stream's hash, or an icon's (spec 0036)
	Since      time.Time
	State      string
	DeletingAt time.Time
}

// DueTombstones lists up to limit tombstones of a domain, after the hash after, to act on: marked
// before cutoff, or deleting since before stale.
func (o Objects) DueTombstones(ctx context.Context, domain string, cutoff, stale time.Time, after string, limit int) ([]Tombstone, error) {
	k := o.k
	q := "SELECT " + k.col + ", since, state, deleting_at FROM " + k.tombstones + " WHERE domain = ? AND " + k.col + " > ? AND " +
		"(state = 'marked' AND since < ? OR state = 'deleting' AND deleting_at < ?) ORDER BY " + k.col + o.s.limit(limit)
	var out []Tombstone
	err := o.s.eachRow(ctx, q, []any{domain, after, o.s.d.timeArg(cutoff), o.s.d.timeArg(stale)}, func(sc func(...any) error) error {
		var t Tombstone
		if err := sc(&t.Hash, scanTime{&t.Since}, &t.State, scanTime{&t.DeletingAt}); err != nil {
			return err
		}
		out = append(out, t)
		return nil
	})
	return out, err
}

// BeginDelete moves a due tombstone to deleting (the checks repeated under the object's lock:
// still due, no row, no live claim). A tombstone whose object has a row or a live claim is removed.
// It returns the delete's stamp (its deleting_at), zero when the object may not be deleted now.
func (o Objects) BeginDelete(ctx context.Context, domain, hash string, cutoff, stale time.Time) (time.Time, error) {
	k := o.k
	var stamp time.Time
	err := o.s.InTx(ctx, k.lock(domain, hash), func(tx *Tx) error {
		stamp = time.Time{}
		if ref, err := tx.referenced(ctx, k, domain, hash); err != nil || ref {
			if err == nil {
				_, err = tx.exec(ctx, "DELETE FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ?", domain, hash)
			}
			return err
		}
		now := tx.Now().Truncate(time.Microsecond) // the stamp is compared as stored
		res, err := tx.exec(ctx, "UPDATE "+k.tombstones+" SET state = 'deleting', deleting_at = ? WHERE domain = ? AND "+k.col+" = ? AND "+
			"(state = 'marked' AND since < ? OR state = 'deleting' AND deleting_at < ?)",
			tx.s.d.timeArg(now), domain, hash, tx.s.d.timeArg(cutoff), tx.s.d.timeArg(stale))
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
func (o Objects) StillDeleting(ctx context.Context, domain, hash string, stamp time.Time) (bool, error) {
	k := o.k
	var n int
	err := o.s.db.QueryRowContext(ctx, o.s.d.rebind("SELECT COUNT(*) FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+
		" = ? AND state = 'deleting' AND deleting_at = ?"), domain, hash, o.s.d.timeArg(stamp)).Scan(&n)
	return n == 1, err
}

// EndDelete removes the tombstone of a delete begun at stamp once its object is deleted.
func (o Objects) EndDelete(ctx context.Context, domain, hash string, stamp time.Time) error {
	k := o.k
	return o.s.InTx(ctx, k.lock(domain, hash), func(tx *Tx) error {
		_, err := tx.exec(ctx, "DELETE FROM "+k.tombstones+" WHERE domain = ? AND "+k.col+" = ? AND state = 'deleting' AND deleting_at = ?",
			domain, hash, tx.s.d.timeArg(stamp))
		return err
	})
}

// AbortDelete returns the tombstone of a delete begun at stamp that the store refused to marked (its
// since kept): writes of the object go on, and a later pass tries again.
func (o Objects) AbortDelete(ctx context.Context, domain, hash string, stamp time.Time) error {
	k := o.k
	return o.s.InTx(ctx, k.lock(domain, hash), func(tx *Tx) error {
		_, err := tx.exec(ctx, "UPDATE "+k.tombstones+" SET state = 'marked', deleting_at = NULL WHERE domain = ? AND "+k.col+" = ? AND "+
			"state = 'deleting' AND deleting_at = ?", domain, hash, tx.s.d.timeArg(stamp))
		return err
	})
}

// RemoveExpiredClaims removes a domain's claims expired before now (a write that never finished).
// It takes no object lock: an expired claim holds nothing already (every check of a claim is of a
// live one), so removing it changes no decision.
func (o Objects) RemoveExpiredClaims(ctx context.Context, domain string) (int, error) {
	res, err := o.s.db.ExecContext(ctx, o.s.d.rebind("DELETE FROM "+o.k.claims+" WHERE domain = ? AND expires_at < ?"), domain,
		o.s.d.timeArg(o.s.Now()))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
