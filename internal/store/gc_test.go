package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func blob(body, stream string) store.Blob {
	return store.Blob{Domain: "default", BodyHash: strings.Repeat(body, 64), StreamHash: strings.Repeat(stream, 64), BodyLen: 10,
		StreamLen: 100, StreamChunks: make([]byte, 32)}
}

// Spec 0016: a commit's claim, its row, a replaced stream; the collector's builds, bodies and
// tombstones; a release whose Build was collected.
func TestStorageGC(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		hour := time.Hour
		at := func(d time.Duration) time.Time { return now.Add(d) }

		// a commit: claim, then the row; the claim and a tombstone go
		b := blob("a", "1")
		if err := s.ClaimStream(ctx, "default", b.StreamHash, "c1", at(hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitBlob(ctx, &b, "c1"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBlob(ctx, "default", b.BodyHash); err != nil || got.StreamHash != b.StreamHash {
			t.Fatalf("the row: %+v %v", got, err)
		}
		// a lost claim: the commit claims again
		b2 := blob("b", "2")
		if err := s.ClaimStream(ctx, "default", b2.StreamHash, "c2", at(time.Minute)); err != nil {
			t.Fatal(err)
		}
		now = at(2 * time.Minute)
		if err := s.CommitBlob(ctx, &b2, "c2"); !errors.Is(err, store.ErrClaimLost) {
			t.Fatalf("an expired claim: %v", err)
		}
		if n, err := s.RemoveExpiredClaims(ctx, "default"); err != nil || n != 1 {
			t.Fatalf("expired claims: %d %v", n, err)
		}
		// a replace: the old stream is marked
		r := blob("a", "3")
		noErr(t, s.ClaimStream(ctx, "default", r.StreamHash, "c3", at(hour)))
		noErr(t, s.CommitBlob(ctx, &r, "c3"))
		if ts, _ := s.DueTombstones(ctx, "default", at(hour), at(0), "", 10); len(ts) != 1 || ts[0].StreamHash != strings.Repeat("1", 64) {
			t.Fatalf("the replaced stream marked: %+v", ts)
		}
		// Builds: one with a release is live; one unused for the grace is dead; a used one is not
		live := cppBuild(tn, "tresor", "1.0", "a")
		dead := cppBuild(tn, "tresor", "2.0", "c")
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &live); err != nil {
				return err
			}
			return tx.FindOrInsertBuild(ctx, &dead)
		})
		rel := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: live.ID, Name: live.Name, ExtVersion: live.ExtVersion,
			Platform: live.Platform, Slot: live.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 1}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &rel, "os:1:t") })
		if ids, _ := s.DeadBuilds(ctx, "default", now, "", 10); len(ids) != 0 {
			t.Fatalf("builds within the grace: %v", ids)
		}
		now = at(25 * hour)
		cutoff := at(-24 * hour)
		ids, err := s.DeadBuilds(ctx, "default", cutoff, "", 10)
		if err != nil || len(ids) != 1 || ids[0] != dead.ID {
			t.Fatalf("dead builds: %v %v", ids, err)
		}
		// a Build an older replica inserted (no used_at) ages from its creation
		noErr(t, store.ExecRaw(ctx, s, "UPDATE builds SET used_at = NULL WHERE id = '"+dead.ID+"'"))
		if n, err := s.FillUsedAt(ctx, "default"); err != nil || n != 1 {
			t.Fatalf("fill used_at: %d %v", n, err)
		}
		if ids, _ := s.DeadBuilds(ctx, "default", cutoff, "", 10); len(ids) != 1 {
			t.Fatalf("a filled build: %v", ids)
		}
		// an intake finding it refreshes it: no longer dead
		again := cppBuild(tn, "tresor", "2.0", "c")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &again) })
		if ok, err := s.DeleteBuild(ctx, dead.ID, cutoff); err != nil || ok {
			t.Fatalf("a used build deleted: %v %v", ok, err)
		}
		now = at(25 * hour)
		cutoff = at(-24 * hour)
		if ok, err := s.DeleteBuild(ctx, dead.ID, cutoff); err != nil || !ok {
			t.Fatalf("delete a dead build: %v %v", ok, err)
		}
		if ok, _ := s.DeleteBuild(ctx, live.ID, cutoff); ok {
			t.Fatal("a released build deleted")
		}
		// a release whose Build is gone; finding it again inserts a new one
		gone := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: dead.ID, Name: dead.Name, ExtVersion: dead.ExtVersion,
			Platform: dead.Platform, Slot: dead.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 2}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertRelease(ctx, &gone, "os:1:t") }); !errors.Is(err, store.ErrBuildGone) {
			t.Fatalf("a release on a collected build: %v", err)
		}
		fresh := cppBuild(tn, "tresor", "2.0", "c")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &fresh) })
		if fresh.ID == dead.ID {
			t.Fatal("the collected build came back")
		}
		// bodies: a, referenced by a build, stays; b's row (none) and c's (a build) stay; an
		// unreferenced old row goes and its stream is marked
		d := blob("d", "4")
		noErr(t, s.ClaimStream(ctx, "default", d.StreamHash, "c4", at(hour)))
		noErr(t, s.CommitBlob(ctx, &d, "c4"))
		if ds, _ := s.DeadBodies(ctx, "default", at(-24*hour), "", 10); len(ds) != 0 {
			t.Fatalf("bodies within the grace: %+v", ds)
		}
		now = at(25 * hour)
		cutoff = at(-24 * hour)
		ds, err := s.DeadBodies(ctx, "default", cutoff, "", 10)
		if err != nil || len(ds) != 1 || ds[0].BodyHash != d.BodyHash {
			t.Fatalf("dead bodies (a has a build): %+v %v", ds, err)
		}
		// a commit claiming its stream keeps the row
		noErr(t, s.ClaimStream(ctx, "default", d.StreamHash, "c4b", at(hour)))
		if ok, err := s.DeleteBody(ctx, "default", ds[0], cutoff); err != nil || ok {
			t.Fatalf("a claimed body deleted: %v %v", ok, err)
		}
		noErr(t, s.ReleaseClaim(ctx, "default", d.StreamHash, "c4b"))
		// a body a Build references is not deleted (the check repeated in the statement)
		aBody := store.DeadBody{BodyHash: strings.Repeat("a", 64), StreamHash: strings.Repeat("3", 64)}
		if ok, err := s.DeleteBody(ctx, "default", aBody, cutoff); err != nil || ok {
			t.Fatalf("a referenced body deleted: %v %v", ok, err)
		}
		if ok, err := s.DeleteBody(ctx, "default", ds[0], cutoff); err != nil || !ok {
			t.Fatalf("delete a body: %v %v", ok, err)
		}
		// tombstones: the listing marks unknown streams only; due ones are deleted
		un, err := s.UnreferencedStreams(ctx, "default", []string{strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64)})
		if err != nil || len(un) != 1 || un[0] != strings.Repeat("5", 64) {
			t.Fatalf("unreferenced (3 has a row, 4 a tombstone): %v %v", un, err)
		}
		if marked, err := s.MarkStream(ctx, "default", un[0]); err != nil || !marked {
			t.Fatalf("mark: %v %v", marked, err)
		}
		if marked, err := s.MarkStream(ctx, "default", strings.Repeat("3", 64)); err != nil || marked {
			t.Fatalf("a referenced stream marked: %v %v", marked, err)
		}
		now = at(25 * hour)
		cutoff, stale := at(-24*hour), at(-10*time.Minute)
		ts, err := s.DueTombstones(ctx, "default", cutoff, stale, "", 10)
		if err != nil || len(ts) != 3 { // 1 (replaced), 4 (its body deleted), 5 (listed)
			t.Fatalf("due tombstones: %+v %v", ts, err)
		}
		// a commit meanwhile claims 4: its tombstone goes, and the collector cannot begin
		noErr(t, s.ClaimStream(ctx, "default", strings.Repeat("4", 64), "c5", at(hour)))
		if stamp, err := s.BeginDelete(ctx, "default", strings.Repeat("4", 64), cutoff, stale); err != nil || !stamp.IsZero() {
			t.Fatalf("a claimed stream: %v %v", stamp, err)
		}
		stamp, err := s.BeginDelete(ctx, "default", strings.Repeat("5", 64), cutoff, stale)
		if err != nil || stamp.IsZero() {
			t.Fatalf("begin: %v %v", stamp, err)
		}
		// while deleting, a commit of 5 waits
		if err := s.ClaimStream(ctx, "default", strings.Repeat("5", 64), "c6", at(hour)); !errors.Is(err, store.ErrStreamBusy) {
			t.Fatalf("a commit during the delete: %v", err)
		}
		// a deleting tombstone is not taken again before it is stale
		if again, _ := s.BeginDelete(ctx, "default", strings.Repeat("5", 64), cutoff, stale); !again.IsZero() {
			t.Fatal("a fresh deleting tombstone taken again")
		}
		// once stale, another collector takes it again: the first one's stamp is no longer its own
		five := strings.Repeat("5", 64)
		if mine, err := s.StillDeleting(ctx, "default", five, stamp); err != nil || !mine {
			t.Fatalf("still deleting: %v %v", mine, err)
		}
		now = at(31 * time.Minute)
		second, err := s.BeginDelete(ctx, "default", five, now.Add(-24*hour), now.Add(-30*time.Minute))
		if err != nil || second.IsZero() || second.Equal(stamp) {
			t.Fatalf("a stale delete taken again: %v %v", second, err)
		}
		if mine, _ := s.StillDeleting(ctx, "default", five, stamp); mine {
			t.Fatal("an overtaken delete still its collector's")
		}
		noErr(t, s.EndDelete(ctx, "default", five, stamp)) // the first collector's: no effect
		if err := s.ClaimStream(ctx, "default", five, "c6", at(hour)); !errors.Is(err, store.ErrStreamBusy) {
			t.Fatalf("an overtaken collector ended the delete: %v", err)
		}
		// a refused delete goes back to marked: commits go on
		noErr(t, s.AbortDelete(ctx, "default", five, second))
		if ts, _ := s.DueTombstones(ctx, "default", now.Add(-24*hour), now.Add(-30*time.Minute), "", 10); !hasStream(ts, five, "marked") {
			t.Fatalf("an aborted delete: %+v", ts)
		}
		second, err = s.BeginDelete(ctx, "default", five, now.Add(-24*hour), now.Add(-30*time.Minute))
		if err != nil || second.IsZero() {
			t.Fatalf("begin again: %v %v", second, err)
		}
		noErr(t, s.EndDelete(ctx, "default", five, second))
		if err := s.ClaimStream(ctx, "default", strings.Repeat("5", 64), "c6", at(hour)); err != nil {
			t.Fatalf("a commit after the delete: %v", err)
		}
		// a marked stream listed again keeps its since: two passes a grace apart delete it
		if marked, err := s.MarkStream(ctx, "default", strings.Repeat("6", 64)); err != nil || !marked {
			t.Fatalf("mark 6: %v %v", marked, err)
		}
		first := now
		now = at(23 * hour)
		if marked, err := s.MarkStream(ctx, "default", strings.Repeat("6", 64)); err != nil || marked {
			t.Fatalf("6 marked again: %v %v", marked, err)
		}
		now = first.Add(25 * hour)
		ts, _ = s.DueTombstones(ctx, "default", now.Add(-24*hour), now.Add(-10*time.Minute), "", 10)
		found := false
		for _, x := range ts {
			found = found || x.StreamHash == strings.Repeat("6", 64)
		}
		if !found {
			t.Fatal("a listing reset an existing tombstone")
		}
	})
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func hasStream(ts []store.Tombstone, stream, state string) bool {
	for _, t := range ts {
		if t.StreamHash == stream && t.State == state {
			return true
		}
	}
	return false
}
