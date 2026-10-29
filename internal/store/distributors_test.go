package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0041 (spec 0033 phase B part 1): on a populated database at level 40, every publisher is
// the tenant's own and every build is not withdrawn, so every channel serves what it served; a
// publisher marked a distributor's afterwards (by the column: spec 0037 marks none) is remembered once
// removed.
func TestMigrationDistributors(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 40); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		const tid = "t0000000-0000-0000-0000-000000000000"
		cid, src := id("c"), "publisher:"+id("p")
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, now},
			{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by, hold_seconds) VALUES (?, ?, 'ci', ?, 'os:1:t', 0)", id("p"), tid, now},
			{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked) VALUES (?, ?, ?, 'acl', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'publication', ?, 'os:1:t', ?)",
				id("b"), tid, src, strings.Repeat("1", 64), now, true},
			{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state, visibility, seq, created_at, " +
				"created_by, state_changed_at, state_changed_by, version, origin, source) VALUES (?, ?, ?, ?, 'acl', '1.0', 'linux_amd64', 'v2.0.0', " +
				"'active', 'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'publication', ?)", id("r"), tid, cid, id("b"), now, now, src},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if p, err := tx.GetPublisher(ctx, tid, "ci"); err != nil || p.Distributor != "" {
				t.Errorf("the publisher: %+v %v", p, err)
			}
			if b, err := tx.GetBuild(ctx, id("b")); err != nil || b.Withdrawn() || b.WithdrawnReason != "" {
				t.Errorf("the build: %+v %v", b, err)
			}
			return nil
		})
		if rs, err := s.ChannelReleases(ctx, cid); err != nil || len(rs) != 1 || rs[0].Withdrawn {
			t.Fatalf("the channel serves what it served: %+v %v", rs, err)
		}
		if d, err := s.SourceDistributor(ctx, tid, src); err != nil || d != "" {
			t.Fatalf("the source's distributor: %q %v", d, err)
		}
		// marked afterwards, then removed: remembered
		inTx(t, s, func(tx *store.Tx) error {
			if _, err := tx.SetPublisherDistributor(ctx, tid, "ci", "line\nbreak"); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("a distributor's name with a line break: %v", err)
			}
			// spec 0037: an existing publisher is never marked; the store renames a marked one only
			if _, err := tx.SetPublisherDistributor(ctx, tid, "ci", "enterest"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an unmarked publisher marked: %v", err)
			}
			return nil
		})
		if err := store.ExecRaw(ctx, s, "UPDATE publishers SET distributor = 'enterest-old' WHERE id = ?", id("p")); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetPublisherDistributor(ctx, tid, "ci", "enterest")
			if err == nil && before.Distributor != "enterest-old" {
				t.Errorf("before: %+v", before)
			}
			return err
		})
		if srcs, err := s.DistributorSources(ctx, tid); err != nil || srcs[src].Label != "publisher:ci" || srcs[src].PublisherID != id("p") {
			t.Fatalf("the distributors' sources: %v %v", srcs, err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tid, "ci", "os:1:t") })
		if d, err := s.SourceDistributor(ctx, tid, src); err != nil || d != "enterest" {
			t.Fatalf("a removed distributor's publisher: %q %v", d, err)
		}
		if d, err := s.SourceDistributor(ctx, id("x"), src); err != nil || d != "" {
			t.Fatalf("another tenant's: %q %v", d, err)
		}
		if got, err := s.ResolveDistributorSource(ctx, tid, "publisher:ci"); err != nil || got != src {
			t.Fatalf("resolved after its removal: %q %v", got, err)
		}
		if _, err := s.ResolveDistributorSource(ctx, tid, "publisher:nope"); !errors.Is(err, store.ErrNoSource) {
			t.Fatalf("an unknown one: %v", err)
		}
		// another distributor's publisher of the same name: the name is ambiguous, each is named by its id
		again := store.Publisher{TenantID: tid, Name: "ci", CreatedBy: "os:1:t", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &again) })
		if _, err := s.ResolveDistributorSource(ctx, tid, "publisher:ci"); !errors.Is(err, store.ErrAmbiguousSource) {
			t.Fatalf("an ambiguous name: %v", err)
		}
		for _, pid := range []string{id("p"), again.ID} {
			if got, err := s.ResolveDistributorSource(ctx, tid, "publisher-id:"+pid); err != nil || got != "publisher:"+pid {
				t.Fatalf("by its id: %q %v", got, err)
			}
		}
		srcs, err := s.DistributorSources(ctx, tid)
		if err != nil || len(srcs) != 2 || srcs[src].Label != "publisher-id:"+id("p") || !srcs[src].Removed ||
			srcs["publisher:"+again.ID].Label != "publisher-id:"+again.ID {
			t.Fatalf("the labels of two of one name: %+v %v", srcs, err)
		}
		if _, err := s.ResolveDistributorSource(ctx, tid, "publisher-id:"+id("x")); !errors.Is(err, store.ErrNoSource) {
			t.Fatalf("an unknown id: %v", err)
		}
	})
}

