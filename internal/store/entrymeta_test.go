package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0044 (spec 0036 phase 1) on a level-43 database: the tables arrive empty, and the level
// an older replica needs stays 43.
func TestMigrationEntryMeta(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 43); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"entry_meta", "icon_claims", "icon_tombstones"} {
			if n, err := store.CountRaw(ctx, s, table, "1 = 1"); err != nil || n != 0 {
				t.Fatalf("%s: %d %v", table, n, err)
			}
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 44 AND min_reader = 43"); err != nil || n != 1 {
			t.Fatalf("migration 0044 needs no newer reader: %d %v", n, err)
		}
	})
}

// An entry's metadata: written, replaced and cleared; read alone and by type; its icon's bytes
// counting toward its source's usage, with or without builds.
func TestEntryMeta(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		const typ, src = "duckdb_extension", "publisher:p1"
		if _, ok, err := s.EntryMeta(ctx, tn.ID, typ, src, "geo"); err != nil || ok {
			t.Fatalf("none yet: %v %v", ok, err)
		}
		m := store.EntryMeta{TenantID: tn.ID, PackageType: typ, Source: src, Name: "geo", Summary: "Geometry \u2014 f\u00FCr alle \U0001F30D",
			IconSHA256: strings.Repeat("a", 64), IconWidth: 128, IconHeight: 128, IconBytes: 4096, SetBy: "os:1:t", SetAt: now,
			SignedAt: now.Add(-time.Minute), SignedBody: strings.Repeat("b", 64)}
		inTx(t, s, func(tx *store.Tx) error { return tx.PutEntryMeta(ctx, m) })
		got, ok, err := s.EntryMeta(ctx, tn.ID, typ, src, "geo")
		if err != nil || !ok || got != m {
			t.Fatalf("read back: %+v %v %v", got, ok, err)
		}
		// a summary of 160 code points beyond the Basic Multilingual Plane fits every dialect
		m.Summary = strings.Repeat("\U0001F30D", 160)
		inTx(t, s, func(tx *store.Tx) error { return tx.PutEntryMeta(ctx, m) })
		if got, _, _ := s.EntryMeta(ctx, tn.ID, typ, src, "geo"); got.Summary != m.Summary {
			t.Fatalf("160 astral characters: %d", len([]rune(got.Summary)))
		}
		if _, n, err := s.SourceUsage(ctx, tn.ID, src); err != nil || n != 4096 {
			t.Fatalf("the icon counts without builds: %d %v", n, err)
		}
		// replaced: cleared, a token's write
		cleared := store.EntryMeta{TenantID: tn.ID, PackageType: typ, Source: src, Name: "geo", SetBy: "server:x", SetAt: now.Add(time.Hour),
			SignedAt: now.Add(time.Hour)}
		var locked store.EntryMeta
		inTx(t, s, func(tx *store.Tx) error {
			var ok bool
			var err error
			if locked, ok, err = tx.EntryMeta(ctx, tn.ID, typ, src, "geo"); err != nil || !ok {
				return errors.Join(err, errors.New("no row"))
			}
			return tx.PutEntryMeta(ctx, cleared)
		})
		if locked.IconSHA256 != m.IconSHA256 {
			t.Fatalf("the locked read: %+v", locked)
		}
		if got, ok, err := s.EntryMeta(ctx, tn.ID, typ, src, "geo"); err != nil || !ok || got != cleared {
			t.Fatalf("cleared: %+v %v", got, err)
		}
		if _, n, _ := s.SourceUsage(ctx, tn.ID, src); n != 0 {
			t.Fatalf("a cleared icon counts no more: %d", n)
		}
		// by type: another type's and another source's apart
		other := m
		other.PackageType, other.Name = "adbc_driver", "pg"
		inTx(t, s, func(tx *store.Tx) error { return tx.PutEntryMeta(ctx, other) })
		ms, err := s.EntryMetas(ctx, tn.ID, typ)
		if err != nil || len(ms) != 1 || ms[[2]string{src, "geo"}] != cleared {
			t.Fatalf("by type: %+v %v", ms, err)
		}
		// an entry exists by its builds, of its type, held, available or withdrawn
		if ok, err := s.EntryExists(ctx, tn.ID, typ, "tenant", "tresor"); err != nil || ok {
			t.Fatalf("no build: %v %v", ok, err)
		}
		b := cppBuild(tn, "tresor", "1.0", "c")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		if ok, err := s.EntryExists(ctx, tn.ID, typ, "tenant", "tresor"); err != nil || !ok {
			t.Fatalf("a build: %v %v", ok, err)
		}
		if ok, _ := s.EntryExists(ctx, tn.ID, "adbc_driver", "tenant", "tresor"); ok {
			t.Fatal("another type's entry")
		}
	})
}

