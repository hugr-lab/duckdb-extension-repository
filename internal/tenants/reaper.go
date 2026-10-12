package tenants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Reaper removes deleting tenants' rows and writes their tombstones (spec 0019): one replica a
// tenant at a time, under the lease kista/tenant-delete/<id>, after the settle period.
type Reaper struct {
	Store  *store.Store
	Holder string // this replica, or the command
	Log    *slog.Logger
	Poll   time.Duration // default 1m
	Settle time.Duration // default SettlePeriod
}

// SettlePeriod is how long after a deletion starts the reaper waits: twice the intake deadline
// (spec 0016), so work that started before it has finished or failed.
const SettlePeriod = 2 * time.Hour

// Late sweep: tombstones are checked again hourly for a week.
const (
	lateSweepFor   = 7 * 24 * time.Hour
	lateSweepEvery = time.Hour
	reapLeaseTTL   = 5 * time.Minute
	maxBackoff     = time.Hour
)

// ErrSettling is a reap asked for before the settle period ended.
var ErrSettling = errors.New("tenants: the settle period has not ended")

// errRunsHeld is a pass that waits for the tenant's runs to release their leases.
var errRunsHeld = errors.New("tenants: the tenant's upstream or feed runs still hold their leases")

func (r *Reaper) settle() time.Duration {
	if r.Settle == 0 {
		return SettlePeriod
	}
	return r.Settle
}

