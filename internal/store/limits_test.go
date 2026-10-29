package store_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0047 (spec 0038) on a level-46 database: a build's body_len is filled from its blob (none
// without one), every tenant has no limits of its own, and a level-46 binary no longer runs here.
func TestMigrationTenantLimits(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 46); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC))
		const tid = "t0000000-0000-0000-0000-000000000000"
		blob := blobRec("a", "1", 2<<20)
		if err := putBlob(t, s, &blob); err != nil {
			t.Fatal(err)
		}
		build := func(id, hash string) []any {
			return []any{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked) VALUES (?, ?, 'tenant', ?, '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'admin', ?, 'os:1:t', ?)",
				id, tid, "ext" + id[:1], strings.Repeat(hash, 64), now, true}
		}
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, storage_domain, created_at, version) VALUES (?, 'acme', '', 'active', 'default', ?, 1)", tid, now},
			build("a0000000-0000-0000-0000-000000000000", "a"),
			build("b0000000-0000-0000-0000-000000000000", "b"), // its blob is gone
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 47 AND min_reader = 47"); err != nil || n != 1 {
			t.Fatalf("migration 0047 needs a reader at 47: %d %v", n, err)
		}
		if n, err := store.CountRaw(ctx, s, "builds", "body_len = 3145728"); err != nil || n != 1 {
			t.Fatalf("the filled length: %d %v", n, err)
		}
		if n, err := store.CountRaw(ctx, s, "builds", "body_len IS NULL"); err != nil || n != 1 {
			t.Fatalf("a build without its blob: %d %v", n, err)
		}
		if tn, err := s.GetTenant(ctx, "acme"); err != nil || tn.Limits != store.DefaultLimits() {
			t.Fatalf("no limits of its own: %+v %v", tn.Limits, err)
		}
		if err := store.CheckAs(ctx, s, 46); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("a level-46 binary on a level-47 database: %v", err)
		}
	})
}

