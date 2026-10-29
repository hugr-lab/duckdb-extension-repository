package store_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0046 (spec 0037 PR 2) on a level-45 database: tenant reservations are gone, a tenant's
// minimum hold for distributors' publishers is one day, and a level-45 binary no longer runs here
// (min_reader 46).
func TestMigrationDistributorHolds(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 45); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC))
		const tid = "t0000000-0000-0000-0000-000000000000"
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			{"INSERT INTO tenant_reserved_names (tenant_id, package_type, name, holder, added_at, added_by) " +
				"VALUES (?, 'duckdb_extension', 'held', 'enterest', ?, 'os:1:t')", tid, now},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 46 AND min_reader = 46"); err != nil || n != 1 {
			t.Fatalf("migration 0046 needs a reader at 46: %d %v", n, err)
		}
		if _, err := store.CountRaw(ctx, s, "tenant_reserved_names", "1 = 1"); err == nil {
			t.Fatal("tenant_reserved_names is still there")
		}
		tn, err := s.GetTenant(ctx, "acme")
		if err != nil || tn.DistributorHold != 24*time.Hour || store.DefaultDistributorHold != 24*time.Hour {
			t.Fatalf("the default minimum hold: %+v %v", tn, err)
		}
		if n, err := store.CountRaw(ctx, s, "tenants", "distributor_hold_seconds = 86400"); err != nil || n != 1 {
			t.Fatalf("the column's default: %d %v", n, err)
		}
		if err := store.CheckAs(ctx, s, 45); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("a level-45 binary on a level-46 database: %v", err)
		}
	})
}

// Spec 0037 PR 2: an entry from a distributor's publisher is held for the larger of the publisher's
// hold and the tenant's minimum; a tenant's own publisher's only for its own; the minimum's change
// and the sources it applies to (every distributor's publisher of the tenant, live or removed).
func TestDistributorHold(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, _ := fixture(t, s)
		if tn.DistributorHold != store.DefaultDistributorHold {
			t.Fatalf("a new tenant's minimum: %s", tn.DistributorHold)
		}
		own := store.Publisher{TenantID: tn.ID, Name: "ci"}
		dist := store.Publisher{TenantID: tn.ID, Name: "enterest-ci", Distributor: "enterest"}
		gone := store.Publisher{TenantID: tn.ID, Name: "enterest-old", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error {
			for _, p := range []*store.Publisher{&own, &dist, &gone} {
				if err := tx.InsertPublisher(ctx, p); err != nil {
					return err
				}
			}
			return nil
		})
		hold := func(p store.Publisher) time.Duration {
			t.Helper()
			d, err := s.SourceHold(ctx, store.PublisherSource(p.ID))
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
		if d := hold(dist); d != 24*time.Hour {
			t.Fatalf("a distributor's publisher without a hold: %s", d)
		}
		if d := hold(own); d != 0 {
			t.Fatalf("the tenant's own publisher: %s", d)
		}
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetPublisherHold(ctx, tn.ID, dist.Name, 48*time.Hour)
			return err
		})
		if d := hold(dist); d != 48*time.Hour {
			t.Fatalf("a distributor's publisher's longer hold: %s", d)
		}
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = store.PublisherSource(dist.ID)
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		if !b.AvailableAt.Equal(clock.Add(48 * time.Hour)) {
			t.Fatalf("held for the publisher's: %+v", b)
		}
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetPublisherHold(ctx, tn.ID, dist.Name, 0)
			return err
		})
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, gone.Name, "os:1:t") })
		inTx(t, s, func(tx *store.Tx) error {
			srcs, err := tx.DistributorPublisherSources(ctx, tn.ID)
			want := []string{store.PublisherSource(dist.ID), store.PublisherSource(gone.ID)}
			slices.Sort(want)
			if err == nil && !slices.Equal(srcs, want) {
				t.Errorf("the distributors' sources: %v, want %v", srcs, want)
			}
			return err
		})
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetDistributorHold(ctx, tn.ID, 2*time.Hour)
			if err == nil && before != 24*time.Hour {
				t.Errorf("the minimum before: %s", before)
			}
			return err
		})
		if d := hold(dist); d != 2*time.Hour {
			t.Fatalf("after the minimum changed: %s", d)
		}
		if got, err := s.GetTenant(ctx, "acme"); err != nil || got.DistributorHold != 2*time.Hour {
			t.Fatalf("the tenant's minimum: %+v %v", got, err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			_, err := tx.SetDistributorHold(ctx, tn.ID, store.MaxHold+time.Second)
			return err
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a minimum past the longest hold: %v", err)
		}
	})
}

