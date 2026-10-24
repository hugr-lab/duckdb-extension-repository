package tenants_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0019's lifecycle through the service and the reaper: the minimum suspension, the settle
// period, a run that still holds its lease, and the tombstone the reaper's poll writes.
func TestDeleteAndReap(t *testing.T) {
	ctx := context.Background()
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			st := e.Open(t)
			svc := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			if _, err := svc.CreateTenant(ctx, admin, "gone", "", "", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateChannel(ctx, admin, "gone", "prod", store.ChannelSigned, ""); err != nil {
				t.Fatal(err)
			}
			var refusal *tenants.Refusal
			if _, _, err := svc.Delete(ctx, admin, "gone", 0, time.Hour); !errors.As(err, &refusal) {
				t.Fatalf("an active tenant: %v", err)
			}
			if _, err := svc.SetTenantState(ctx, admin, "gone", store.TenantSuspended, 0); err != nil {
				t.Fatal(err)
			}
			if _, _, err := svc.Delete(ctx, admin, "gone", 0, time.Hour); !errors.As(err, &refusal) {
				t.Fatalf("suspended under the minimum: %v", err)
			}
			tn, d, err := svc.Delete(ctx, admin, "gone", 0, 0)
			if err != nil || tn.Phase() != store.TenantDeleting || len(d.ChannelIDs) != 1 {
				t.Fatalf("delete: %+v %+v %v", tn, d, err)
			}
			if _, err := svc.SetTenantState(ctx, admin, "gone", store.TenantActive, 0); !errors.Is(err, store.ErrDeleting) {
				t.Fatalf("resume of a deleting tenant: %v", err)
			}

			r := &tenants.Reaper{Store: st, Holder: "test", Log: slog.New(slog.DiscardHandler)}
			if _, err := r.Reap(ctx, "gone", false); !errors.Is(err, tenants.ErrSettling) {
				t.Fatalf("before the settle period: %v", err)
			}
			// a feed run of the tenant holding its lease: the pass waits
			if err := store.ExecRaw(ctx, st, "INSERT INTO feeds (id, tenant_id, name, url, kinds, readers, offer, state, version, status, "+
				"created_at, created_by) VALUES ('f1', ?, 'f', 'https://src.example/api/v1/tenants/x', '[]', ?, 'upstreams', 'active', 1, 'new', ?, 'os')",
				tn.ID, false, store.TimeArg(st, st.Now())); err != nil {
				t.Fatal(err)
			}
			if held, err := st.AcquireLease(ctx, "kista/feed/f1", "a run", time.Minute); err != nil || !held {
				t.Fatal(held, err)
			}
			r.Settle = time.Nanosecond
			r.Once(ctx)
			if cur, _ := st.GetTenantAny(ctx, "gone"); cur.Phase() != store.TenantDeleting {
				t.Fatalf("reaped under a held run lease: %s", cur.Phase())
			}
			if err := st.ReleaseLease(ctx, "kista/feed/f1", "a run"); err != nil {
				t.Fatal(err)
			}
			if err := store.ExecRaw(ctx, st, "UPDATE leases SET expires_at = ? WHERE name = 'kista/feed/f1'", store.TimeArg(st, st.Now().Add(-time.Minute))); err != nil {
				t.Fatal(err)
			}
			if err := store.ExecRaw(ctx, st, "UPDATE tenant_deletions SET next_attempt_at = ?", store.TimeArg(st, st.Now().Add(-time.Minute))); err != nil {
				t.Fatal(err)
			}
			r.Once(ctx)
			cur, err := st.GetTenantAny(ctx, "gone")
			if err != nil || cur.Phase() != store.TenantDeleted {
				t.Fatalf("after the poll: %+v %v", cur, err)
			}
			if n, _ := store.CountRaw(ctx, st, "leases", "name = 'kista/feed/f1' OR name LIKE 'kista/tenant-delete/%'"); n != 0 {
				t.Errorf("%d lease rows left", n)
			}
			if got, err := r.Reap(ctx, "gone", false); err != nil || got.Phase() != store.TenantDeleted {
				t.Fatalf("reaping a tombstone: %v", err)
			}
		})
	}
}

// The backoff after failed passes in a row: 1 minute, doubling, at most an hour.
func TestBackoff(t *testing.T) {
	for n, want := range map[int64]time.Duration{0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 7: time.Hour, 50: time.Hour} {
		if got := tenants.Backoff(n); got != want {
			t.Errorf("%d: %s, want %s", n, got, want)
		}
	}
}