// Spec 0038: a tenant's own bytes are each distinct body of its kept own builds once (placed,
// landed, withdrawn; not one waiting for the collector), and the icons of its own entries that still
// have a kept build; a distributor's sources, live or removed, are left out, and so is another
// tenant's. The figure against a hand count; FillBodyLen; the icons' sum past 2 GiB.
func TestTenantBytes(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		own := store.Publisher{TenantID: tn.ID, Name: "ci"}
		dist := store.Publisher{TenantID: tn.ID, Name: "enterest-ci", Distributor: "enterest"}
		gone := store.Publisher{TenantID: tn.ID, Name: "enterest-old", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error {
			for _, p := range []*store.Publisher{&own, &dist, &gone} {
				if err := tx.InsertPublisher(ctx, p); err != nil {
					return err
				}
			}
			_, err := tx.SetDistributorHold(ctx, tn.ID, 0)
			return err
		})
		add := func(tenant store.Tenant, source, name, hash string, size int64) store.Build {
			t.Helper()
			b := cppBuild(tenant, name, "1.0", hash)
			b.Source, b.BodyLen = source, size
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		landed := func(b store.Build) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error { return tx.SetLanded(ctx, b.ID) })
		}
		placed := add(tn, store.SourceTenant, "geo", "a", 100)
		inTx(t, s, func(tx *store.Tx) error {
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: placed.ID, Name: "geo", Source: store.SourceTenant,
				PackageVersion: "1.0", Platform: placed.Platform, Slot: placed.Slot(), State: store.ReleaseActive, Visibility: store.Public,
				Origin: store.OriginAdmin}
			return tx.InsertRelease(ctx, &r, "os:1:t")
		})
		landed(add(tn, store.SourceTenant, "lib", "b", 200))
		add(tn, store.SourceTenant, "dead", "c", 400)                   // waiting for the collector
		landed(add(tn, store.PublisherSource(own.ID), "geo", "a", 100)) // a body counted once
		landed(add(tn, store.PublisherSource(dist.ID), "geo", "d", 800))
		w := add(tn, store.PublisherSource(gone.ID), "geo", "e", 1600)
		inTx(t, s, func(tx *store.Tx) error {
			_, _, err := tx.Withdraw(ctx, tn.ID, "duckdb_extension", store.PublisherSource(gone.ID), "geo", "1.0", "CVE", "os:1:t")
			return err
		})
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, gone.Name, "os:1:t") })
		inTx(t, s, func(tx *store.Tx) error {
			if b, err := tx.GetBuild(ctx, w.ID); err != nil || !b.Withdrawn() {
				t.Errorf("a removed distributor's withdrawn build is kept: %+v %v", b, err)
			}
			return nil
		})
		other := store.Tenant{Name: "beta"}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateTenant(ctx, &other) })
		landed(add(other, store.SourceTenant, "lib", "b", 200))
		icon := func(source, name string, size int64) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: source, Name: name,
					IconSHA256: strings.Repeat("f", 64), IconWidth: 64, IconHeight: 64, IconBytes: size, SetBy: "os:1:t", SetAt: time.Now()})
			})
		}
		icon(store.SourceTenant, "geo", 10)
		icon(store.SourceTenant, "dead", 20)             // its entry has no kept build
		icon(store.SourceTenant, "none", 40)             // no build at all
		icon(store.PublisherSource(dist.ID), "geo", 80)  // a distributor's
		icon(store.PublisherSource(gone.ID), "geo", 160) // a removed distributor's
		if got, err := s.TenantBytes(ctx, tn.ID); err != nil || got != 100+200+10 {
			t.Fatalf("the tenant's bytes: %d %v", got, err)
		}
		if got, err := s.TenantBytes(ctx, other.ID); err != nil || got != 200 {
			t.Fatalf("the other tenant's: %d %v", got, err)
		}
		u, err := s.TenantUsage(ctx, tn.ID)
		if err != nil || u != (store.Limits{Bytes: 310, Channels: 1, Upstreams: 0, Publishers: 1}) {
			t.Fatalf("the usage: %+v %v", u, err)
		}

		// a row an older replica inserted counts 0 until the collector fills it
		blob := blobRec("9", "8", 4<<20)
		if err := putBlob(t, s, &blob); err != nil {
			t.Fatal(err)
		}
		landed(add(tn, store.SourceTenant, "old", "9", 0))
		if got, _ := s.TenantBytes(ctx, tn.ID); got != 310 {
			t.Fatalf("before the fill: %d", got)
		}
		if n, err := s.FillBodyLen(ctx, "default"); err != nil || n != 1 {
			t.Fatalf("filled: %d %v", n, err)
		}
		if got, _ := s.TenantBytes(ctx, tn.ID); got != 310+3<<20 {
			t.Fatalf("after the fill: %d", got)
		}

		// icons past 2 GiB (SQL Server sums an int column as int)
		for _, n := range []string{"big1", "big2"} {
			landed(add(other, store.SourceTenant, n, n[3:], 1))
			inTx(t, s, func(tx *store.Tx) error {
				return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: other.ID, PackageType: "duckdb_extension", Source: store.SourceTenant,
					Name: n, IconSHA256: strings.Repeat("e", 64), IconWidth: 64, IconHeight: 64, IconBytes: 2_000_000_000, SetBy: "os:1:t", SetAt: time.Now()})
			})
		}
		if got, err := s.TenantBytes(ctx, other.ID); err != nil || got != 200+2+4_000_000_000 {
			t.Fatalf("icons past 2 GiB: %d %v", got, err)
		}
		if _, bytes, err := s.SourceUsage(ctx, other.ID, store.SourceTenant); err != nil || bytes < 4_000_000_000 {
			t.Fatalf("the source's usage past 2 GiB: %d %v", bytes, err)
		}
	})
}

