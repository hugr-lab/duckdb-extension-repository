package store_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func inTx(t *testing.T, s *store.Store, fn func(tx *store.Tx) error) {
	t.Helper()
	if err := s.InTx(ctx, "", fn); err != nil {
		t.Fatal(err)
	}
}

func cppBuild(tn store.Tenant, name, ver, hash string) store.Build {
	return store.Build{TenantID: tn.ID, Name: name, ExtVersion: ver, Platform: "linux_amd64", ABI: store.ABICPP,
		DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat(hash, 64), Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
}

func TestBuildsAndReleases(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		k := addKey(t, s, ch, "sha256:"+strings.Repeat("a", 64), store.KeyActive)

		b := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		again := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &again) })
		if again.ID != b.ID {
			t.Fatal("the same body under the same name made a second build")
		}
		cb := store.Build{TenantID: tn.ID, Name: "demo", ExtVersion: "1.0", Platform: "linux_amd64", ABI: store.ABICStruct,
			CAPI: &store.CAPI{Major: 1, Minor: 2, Patch: 0}, BodyHash: strings.Repeat("c", 64), Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &cb) })
		if cb.Slot() != "capi:1" || b.Slot() != "v2.0.0" {
			t.Fatalf("slots %q %q", cb.Slot(), b.Slot())
		}
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetBuild(ctx, cb.ID)
			if err != nil || got.CAPI == nil || *got.CAPI != *cb.CAPI || got.DuckDBVersion != "" {
				t.Errorf("c_struct build round trip: %+v %v", got, err)
			}
			return nil
		})

		rel := func(bd store.Build, seq int64, vis string) store.Release {
			return store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: bd.ID, Name: bd.Name, ExtVersion: bd.ExtVersion,
				Platform: bd.Platform, Slot: bd.Slot(), State: store.ReleaseActive, Visibility: vis, Seq: seq}
		}
		r1 := rel(b, 1, store.Public)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r1, "os:1:t") })
		dup := rel(b, 2, store.Public)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertRelease(ctx, &dup, "os:1:t") }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("a second release in one slot: %v", err)
		}
		// seq is unique per channel; releases without one are many
		cr := rel(cb, 1, store.Private)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertRelease(ctx, &cr, "os:1:t") }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("a duplicate seq: %v", err)
		}
		cr.Seq = 0
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &cr, "os:1:t") })
		b2 := cppBuild(tn, "tresor", "1.1", "b")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b2) })
		r2 := rel(b2, 0, store.Private)
		inTx(t, s, func(tx *store.Tx) error {
			n, err := tx.NextSeq(ctx, ch.ID)
			if err != nil || n != 2 {
				t.Errorf("next seq %d %v", n, err)
			}
			r2.Seq = n
			return tx.InsertRelease(ctx, &r2, "os:1:t")
		})

		// the channel's releases with their builds, every state and visibility
		all, err := s.ChannelReleases(ctx, ch.ID)
		if err != nil || len(all) != 3 {
			t.Fatalf("channel releases: %+v %v", all, err)
		}
		byID := map[string]store.Candidate{}
		for _, c := range all {
			byID[c.ID] = c
		}
		if c := byID[r2.ID]; c.BodyHash != b2.BodyHash || c.DuckDBVersion != "v2.0.0" || c.Seq != 2 || c.Visibility != store.Private {
			t.Fatalf("release with its build: %+v", c)
		}
		if c := byID[cr.ID]; c.CAPI == nil || c.ABI != store.ABICStruct || c.Seq != 0 {
			t.Fatalf("c_struct release: %+v", c)
		}

		// state, seq, visibility changes are compare-and-set
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r2, store.ReleaseYanked, "os:1:t") })
		stale := r2
		stale.Version--
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetReleaseVisibility(ctx, &stale, store.Public, "x") }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale visibility change: %v", err)
		}
		for _, c := range must(s.ChannelReleases(ctx, ch.ID)) {
			if c.ID == r2.ID && c.State != store.ReleaseYanked {
				t.Fatal("the yank is not stored")
			}
		}

		// signatures: 256 bytes, one per (release, key), only by the release's channel's keys
		sig := bytes.Repeat([]byte{7}, 256)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertSignature(ctx, r1.ID, ch.ID, k.ID, sig) })
		inTx(t, s, func(tx *store.Tx) error {
			return tx.InsertSignature(ctx, r1.ID, ch.ID, k.ID, bytes.Repeat([]byte{8}, 256))
		})
		if got, err := s.Signature(ctx, r1.ID, k.ID); err != nil || !bytes.Equal(got, sig) {
			t.Fatalf("signature: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertSignature(ctx, r1.ID, ch.ID, k.ID, sig[:10]) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("short signature: %v", err)
		}
		other := store.Channel{TenantID: tn.ID, Name: "staging", Kind: store.ChannelSigned}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &other) })
		ok2 := addKey(t, s, other, "sha256:"+strings.Repeat("b", 64), store.KeyActive)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertSignature(ctx, r1.ID, ch.ID, ok2.ID, sig) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a signature by another channel's key: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetServingKey(ctx, ch.ID, "", ok2.ID) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("another channel's key as the serving key: %v", err)
		}
		unsigned, hashes, err := s.UnsignedReleases(ctx, ch.ID, k.ID, 0)
		if err != nil || len(unsigned) != 1 || unsigned[0].ID != cr.ID || hashes[0] != cb.BodyHash {
			t.Fatalf("unsigned: %+v %v", unsigned, err)
		}

		// the serving key and release_version
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.SetServingKey(ctx, ch.ID, "", k.ID); err != nil {
				return err
			}
			return tx.BumpReleaseVersion(ctx, ch.ID)
		})
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetServingKey(ctx, ch.ID, "", k.ID) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("serving key set twice from empty: %v", err)
		}
		sc, err := s.GetServeChannel(ctx, "acme", "prod")
		if err != nil || sc.Channel.ServingKeyID != k.ID || sc.Channel.ReleaseVersion != 1 || sc.Tenant.StorageDomain != "default" {
			t.Fatalf("serve channel: %+v %v", sc, err)
		}
		if list, _ := s.ListReleases(ctx, ch.ID, "tresor"); len(list) != 2 {
			t.Fatalf("list: %d", len(list))
		}
	})
}