// Spec 0037 PR 2: a build inserted from a distributor's publisher while the tenant's minimum is
// being raised is held the raised time: the insert reads the tenant's row with a shared lock the
// raise waits for until the arrival commits (without it, on PostgreSQL and SQL Server, the raise would
// finish meanwhile and miss the build), so the raise then finds the build. SQLite serialises
// writers.
func TestDistributorHoldReadLocked(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, _ := fixture(t, s)
		dist := store.Publisher{TenantID: tn.ID, Name: "enterest-ci", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &dist); err != nil {
				return err
			}
			_, err := tx.SetDistributorHold(ctx, tn.ID, time.Hour)
			return err
		})
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = store.PublisherSource(dist.ID)
		raised := make(chan error, 1)
		var done error
		finished := false
		inTx(t, s, func(tx *store.Tx) error {
			// the minimum read first, alone: its lock, not the build insert's own (WritableTenant),
			// is what the raise must wait for
			if d, err := tx.SourceHold(ctx, b.Source); err != nil || d != time.Hour {
				t.Errorf("the minimum read: %s %v", d, err)
			}
			go func() {
				raised <- s.InTenantTxForUpdate(ctx, tn.ID, nil, func(tx *store.Tx) error {
					if _, err := tx.SetDistributorHold(ctx, tn.ID, 3*time.Hour); err != nil {
						return err
					}
					_, err := tx.RaiseHeld(ctx, tn.ID, b.Source, 3*time.Hour)
					return err
				})
			}()
			time.Sleep(300 * time.Millisecond) // the raise reaches the tenant's row meanwhile
			select {
			case done = <-raised:
				finished = true
				t.Errorf("the raise committed while an arrival had read the minimum: %v", done)
			default: // it waits for this transaction
			}
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			if !b.AvailableAt.Equal(b.CreatedAt.Add(time.Hour)) {
				t.Errorf("held for the minimum read: %+v", b)
			}
			return nil
		})
		if !finished {
			done = <-raised
		}
		if done != nil {
			t.Fatal(done)
		}
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetBuild(ctx, b.ID)
			if err == nil && !got.AvailableAt.Equal(b.CreatedAt.Add(3*time.Hour)) {
				t.Errorf("a build inserted during the raise: %+v", got)
			}
			return err
		})
	})
}

// Spec 0037 PR 2: a tenant's distributors' publishers are counted apart from its own
// (MaxDistributorPublishers), so they never take the tenant's own room; a distributor's publisher is
// inserted marked.
func TestDistributorPublisherCap(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		insert := func(name, dist string) error {
			return s.InTx(ctx, "", func(tx *store.Tx) error {
				return tx.InsertPublisher(ctx, &store.Publisher{TenantID: tn.ID, Name: name, Distributor: dist})
			})
		}
		for i := range store.MaxDistributorPublishers {
			if err := insert("dist-"+strconv.Itoa(i), "enterest"); err != nil {
				t.Fatal(err)
			}
		}
		if err := insert("dist-more", "enterest"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("past the distributors' cap: %v", err)
		}
		if err := insert("own-ci", ""); err != nil {
			t.Fatalf("the tenant's own, with the distributors' cap full: %v", err)
		}
		if err := insert("bad-dist", "line\nbreak"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a distributor's name: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			p, err := tx.GetPublisher(ctx, tn.ID, "dist-0")
			if err == nil && p.Distributor != "enterest" {
				t.Errorf("inserted marked: %+v", p)
			}
			return err
		})
	})
}