// Spec 0033 phase B: withdrawing a version marks its entries withdrawn and bumps every channel
// holding a release of them; serving leaves those releases out, listings show them withdrawn; the
// body never arrives again in the source, the entry is never purged nor collected, its key records
// count for nothing, a purge of held entries leaves it, and the catalogue's backlog does not count it.
func TestWithdraw(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		other := store.Channel{TenantID: tn.ID, Name: "dev", Kind: store.ChannelSigned}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &other) })
		p := store.Publisher{TenantID: tn.ID, Name: "enterest-ci", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetDistributorHold(ctx, tn.ID, 0) // spec 0037: its entries not held by the tenant's minimum
			return err
		})
		src := store.PublisherSource(p.ID)
		build := func(ver, hash, platform string) store.Build {
			b := cppBuild(tn, "geo", ver, hash)
			b.Source, b.Platform, b.Origin = src, platform, store.OriginPublication
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		amd, arm, keep := build("1.0", "a", "linux_amd64"), build("1.0", "b", "linux_arm64"), build("1.1", "c", "linux_amd64")
		release := func(c store.Channel, b store.Build) {
			inTx(t, s, func(tx *store.Tx) error {
				r := store.Release{TenantID: tn.ID, ChannelID: c.ID, BuildID: b.ID, Name: "geo", Source: src, PackageVersion: b.PackageVersion,
					Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPlacement}
				return tx.InsertRelease(ctx, &r, "os:1:t")
			})
		}
		release(ch, amd)
		release(ch, keep)
		version := func(c store.Channel) int64 {
			got, err := s.GetChannel(ctx, "acme", c.Name)
			if err != nil {
				t.Fatal(err)
			}
			return got.ReleaseVersion
		}
		prodBefore, devBefore := version(ch), version(other)
		withdraw := func(ver, reason string) (w store.Withdrawn, already bool, err error) {
			err = s.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				w, already, err = tx.Withdraw(ctx, tn.ID, "duckdb_extension", src, "geo", ver, reason, "os:1:t")
				return err
			})
			return w, already, err
		}
		for _, reason := range []string{"", strings.Repeat("x", store.MaxWithdrawReason+1), "a\x00b"} {
			if _, _, err := withdraw("1.0", reason); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("reason %q: %v", reason, err)
			}
		}
		for _, ver := range []string{"", "a b", "1.0\x00", "../1", strings.Repeat("1", 65)} {
			if _, _, err := withdraw(ver, "CVE"); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("version %q: %v", ver, err)
			}
		}
		// a version the source does not have (yet): withdrawn all the same, and none of it arrives
		if w, already, err := withdraw("9.9", "CVE"); err != nil || already || len(w.Builds) != 0 {
			t.Fatalf("a version the source does not have: %+v %v %v", w, already, err)
		}
		if _, already, err := withdraw("9.9", "again"); err != nil || !already {
			t.Fatalf("again: %v %v", already, err)
		}
		late := cppBuild(tn, "geo", "9.9", "f")
		late.Source, late.Origin = src, store.OriginPublication
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &late) }); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("a build of a version withdrawn before it arrived: %v", err)
		}
		w, already, err := withdraw("1.0", "CVE-2026-0001")
		if err != nil || already || len(w.Builds) != 2 || len(w.Channels) != 1 || w.Channels[0] != ch.ID ||
			!w.Builds[0].WithdrawnAt.Equal(clock) || w.Builds[0].WithdrawnReason != "CVE-2026-0001" {
			t.Fatalf("withdrawn: %+v %v %v", w, already, err)
		}
		if version(ch) <= prodBefore || version(other) != devBefore {
			t.Fatalf("bumped: prod %d→%d, dev %d→%d", prodBefore, version(ch), devBefore, version(other))
		}
		if _, already, err := withdraw("1.0", "again"); err != nil || !already {
			t.Fatalf("again: %v %v", already, err)
		}
		// serving leaves the release out; listings show it withdrawn
		rs, err := s.ChannelReleases(ctx, ch.ID)
		if err != nil || len(rs) != 1 || rs[0].BuildID != keep.ID {
			t.Fatalf("served: %+v %v", rs, err)
		}
		all, err := s.ListReleases(ctx, ch.ID, "geo")
		if err != nil || len(all) != 2 {
			t.Fatalf("listed: %+v %v", all, err)
		}
		for _, r := range all {
			if r.Withdrawn != (r.BuildID == amd.ID) {
				t.Errorf("listed %s %s withdrawn %v", r.PackageVersion, r.Platform, r.Withdrawn)
			}
		}
		if wv, err := s.WithdrawnVersions(ctx, tn.ID, "duckdb_extension"); err != nil || len(wv[[2]string{src, "geo"}]) != 1 {
			t.Fatalf("withdrawn versions: %v %v", wv, err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if w, err := tx.BuildWithdrawn(ctx, amd.ID); err != nil || !w {
				t.Errorf("the locked read: %v %v", w, err)
			}
			if w, err := tx.BuildWithdrawn(ctx, keep.ID); err != nil || w {
				t.Errorf("the locked read of another: %v %v", w, err)
			}
			return nil
		})
		// the body never arrives again, the entry is never purged
		again := cppBuild(tn, "geo", "1.0", "b")
		again.Source, again.Platform, again.Origin = src, "linux_arm64", store.OriginPublication
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &again) }); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("the body again: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.DeleteEntry(ctx, tn.ID, arm.ID); return err }); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("purged: %v", err)
		}
		// no build of the version arrives again: another body, another platform
		for _, platform := range []string{"linux_arm64", "osx_arm64"} {
			other := cppBuild(tn, "geo", "1.0", "d")
			other.Source, other.Platform = src, platform
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &other) }); !errors.Is(err, store.ErrWithdrawn) {
				t.Fatalf("another build of the version (%s): %v", platform, err)
			}
		}
		// never collected; counted in the source's caps, released or not
		if dead, err := s.DeadBuilds(ctx, "default", clock.Add(time.Hour), "", 100); err != nil || len(dead) != 0 {
			t.Fatalf("dead builds: %v %v", dead, err)
		}
		if n, _, err := s.SourceUsage(ctx, tn.ID, src); err != nil || n != 2 {
			t.Fatalf("the source's usage: %d %v", n, err)
		}
		// a purge of held entries leaves a withdrawn one
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetPublisherHold(ctx, tn.ID, "enterest-ci", time.Hour)
			return err
		})
		held := build("1.2", "e", "linux_amd64")
		if _, _, err := withdraw("1.2", "bad"); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			bs, kept, err := tx.PurgeHeld(ctx, tn.ID, src, "")
			if err == nil && (len(bs) != 0 || len(kept) != 0) {
				t.Errorf("purged %+v kept %+v", bs, kept)
			}
			return err
		})
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.GetBuild(ctx, held.ID)
			return err
		})
	})
}