func TestCAPIs(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		_, ch := fixture(t, s)
		var v1, v2 store.DuckDBVersion
		inTx(t, s, func(tx *store.Tx) error {
			v1 = store.DuckDBVersion{Name: "v2.0.0", Kind: "release"}
			if err := tx.AddDuckDBVersion(ctx, &v1); err != nil {
				return err
			}
			v2 = store.DuckDBVersion{Name: "eb0d9df48e", Kind: "dev", CAPIVersion: "v1.5.6"} // legacy column only
			if err := tx.AddDuckDBVersion(ctx, &v2); err != nil {
				return err
			}
			for _, c := range []store.CAPI{{1, 5, 6}, {2, 0, 0}} {
				if err := tx.AddCAPI(ctx, v1.ID, c); err != nil {
					return err
				}
			}
			if err := tx.AddChannelVersion(ctx, ch.ID, v1.ID); err != nil {
				return err
			}
			return tx.AddChannelVersion(ctx, ch.ID, v2.ID)
		})
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.AddCAPI(ctx, v1.ID, store.CAPI{1, 9, 9}) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("changing a major: %v", err)
		}
		// a store-level add of another major to the legacy version keeps reading the legacy value
		// only while there are no rows: tenants.AddVersionCAPI turns it into a row first
		m, err := s.ChannelCAPIs(ctx, ch.ID)
		if err != nil || len(m["v2.0.0"]) != 2 || len(m["eb0d9df48e"]) != 1 || m["eb0d9df48e"][0] != (store.CAPI{1, 5, 6}) {
			t.Fatalf("channel C APIs: %+v %v", m, err)
		}
	})
}

func TestCAPIAccepts(t *testing.T) {
	max := store.CAPI{1, 5, 6}
	for b, want := range map[store.CAPI]bool{{1, 4, 9}: true, {1, 5, 6}: true, {1, 5, 7}: false, {1, 6, 0}: false, {2, 0, 0}: false, {1, 0, 0}: true} {
		if max.Accepts(b) != want {
			t.Errorf("%v under %v: want %v", b, max, want)
		}
	}
	for in, ok := range map[string]bool{"v1.2.3": true, "1.2.3": false, "v1.2": false, "v1.2.3-rc": false, "v01.2.3": true} {
		if _, err := store.ParseCAPI(in); (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestLeases(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		var wg sync.WaitGroup
		got := make([]bool, 6)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.AcquireLease(ctx, "resign/x", "h"+string(rune('a'+i)), time.Minute)
				if err != nil {
					t.Error(err)
				}
				got[i] = ok
			}()
		}
		wg.Wait()
		n, holder := 0, ""
		for i, ok := range got {
			if ok {
				n++
				holder = "h" + string(rune('a'+i))
			}
		}
		if n != 1 {
			t.Fatalf("%d holders", n)
		}
		if ok, _ := s.AcquireLease(ctx, "resign/x", holder, time.Minute); !ok {
			t.Fatal("the holder cannot renew")
		}
		if err := s.ReleaseLease(ctx, "resign/x", holder); err != nil {
			t.Fatal(err)
		}
		if h, until, found, err := s.LeaseState(ctx, "resign/x"); err != nil || !found || h != holder || until.After(time.Now()) {
			t.Fatalf("a released lease: %s %v %v %v", h, until, found, err)
		}
		if _, _, found, err := s.LeaseState(ctx, "resign/none"); err != nil || found {
			t.Fatalf("a lease never taken: %v %v", found, err)
		}
		if ok, _ := s.AcquireLease(ctx, "resign/x", "other", time.Millisecond); !ok {
			t.Fatal("a released lease is not free")
		}
		time.Sleep(5 * time.Millisecond)
		if ok, _ := s.AcquireLease(ctx, "resign/x", "third", time.Minute); !ok {
			t.Fatal("an expired lease is not free")
		}
	})
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