// Spec 0038: a tenant's own values of its limits round-trip (default, unlimited, 0, a value), apply
// over the server's defaults, and refuse growth past them with a LimitError (the limit before the
// ceiling); a distributor's publishers and a call WithoutLimits are never refused; a body the
// tenant counts already always passes.
func TestTenantLimits(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		set := func(l map[string]int64) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				cur, err := tx.GetTenantForUpdate(ctx, "acme")
				if err != nil {
					return err
				}
				return tx.SetTenantLimits(ctx, &cur, l)
			})
		}
		set(map[string]int64{store.LimitBytes: store.LimitUnlimited, store.LimitChannels: 0, store.LimitUpstreams: 5})
		got, err := s.GetTenant(ctx, "acme")
		if err != nil || got.Limits != (store.Limits{Bytes: -1, Channels: 0, Upstreams: 5, Publishers: store.LimitDefault}) {
			t.Fatalf("stored: %+v %v", got.Limits, err)
		}
		s.LimitDefaults = store.Limits{Bytes: 1 << 30, Channels: store.LimitUnlimited, Upstreams: 7, Publishers: 3}
		if in := s.Effective(got.Limits); in != (store.Limits{Bytes: -1, Channels: 0, Upstreams: 5, Publishers: 3}) {
			t.Fatalf("in force: %+v", in)
		}
		s.LimitDefaults = store.UnlimitedLimits()
		if in := s.Effective(store.DefaultLimits()); in != (store.Limits{Bytes: -1, Channels: 1000, Upstreams: 100, Publishers: 200}) {
			t.Fatalf("the defaults' unlimited: %+v", in)
		}
		for _, bad := range []struct {
			n string
			v int64
		}{{store.LimitChannels, 1001}, {store.LimitUpstreams, -3}, {"egress", 1}} {
			if err := store.ValidLimit(bad.n, bad.v); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s %d: %v", bad.n, bad.v, err)
			}
		}

		// channels: none at all, then the limit; a NoLimits transaction past it
		channel := func(name string, lifted bool) error {
			c := store.Channel{TenantID: tn.ID, Name: name, Kind: store.ChannelSigned}
			cx := ctx
			if lifted {
				cx = store.WithoutLimits(ctx)
			}
			return s.InTenantTxForUpdate(cx, tn.ID, nil, func(tx *store.Tx) error { return tx.CreateChannel(cx, &c) })
		}
		var le *store.LimitError
		if err := channel("dev", false); !errors.As(err, &le) || le.Limit != store.LimitChannels || le.AtCeiling {
			t.Fatalf("a channel past 0: %v", err)
		}
		set(map[string]int64{store.LimitChannels: 2})
		if err := channel("dev", false); err != nil {
			t.Fatal(err)
		}
		if err := channel("qa", false); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("a channel past 2: %v", err)
		}
		if err := channel("qa", true); err != nil {
			t.Fatalf("the bootstrap's channel: %v", err)
		}
		set(map[string]int64{store.LimitChannels: store.LimitUnlimited})
		if err := channel("ops", false); err != nil {
			t.Fatal(err)
		}

		// publishers: the tenant's own, not a distributor's
		set(map[string]int64{store.LimitPublishers: 1})
		pub := func(name, dist string) error {
			p := store.Publisher{TenantID: tn.ID, Name: name, Distributor: dist}
			return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		}
		if err := pub("ci", ""); err != nil {
			t.Fatal(err)
		}
		if err := pub("ci2", ""); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("a publisher past 1: %v", err)
		}
		if err := pub("enterest-ci", "enterest"); err != nil {
			t.Fatalf("a distributor's publisher: %v", err)
		}

		// bytes: the room left, a body counted already, an unknown length
		set(map[string]int64{store.LimitBytes: 150})
		intake := func(source, name, hash string, size int64) error {
			b := cppBuild(tn, name, "1.0", hash)
			b.Source, b.BodyLen = source, size
			keys, err := s.BytesLocks(ctx, tn.ID)
			if err != nil {
				return err
			}
			return s.InTxLocks(ctx, keys, func(tx *store.Tx) error {
				if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
					return err
				}
				return tx.SetLanded(ctx, b.ID)
			})
		}
		if err := intake(store.SourceTenant, "geo", "a", 100); err != nil {
			t.Fatal(err)
		}
		if err := intake(store.SourceTenant, "lib", "b", 51); !errors.As(err, &le) || le.Limit != store.LimitBytes {
			t.Fatalf("a body past the limit: %v", err)
		}
		if err := s.BytesRoom(ctx, tn.ID, store.SourceTenant, strings.Repeat("b", 64), 51); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("the early check: %v", err)
		}
		var p, d store.Publisher
		inTx(t, s, func(tx *store.Tx) error {
			var err error
			if p, err = tx.GetPublisher(ctx, tn.ID, "ci"); err != nil {
				return err
			}
			d, err = tx.GetPublisher(ctx, tn.ID, "enterest-ci")
			return err
		})
		if err := intake(store.PublisherSource(p.ID), "geo", "a", 100); err != nil {
			t.Fatalf("a body the tenant counts, in another source: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetDistributorHold(ctx, tn.ID, 0); return err })
		if err := intake(store.PublisherSource(d.ID), "geo", "z", 1<<20); err != nil {
			t.Fatalf("a distributor's body past the limit: %v", err)
		}
		if err := intake(store.SourceTenant, "lib", "b", 50); err != nil {
			t.Fatal(err)
		}
		if err := intake(store.SourceTenant, "len", "c", 0); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("an unknown length at a full tenant: %v", err)
		}
		if err := s.BytesRoom(store.WithoutLimits(ctx), tn.ID, store.SourceTenant, strings.Repeat("c", 64), 1); err != nil {
			t.Fatalf("a bundle import: %v", err)
		}
		if n := store.LimitRefusals()[store.LimitBytes]; n < 3 {
			t.Fatalf("the refusals counted: %d", n)
		}
	})
}

