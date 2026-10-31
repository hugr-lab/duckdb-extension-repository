package tenants

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Deleting a tenant (spec 0019): a server administrator deletes a tenant suspended for at least
// the minimum; the reaper removes its rows and writes the tombstone.

// Refusal is a request the tenant's phase refuses, with the reason the caller is told (409).
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "tenants: " + r.Reason }

// DeletableAt is when a suspended tenant may be deleted, or zero for one that may not be: active,
// deleting, or suspended by an older binary (no suspended_at).
func DeletableAt(t store.Tenant, minSuspended time.Duration) time.Time {
	if t.Phase() != store.TenantSuspended || t.SuspendedAt.IsZero() {
		return time.Time{}
	}
	return t.SuspendedAt.Add(minSuspended)
}

// Delete starts deleting a tenant (server-wide action): it must be suspended for at least
// minSuspended, and a non-zero expected version must be its own. From the commit the tenant is
// closed; the reaper removes its rows later.
func (s *Service) Delete(ctx context.Context, a authz.Actor, name string, expected int64, minSuspended time.Duration) (store.Tenant, store.Deletion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, store.Deletion{}, err
	}
	var t store.Tenant
	var d store.Deletion
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if t, err = tx.GetTenantAnyForUpdate(ctx, name); err != nil {
			return err
		}
		switch t.Phase() {
		case store.TenantDeleting:
			return &Refusal{"the tenant is being deleted"}
		case store.TenantDeleted:
			return &Refusal{"the tenant was deleted"}
		case store.TenantActive:
			return &Refusal{"only a suspended tenant can be deleted: suspend it first"}
		}
		if expected != 0 && t.Version != expected {
			return store.ErrConflict
		}
		if t.SuspendedAt.IsZero() {
			return &Refusal{"the tenant was suspended by an older version: resume it and suspend it again"}
		}
		if at := DeletableAt(t, minSuspended); tx.Now().Before(at) {
			return &Refusal{fmt.Sprintf("the tenant may be deleted from %s (tenants.min_suspended)", at.UTC().Format(time.RFC3339))}
		}
		if d, err = tx.StartDeletion(ctx, &t, a.String()); err != nil {
			return err
		}
		fps := []string{}
		for _, k := range d.Keys {
			fps = append(fps, k.Fingerprint)
		}
		return tx.Event(ctx, t.ID, a.String(), "tenant.delete", "tenant:"+t.Name,
			map[string]any{"channels": len(d.ChannelIDs), "suspended_at": t.SuspendedAt.UTC().Format(time.RFC3339), "keys": fps,
				"min_suspended": audit.Duration(minSuspended)}) // the minimum this process applied
	})
	return t, d, err
}

// Show reads a tenant in any phase, with its deletion record once its deletion started (server-wide
// action).
func (s *Service) Show(ctx context.Context, a authz.Actor, name string) (store.Tenant, *store.Deletion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, nil, err
	}
	t, err := s.Store.GetTenantAny(ctx, name)
	if err != nil || t.DeletingAt.IsZero() {
		return t, nil, err
	}
	d, err := s.Store.GetDeletion(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return t, nil, nil
	}
	return t, &d, err
}

// Phases filters tenants by phase; none given: every phase but deleted.
func Phases(ts []store.Tenant, phases ...string) []store.Tenant {
	out := []store.Tenant{}
	for _, t := range ts {
		p := t.Phase()
		if len(phases) == 0 && p != store.TenantDeleted || slices.Contains(phases, p) {
			out = append(out, t)
		}
	}
	return out
}

// ValidPhase reports whether p names a tenant phase.
func ValidPhase(p string) bool {
	return slices.Contains([]string{store.TenantActive, store.TenantSuspended, store.TenantDeleting, store.TenantDeleted}, p)
}