// Run polls until ctx is done.
func (r *Reaper) Run(ctx context.Context) {
	if r.Poll == 0 {
		r.Poll = time.Minute
	}
	t := time.NewTicker(r.Poll)
	defer t.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once is one poll: the deletions due, then the tombstones to check again.
func (r *Reaper) Once(ctx context.Context) {
	if ok, err := r.Store.SchemaCurrent(ctx); err != nil || !ok {
		return // an older (or newer) binary than the schema: another replica reaps
	}
	now := r.Store.Now()
	ids, err := r.Store.DueDeletions(ctx, now.Add(-r.settle()))
	if err != nil {
		r.Log.Error("tenants: listing deletions", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := r.reap(ctx, id, false); err != nil && !errors.Is(err, errRunsHeld) && !errors.Is(err, errLeaseHeld) {
			r.Log.Error("tenants: a deletion pass failed", "tenant_id", id, "error", err)
		}
	}
	late, err := r.Store.LateSweeps(ctx, now.Add(-lateSweepFor), now.Add(-lateSweepEvery))
	if err != nil {
		r.Log.Error("tenants: listing tombstones to check", "error", err)
		return
	}
	for _, id := range late {
		r.sweep(ctx, id)
	}
}

var errLeaseHeld = errors.New("tenants: another reaper holds the tenant's deletion")

// Reap runs a deleting tenant's reaper in this process until its tombstone is written (kista
// admin tenant reap). force skips the settle period: the operator vouches that nothing else runs.
func (r *Reaper) Reap(ctx context.Context, name string, force bool) (store.Tenant, error) {
	t, err := r.Store.GetTenantAny(ctx, name)
	if err != nil {
		return t, err
	}
	switch t.Phase() {
	case store.TenantDeleted:
		return t, nil
	case store.TenantDeleting:
	default:
		return t, &Refusal{"the tenant is not being deleted"}
	}
	if ok, err := r.Store.SchemaCurrent(ctx); err != nil || !ok {
		return t, fmt.Errorf("%w: the database's schema is not this binary's", store.ErrInvalid)
	}
	if left := t.DeletingAt.Add(r.settle()).Sub(r.Store.Now()); left > 0 && !force {
		return t, fmt.Errorf("%w: %s left (or -force when nothing else runs)", ErrSettling, left.Round(time.Minute))
	}
	if err := r.reap(ctx, t.ID, force); err != nil {
		return t, err
	}
	return r.Store.GetTenantAny(ctx, name)
}

// reap runs one pass over a tenant under its lease, recording progress, or the failure and its
// backoff.
func (r *Reaper) reap(ctx context.Context, id string, force bool) error {
	st := r.Store
	lease := "kista/tenant-delete/" + id
	held, err := st.AcquireLease(ctx, lease, r.Holder, reapLeaseTTL)
	if err != nil {
		return err
	}
	if !held {
		return errLeaseHeld
	}
	defer func() { _ = st.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder) }()
	t, err := st.GetTenantAnyByID(ctx, id)
	if err != nil || t.Phase() != store.TenantDeleting {
		return err
	}
	d, err := st.GetDeletion(ctx, id)
	if err != nil {
		return err
	}
	runs, err := st.RunLeases(ctx, id, d.ChannelIDs)
	if err != nil {
		return err
	}
	if held, err := st.HeldLeases(ctx, runs); err != nil {
		return err
	} else if len(held) > 0 && !force {
		_ = st.DeletionPaused(ctx, id, st.Now().Add(time.Minute), "", 0) // a pause, not a failure
		return errRunsHeld
	}
	if err := st.DeleteExpiredLeases(ctx, runs); err != nil {
		return err
	}
	table := ""
	var deleted int64
	_, err = st.ReapTenant(ctx, t, d, func(tb string, n int64) error {
		table, deleted = tb, deleted+n
		if ok, err := st.RenewLease(ctx, lease, r.Holder, reapLeaseTTL); err != nil || !ok {
			return errReapLease
		}
		return nil
	})
	if err == nil {
		r.Log.Info("tenants: a tenant's tombstone was written", "tenant", t.Name, "rows", deleted)
		return st.DeleteLease(context.WithoutCancel(ctx), lease, r.Holder)
	}
	if errors.Is(err, context.Canceled) { // a shutdown: the next replica goes on, nothing failed
		return errors.Join(err, st.DeletionPaused(context.WithoutCancel(ctx), id, st.Now(), table, deleted))
	}
	class := reapClass(err)
	changed, ferr := st.DeletionFailed(context.WithoutCancel(ctx), id, class, Backoff, table, deleted)
	if ferr == nil && changed {
		ferr = r.failedEvent(context.WithoutCancel(ctx), t, d, class)
	}
	return errors.Join(err, ferr)
}

var errReapLease = errors.New("tenants: the reaper lost its lease")

// Backoff is the wait after failures failed passes in a row: 1 minute, doubling, at most an hour.
func Backoff(failures int64) time.Duration {
	if failures < 1 {
		failures = 1
	}
	return min(time.Minute<<min(failures-1, 6), maxBackoff)
}

// reapClass is a failed pass's error class for its event (spec 0010: never the error's text).
func reapClass(err error) string {
	switch {
	case errors.Is(err, errReapLease):
		return "lease"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	}
	return "error"
}

func (r *Reaper) failedEvent(ctx context.Context, t store.Tenant, d store.Deletion, class string) error {
	e, err := r.Store.NewEvent(ctx, t.ID, "system:tenant-delete", "tenant.purge", "failed", "tenant:"+t.Name,
		map[string]any{"rows": d.DeletedRows, "passes": d.Passes + 1, "late": false, "error": class})
	if err != nil {
		return err
	}
	return r.Store.InsertEvents(ctx, []store.Event{e})
}

// sweep checks a tombstone again for rows that appeared under it.
func (r *Reaper) sweep(ctx context.Context, id string) {
	st := r.Store
	lease := "kista/tenant-delete/" + id
	if held, err := st.AcquireLease(ctx, lease, r.Holder, reapLeaseTTL); err != nil || !held {
		return
	}
	defer func() { _ = st.DeleteLease(context.WithoutCancel(ctx), lease, r.Holder) }()
	t, err := st.GetTenantAnyByID(ctx, id)
	if err != nil {
		return
	}
	d, err := st.GetDeletion(ctx, id)
	if err != nil {
		return
	}
	n, err := st.SweepTombstone(ctx, t, d)
	switch {
	case err != nil:
		r.Log.Error("tenants: checking a tombstone", "tenant", t.Name, "error", err)
	case n > 0:
		r.Log.Warn("tenants: rows appeared under a tombstone and were deleted", "tenant", t.Name, "rows", n)
	}
}