// Icons follow spec 0016's protocol in tables of their own: a write claims, the row's transaction
// commits the claim; the collector marks an icon no row of the domain's tenants references and no
// live claim holds, and deletes it a grace later.
func TestIconObjects(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		icons := s.Icons()
		used, gone := strings.Repeat("1", 64), strings.Repeat("2", 64)
		// a write: claimed, uploaded, committed with its row
		noErr(t, icons.Claim(ctx, "default", used, "c1", now.Add(time.Hour)))
		noErr(t, s.InTxLocks(ctx, []string{store.PublisherAuthLock(tn.ID), store.IconLock("default", used)}, func(tx *store.Tx) error {
			if err := tx.CommitIcon(ctx, "default", used, "c1"); err != nil {
				return err
			}
			return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: "tenant", Name: "acl",
				IconSHA256: used, IconWidth: 64, IconHeight: 64, IconBytes: 10, SetBy: "os:1:t", SetAt: now})
		}))
		if n, _ := store.CountRaw(ctx, s, "icon_claims", "1 = 1"); n != 0 {
			t.Fatalf("the claim stays: %d", n)
		}
		// a commit whose claim was lost
		noErr(t, icons.Claim(ctx, "default", gone, "c2", now.Add(time.Minute)))
		now = now.Add(2 * time.Minute)
		if err := s.InTx(ctx, store.IconLock("default", gone), func(tx *store.Tx) error { return tx.CommitIcon(ctx, "default", gone, "c2") }); !errors.Is(err, store.ErrClaimLost) {
			t.Fatalf("an expired claim: %v", err)
		}
		// the listing: the referenced one is not marked; the other, once its claim expired, is
		un, err := icons.Unreferenced(ctx, "default", []string{used, gone})
		if err != nil || len(un) != 1 || un[0] != gone {
			t.Fatalf("unreferenced: %v %v", un, err)
		}
		if un, _ := icons.Unreferenced(ctx, "other", []string{used}); len(un) != 1 {
			t.Fatalf("another domain's tenants reference nothing here: %v", un)
		}
		if ok, err := icons.Mark(ctx, "default", used); err != nil || ok {
			t.Fatalf("a referenced icon marked: %v %v", ok, err)
		}
		noErr(t, icons.Claim(ctx, "default", gone, "c3", now.Add(time.Hour)))
		if ok, err := icons.Mark(ctx, "default", gone); err != nil || ok {
			t.Fatalf("a claimed icon marked: %v %v", ok, err)
		}
		noErr(t, icons.ReleaseClaim(ctx, "default", gone, "c3"))
		if ok, err := icons.Mark(ctx, "default", gone); err != nil || !ok {
			t.Fatalf("an unreferenced icon not marked: %v %v", ok, err)
		}
		if n, err := icons.RemoveExpiredClaims(ctx, "default"); err != nil || n != 1 {
			t.Fatalf("expired claims: %d %v", n, err)
		}
		// the streams' tables never see an icon
		if ts, _ := s.DueTombstones(ctx, "default", now.Add(time.Hour), now, "", 10); len(ts) != 0 {
			t.Fatalf("a stream tombstone: %+v", ts)
		}
		// a write waits while the collector deletes it
		grace := now.Add(time.Hour)
		now = now.Add(2 * time.Hour)
		ts, err := icons.DueTombstones(ctx, "default", grace, now.Add(-30*time.Minute), "", 10)
		if err != nil || len(ts) != 1 || ts[0].StreamHash != gone {
			t.Fatalf("due: %+v %v", ts, err)
		}
		stamp, err := icons.BeginDelete(ctx, "default", gone, grace, now.Add(-30*time.Minute))
		if err != nil || stamp.IsZero() {
			t.Fatalf("begin: %v %v", stamp, err)
		}
		if err := icons.Claim(ctx, "default", gone, "c4", now.Add(time.Hour)); !errors.Is(err, store.ErrStreamBusy) {
			t.Fatalf("a write during the delete: %v", err)
		}
		if ok, err := icons.StillDeleting(ctx, "default", gone, stamp); err != nil || !ok {
			t.Fatalf("still deleting: %v %v", ok, err)
		}
		// a delete of unknown outcome: stale, taken again by a resolver, finished
		now = now.Add(time.Hour)
		stamp2, err := icons.BeginDelete(ctx, "default", gone, time.Time{}, now.Add(-30*time.Minute))
		if err != nil || stamp2.IsZero() {
			t.Fatalf("taken again: %v %v", stamp2, err)
		}
		if ok, _ := icons.StillDeleting(ctx, "default", gone, stamp); ok {
			t.Fatal("the first delete is still its collector's")
		}
		noErr(t, icons.EndDelete(ctx, "default", gone, stamp2))
		noErr(t, icons.Claim(ctx, "default", gone, "c5", now.Add(time.Hour)))
		// a store refusing the delete: back to marked
		noErr(t, icons.ReleaseClaim(ctx, "default", gone, "c5"))
		if ok, _ := icons.Mark(ctx, "default", gone); !ok {
			t.Fatal("marked again")
		}
		now = now.Add(3 * time.Hour)
		stamp3, _ := icons.BeginDelete(ctx, "default", gone, now.Add(-time.Hour), now.Add(-30*time.Minute))
		noErr(t, icons.AbortDelete(ctx, "default", gone, stamp3))
		if n, _ := store.CountRaw(ctx, s, "icon_tombstones", "state = 'marked'"); n != 1 {
			t.Fatalf("aborted: %d", n)
		}
	})
}
