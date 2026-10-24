package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Attachments (spec 0011): records, chains and heads, limits, the publisher's check, removal and
// tombstones, and what the collector finds when a body is gone (unless blocked).
func TestAttachments(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		b := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		doc := func(kind, origin, supersedes string) store.Attachment {
			return store.Attachment{TenantID: tn.ID, BodyHash: b.BodyHash, Kind: kind, MediaType: "text/markdown", Title: "t", Size: 1,
				ContentHash: strings.Repeat("0", 64), Summary: `{}`, Origin: origin, AttachedBy: "os:1:t", Visibility: store.VisibleAdministrators,
				Supersedes: supersedes}
		}
		insert := func(a *store.Attachment) error {
			return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, a) })
		}
		// the body is held under its name only; a touch refreshes used_at
		inTx(t, s, func(tx *store.Tx) error {
			for name, want := range map[string]bool{"tresor": true, "other": false} {
				if held, err := tx.TouchBuild(ctx, tn.ID, name, b.BodyHash); err != nil || held != want {
					t.Errorf("%s: %v %v", name, held, err)
				}
			}
			return nil
		})

		first := doc(store.KindNotes, store.AttachedByTenant, "")
		if err := insert(&first); err != nil || first.IssuerID != first.ID || first.Version != 1 {
			t.Fatalf("insert: %+v %v", first, err)
		}
		second := doc(store.KindNotes, store.AttachedByTenant, first.ID)
		if err := insert(&second); err != nil {
			t.Fatal(err)
		}
		for name, a := range map[string]store.Attachment{
			"a superseded one again":  doc(store.KindNotes, store.AttachedByTenant, first.ID),
			"another kind":            doc(store.KindSBOM, store.AttachedByTenant, second.ID),
			"another origin":          doc(store.KindNotes, store.AttachedByPublisher, second.ID),
			"one that does not exist": doc(store.KindNotes, store.AttachedByTenant, store.NewID()),
		} {
			if err := insert(&a); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// heads per (body, kind, origin) are limited; superseded versions do not count
		for range store.MaxAttachmentHeads - 1 {
			a := doc(store.KindNotes, store.AttachedByTenant, "")
			if err := insert(&a); err != nil {
				t.Fatal(err)
			}
		}
		if a := doc(store.KindNotes, store.AttachedByTenant, ""); !errors.Is(insert(&a), store.ErrInvalid) {
			t.Fatal("a head beyond the limit")
		}
		if a := doc(store.KindNotes, store.AttachedByPublisher, ""); insert(&a) != nil {
			t.Fatal("another origin has its own limit")
		}
		all, err := s.ExtensionAttachments(ctx, tn.ID, "tresor")
		if err != nil || len(all) != store.MaxAttachmentHeads+2 {
			t.Fatalf("listed %d %v", len(all), err)
		}
		if other, _ := s.ExtensionAttachments(ctx, tn.ID, "other"); len(other) != 0 {
			t.Fatal("another name lists them")
		}

		// flags compare the version
		inTx(t, s, func(tx *store.Tx) error { return tx.SetAttachmentFlags(ctx, &second, store.VisibleReaders, true, 1) })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetAttachmentFlags(ctx, &second, store.VisibleReaders, false, 1) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		got, _ := s.GetAttachment(ctx, tn.ID, second.ID)
		if got.Visibility != store.VisibleReaders || !got.Feedable || got.Version != 2 {
			t.Fatalf("flags: %+v", got)
		}

		// removing a middle version links its successor to its predecessor; the collector takes a
		// version superseded before its cutoff (and links its successor the same way)
		c1 := doc(store.KindProvenance, store.AttachedByTenant, "")
		if err := insert(&c1); err != nil {
			t.Fatal(err)
		}
		c2 := doc(store.KindProvenance, store.AttachedByTenant, c1.ID)
		if err := insert(&c2); err != nil {
			t.Fatal(err)
		}
		c3 := doc(store.KindProvenance, store.AttachedByTenant, c2.ID)
		if err := insert(&c3); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.RemoveAttachment(ctx, c2); return err })
		if g, _ := s.GetAttachment(ctx, tn.ID, c3.ID); g.Supersedes != c1.ID {
			t.Fatalf("the successor was not linked: %+v", g)
		}
		ids := func(as []store.Attachment) map[string]bool {
			m := map[string]bool{}
			for _, a := range as {
				m[a.ID] = true
			}
			return m
		}
		if old, err := s.SupersededBefore(ctx, "default", time.Now().Add(-time.Hour), 100); err != nil || ids(old)[c1.ID] {
			t.Fatalf("superseded an hour ago: %v %v", ids(old), err)
		}
		later := time.Now().Add(time.Hour)
		if old, err := s.SupersededBefore(ctx, "default", later, 100); err != nil || !ids(old)[c1.ID] || ids(old)[c3.ID] {
			t.Fatalf("superseded before an hour from now: %v %v", ids(old), err)
		}
		if ok, err := s.RemoveSuperseded(ctx, c1, later); err != nil || !ok {
			t.Fatalf("remove superseded: %v %v", ok, err)
		}
		if g, _ := s.GetAttachment(ctx, tn.ID, c3.ID); g.Supersedes != "" {
			t.Fatalf("the head still names a removed version: %+v", g)
		}
		if ok, err := s.RemoveSuperseded(ctx, c3, later); err != nil || ok {
			t.Fatalf("a head removed as superseded: %v %v", ok, err)
		}

		// the publisher check: a publication's release by that actor
		r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: b.PackageVersion, Platform: b.Platform,
			Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 1, Origin: store.OriginPublication}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r, "publisher:acme/ci") })
		inTx(t, s, func(tx *store.Tx) error {
			for actor, want := range map[string]bool{"publisher:acme/ci": true, "publisher:acme/other": false} {
				if ok, err := tx.PublishedBy(ctx, tn.ID, "tresor", b.BodyHash, actor); err != nil || ok != want {
					t.Errorf("%s: %v %v", actor, ok, err)
				}
			}
			return nil
		})

		// removal: the tenant's go, a feed's leave a tombstone
		feed := doc(store.KindReview, store.AttachedByFeed, "")
		feed.Issuer, feed.IssuerID = "https://source.example/api/v1/tenants/x", "r1"
		if err := insert(&feed); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.RemoveAttachment(ctx, feed); return err })
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.RemoveAttachment(ctx, first); return err })
		if g, err := s.GetAttachment(ctx, tn.ID, feed.ID); err != nil || g.RemovedAt.IsZero() {
			t.Fatalf("no tombstone: %+v %v", g, err)
		}
		if _, err := s.GetAttachment(ctx, tn.ID, first.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a tenant's document kept: %v", err)
		}
		if again := feed; errors.Is(insert(&again), store.ErrExists) == false {
			t.Fatal("the feed's document came back over its tombstone")
		}

		// the collector: nothing while a Build holds the body; everything once it is gone, unless blocked
		if gone, _ := s.AttachmentsOfGoneBodies(ctx, "default", 100); len(gone) != 0 {
			t.Fatalf("gone while held: %d", len(gone))
		}
		if err := store.ExecRaw(ctx, s, "DELETE FROM releases WHERE build_id = ?", b.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.ExecRaw(ctx, s, "DELETE FROM builds WHERE id = ?", b.ID); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: b.BodyHash, Reason: "cve", CreatedBy: "os:1:t"})
		})
		if gone, _ := s.AttachmentsOfGoneBodies(ctx, "default", 100); len(gone) != 0 {
			t.Fatalf("a blocked body's documents gone: %d", len(gone))
		}
		if err := store.ExecRaw(ctx, s, "DELETE FROM blocks WHERE body_hash = ?", b.BodyHash); err != nil {
			t.Fatal(err)
		}
		gone, err := s.AttachmentsOfGoneBodies(ctx, "default", 100)
		if err != nil || len(gone) == 0 {
			t.Fatalf("gone: %d %v", len(gone), err)
		}
		if ok, err := s.DeleteAttachmentRecord(ctx, gone[0]); err != nil || !ok {
			t.Fatalf("delete: %v %v", ok, err)
		}
		have, err := s.AttachmentRecordsExist(ctx, []string{gone[0].ID, gone[1].ID, store.NewID()})
		if err != nil || len(have) != 1 || !have[gone[1].ID] {
			t.Fatalf("records exist: %v %v", have, err)
		}
	})
}
