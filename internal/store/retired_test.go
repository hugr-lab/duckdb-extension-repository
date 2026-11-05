package store_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0053 (spec 0042) on a populated database: the releases' state CHECK widened to retired
// (SQLite rebuilds the table, keeping its rows, signatures and indexes), retired_from and retired_at
// added, and min_reader raised: an older replica neither knows the state nor reaps retention_pins.
func TestMigrationRetired(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 52); err != nil {
			t.Fatal(err)
		}
		tn, ch := fixture(t, s)
		k := addKey(t, s, ch, "sha256:"+strings.Repeat("a", 64), store.KeyActive)
		var rels []store.Release
		for i, v := range []string{"1.0", "1.1"} {
			b := cppBuild(tn, "tresor", v, string(rune('a'+i)))
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: v, Platform: b.Platform,
				Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: int64(i + 1)}
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.InsertRelease(ctx, &r, "os:1:t"); err != nil {
					return err
				}
				return tx.InsertSignature(ctx, r.ID, ch.ID, k.ID, bytes.Repeat([]byte{byte(i + 1)}, 256))
			})
			rels = append(rels, r)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &rels[0], store.ReleaseYanked, "os:1:t") })
		if err := store.ExecRaw(ctx, s, "UPDATE releases SET state = 'retired' WHERE id = ?", rels[1].ID); err == nil {
			t.Fatal("retired before the migration")
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 53 AND min_reader = 53"); err != nil || n != 1 {
			t.Fatalf("migration 53 raises min_reader: %d %v", n, err)
		}
		got, err := s.ChannelReleases(ctx, ch.ID)
		if err != nil || len(got) != 2 || got[0].ID != rels[0].ID || got[0].State != store.ReleaseYanked || got[1].Seq != 2 ||
			got[1].RetiredFrom != "" || !got[1].RetiredAt.IsZero() {
			t.Fatalf("the releases after: %+v %v", got, err)
		}
		for i, r := range rels {
			if sig, err := s.Signature(ctx, r.ID, k.ID); err != nil || !bytes.Equal(sig, bytes.Repeat([]byte{byte(i + 1)}, 256)) {
				t.Fatalf("the signature of %s: %v", r.ID, err)
			}
		}
		// the state takes retired, and nothing else new
		if err := store.ExecRaw(ctx, s, "UPDATE releases SET state = 'gone' WHERE id = ?", rels[1].ID); err == nil {
			t.Fatal("an unknown state stored")
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.RetireRelease(ctx, &rels[1], "system:release-retention") })
		// the indexes and the foreign keys are kept: a signature of a release that is not there is refused
		var q string
		switch e.Name {
		case "postgres":
			q = "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'releases'"
		case "sqlserver":
			q = "SELECT name FROM sys.indexes WHERE object_id = OBJECT_ID('releases') AND name IS NOT NULL"
		default:
			q = "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'releases'"
		}
		var idx []string
		inTx(t, s, func(tx *store.Tx) (err error) {
			idx, err = store.QueryStrings(ctx, tx, q)
			return err
		})
		for _, n := range []string{"ux_releases_seq", "ix_releases_resolve", "ix_releases_build"} {
			if !slices.Contains(idx, n) {
				t.Errorf("index %s lost: %v", n, idx)
			}
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertSignature(ctx, store.NewID(), ch.ID, k.ID, bytes.Repeat([]byte{9}, 256))
		}); err == nil {
			t.Fatal("a signature of no release")
		}
		dup := rels[1]
		dup.Seq = 3
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertRelease(ctx, &dup, "os:1:t") }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the slot's unique key: %v", err)
		}
	})
}