// Spec 0033 phase B: a placement reads its entry with a row lock the withdrawal's update waits for.
// A placement that read the entry first commits its release, and the withdrawal then bumps that
// channel too; a withdrawal that updated first makes the placement read it withdrawn.
func TestWithdrawRace(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "enterest-ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		src := store.PublisherSource(p.ID)
		build := func(ver, hash string) store.Build {
			b := cppBuild(tn, "geo", ver, hash)
			b.Source, b.Origin = src, store.OriginPublication
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		withdraw := func(ver string) <-chan store.Withdrawn {
			done := make(chan store.Withdrawn, 1)
			go func() {
				var w store.Withdrawn
				err := s.InTx(ctx, "", func(tx *store.Tx) error {
					var err error
					w, _, err = tx.Withdraw(ctx, tn.ID, "duckdb_extension", src, "geo", ver, "CVE", "os:1:t")
					return err
				})
				if err != nil {
					t.Error(err)
				}
				done <- w
			}()
			return done
		}
		// the placement first: its release is committed before the withdrawal reads the channels
		first := build("1.0", "a")
		var done <-chan store.Withdrawn
		inTx(t, s, func(tx *store.Tx) error {
			if w, err := tx.BuildWithdrawn(ctx, first.ID); err != nil || w {
				return errors.Join(err, errors.New("withdrawn before the withdrawal"))
			}
			done = withdraw("1.0")
			time.Sleep(300 * time.Millisecond) // the withdrawal reaches its update meanwhile
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: first.ID, Name: "geo", Source: src, PackageVersion: "1.0",
				Platform: "linux_amd64", Slot: first.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPlacement}
			return tx.InsertRelease(ctx, &r, "os:1:t")
		})
		if w := <-done; len(w.Channels) != 1 || w.Channels[0] != ch.ID {
			t.Fatalf("the withdrawal after the placement: %+v", w)
		}
		// the withdrawal first: the placement's locked read waits for it, and sees the entry withdrawn
		second := build("2.0", "b")
		seen := make(chan bool, 1)
		inTx(t, s, func(tx *store.Tx) error {
			w, _, err := tx.Withdraw(ctx, tn.ID, "duckdb_extension", src, "geo", "2.0", "CVE", "os:1:t")
			if err != nil || len(w.Builds) != 1 {
				return errors.Join(err, errors.New("not withdrawn"))
			}
			go func() {
				var w bool
				if err := s.InTx(ctx, "", func(tx *store.Tx) error {
					var err error
					w, err = tx.BuildWithdrawn(ctx, second.ID)
					return err
				}); err != nil {
					t.Error(err)
				}
				seen <- w
			}()
			time.Sleep(300 * time.Millisecond) // the placement reaches its read meanwhile
			return nil
		})
		if !<-seen {
			t.Fatal("a placement racing a withdrawal read the entry available")
		}
	})
}