// Spec 0038: intakes and inserts at once at the last place or the last room: exactly one passes, on
// one store or two (replicas), with the bytes lock taken at the start or, for a limit set after the
// caller read it, in the transaction; a lowering change racing an insert leaves no more than the
// limit unless the insert committed first.
func TestTenantLimitsConcurrent(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		stores := e.Fresh(t, 2)
		for _, s := range stores {
			if err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
		}
		s := stores[0]
		tn, _ := fixture(t, s)
		set := func(l map[string]int64) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				cur, err := tx.GetTenantForUpdate(ctx, "acme")
				if err != nil {
					return err
				}
				return tx.SetTenantLimits(ctx, &cur, l)
			})
		}
		race := func(n int, f func(i int) error) (ok, refused int) {
			t.Helper()
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := f(i)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						ok++
					case errors.Is(err, store.ErrTenantLimit):
						refused++
					default:
						t.Errorf("%d: %v", i, err)
					}
				}()
			}
			wg.Wait()
			return ok, refused
		}
		set(map[string]int64{store.LimitChannels: 2})
		if ok, refused := race(2, func(i int) error {
			c := store.Channel{TenantID: tn.ID, Name: []string{"dev", "qa"}[i], Kind: store.ChannelSigned}
			return stores[i].InTenantTxForUpdate(ctx, tn.ID, nil, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &c) })
		}); ok != 1 || refused != 1 {
			t.Fatalf("channels at the last place: %d passed, %d refused", ok, refused)
		}
		set(map[string]int64{store.LimitBytes: 100})
		for _, keyed := range []bool{true, false} {
			hashes := []string{"1", "2"}
			if !keyed {
				hashes = []string{"3", "4"}
			}
			if ok, refused := race(2, func(i int) error {
				b := cppBuild(tn, "ext"+hashes[i], "1.0", hashes[i])
				b.BodyLen = 60
				var keys []string
				if keyed {
					keys = []string{store.TenantBytesLock(tn.ID)}
				}
				return stores[i].InTxLocks(ctx, keys, func(tx *store.Tx) error {
					if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
						return err
					}
					return tx.SetLanded(ctx, b.ID)
				})
			}); ok != 1 || refused != 1 {
				t.Fatalf("bodies at the last room (lock at the start %v): %d passed, %d refused", keyed, ok, refused)
			}
			set(map[string]int64{store.LimitBytes: 160}) // room for one more after the first round
		}
		// a lowering change racing an insert: never more than the new limit, unless the insert came first
		set(map[string]int64{store.LimitChannels: store.LimitUnlimited})
		var inserted bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := stores[1].InTx(ctx, "", func(tx *store.Tx) error {
				cur, err := tx.GetTenantForUpdate(ctx, "acme")
				if err != nil {
					return err
				}
				return tx.SetTenantLimits(ctx, &cur, map[string]int64{store.LimitChannels: 2})
			})
			if err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			c := store.Channel{TenantID: tn.ID, Name: "late", Kind: store.ChannelSigned}
			err := s.InTenantTxForUpdate(ctx, tn.ID, nil, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &c) })
			if err != nil && !errors.Is(err, store.ErrTenantLimit) {
				t.Error(err)
			}
			inserted = err == nil
		}()
		wg.Wait()
		u, err := s.TenantUsage(ctx, tn.ID)
		if err != nil || inserted != (u.Channels == 3) || u.Channels > 3 {
			t.Fatalf("channels after the race: %d (inserted %v) %v", u.Channels, inserted, err)
		}
	})
}