// Spec 0042: a release retired keeps the state it came from and when; a restore returns it there,
// keeping its seq, both compare-and-set; it is in the channel's releases (the index shows it), and the
// re-signer counts it; an upstream's yank of it and the lift return it to retired with a new clock.
func TestRetiredRelease(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return at }
		tn, ch := fixture(t, s)
		k := addKey(t, s, ch, "sha256:"+strings.Repeat("a", 64), store.KeyActive)
		b := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: "1.0", Platform: b.Platform,
			Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 1}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r, "os:1:t") })
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r, store.ReleaseDeprecated, "os:1:t") })
		stale := r
		inTx(t, s, func(tx *store.Tx) error { return tx.RetireRelease(ctx, &r, "system:release-retention") })
		if r.State != store.ReleaseRetired || r.RetiredFrom != store.ReleaseDeprecated || !r.RetiredAt.Equal(at) {
			t.Fatalf("retired: %+v", r)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.RetireRelease(ctx, &stale, "x") }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale retirement: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { cp := r; return tx.RetireRelease(ctx, &cp, "x") }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a retired release retired again: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetRelease(ctx, ch.ID, r.ID)
			if err != nil || got.State != store.ReleaseRetired || got.RetiredFrom != store.ReleaseDeprecated || !got.RetiredAt.Equal(at) || got.Seq != 1 {
				t.Errorf("read back: %+v %v", got, err)
			}
			return nil
		})
		// served nowhere, but in the channel's releases, and still signed by every new key
		if all, err := s.ChannelReleases(ctx, ch.ID); err != nil || len(all) != 1 || all[0].State != store.ReleaseRetired {
			t.Fatalf("channel releases: %+v %v", all, err)
		}
		if rev, err := s.RevokedReleases(ctx, ch.ID); err != nil || len(rev) != 0 {
			t.Fatalf("a retired release is not revoked: %+v %v", rev, err)
		}
		k2 := addKey(t, s, ch, "sha256:"+strings.Repeat("b", 64), store.KeyTrusted)
		if n, err := s.CountUnsigned(ctx, ch.ID, k2.ID); err != nil || n != 1 {
			t.Fatalf("the re-signer counts it: %d %v", n, err)
		}
		if us, _, err := s.UnsignedReleases(ctx, ch.ID, k.ID, 0); err != nil || len(us) != 1 || us[0].ID != r.ID {
			t.Fatalf("the re-signer signs it: %+v %v", us, err)
		}

		// an upstream's yank of it, then the lift: retired again, retired_from kept, a new grace
		at = at.Add(time.Hour)
		prior := r.State
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r, store.ReleaseYanked, "system:upstream") })
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.ReleaseByID(ctx, r.ID)
			if err != nil || got.State != store.ReleaseYanked || got.RetiredFrom != store.ReleaseDeprecated {
				t.Errorf("yanked: %+v %v", got, err)
			}
			at = at.Add(24 * time.Hour)
			return tx.SetReleaseState(ctx, &got.Release, prior, "os:1:t")
		})
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetRelease(ctx, ch.ID, r.ID)
			if err != nil || got.State != store.ReleaseRetired || got.RetiredFrom != store.ReleaseDeprecated || !got.RetiredAt.Equal(at) {
				t.Errorf("lifted: %+v %v", got, err)
			}
			r = got
			return nil
		})

		// a restore: back to deprecated, seq kept, the columns cleared; a stale one refused
		stale = r
		inTx(t, s, func(tx *store.Tx) error { return tx.RestoreRelease(ctx, &r, "os:1:t") })
		if r.State != store.ReleaseDeprecated || r.RetiredFrom != "" || !r.RetiredAt.IsZero() || r.Seq != 1 {
			t.Fatalf("restored: %+v", r)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.RestoreRelease(ctx, &stale, "x") }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale restore: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { cp := r; return tx.RestoreRelease(ctx, &cp, "x") }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a release not retired restored: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetRelease(ctx, ch.ID, r.ID)
			if err != nil || got.State != store.ReleaseDeprecated || got.RetiredFrom != "" || !got.RetiredAt.IsZero() {
				t.Errorf("read back: %+v %v", got, err)
			}
			return nil
		})
		// a release never retired is not set retired by a state change
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r, store.ReleaseRetired, "x") }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("set retired without a retirement: %v", err)
		}
	})
}
