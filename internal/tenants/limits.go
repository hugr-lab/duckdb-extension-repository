package tenants

import (
	"context"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0038: a tenant's limits, which a server administrator sets and its administrators read.

// Limits are a tenant's limits as they are read: its own stored values (store.LimitDefault where
// the server's default applies), the values in force, and what each limit counts.
type Limits struct {
	Set, Limit, Used store.Limits
}

// LimitsTag is the ETag of a tenant's stored limits: a PUT's If-Match compares it.
func LimitsTag(l store.Limits) string {
	return fmt.Sprintf(`"l%d.%d.%d.%d"`, l.Bytes, l.Channels, l.Upstreams, l.Publishers)
}

// TenantLimits reads a tenant's limits (its administrators and server administrators; a suspended
// tenant's too).
func (s *Service) TenantLimits(ctx context.Context, a authz.Actor, name string) (store.Tenant, Limits, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, Limits{}, err
	}
	t, err := s.Store.GetTenant(ctx, name)
	if err != nil {
		return t, Limits{}, err
	}
	l, err := s.limitsOf(ctx, t)
	return t, l, err
}

func (s *Service) limitsOf(ctx context.Context, t store.Tenant) (Limits, error) {
	used, err := s.Store.TenantUsage(ctx, t.ID)
	return Limits{Set: t.Limits, Limit: s.Store.Effective(t.Limits), Used: used}, err
}

// SetTenantLimits stores a tenant's own values of the limits named in set (store.LimitDefault
// clears one, store.LimitUnlimited lifts it): a server-wide action, a suspended tenant's too. A
// non-empty expected tag must be the stored limits' (store.ErrConflict otherwise). A limit left out
// is unchanged, and nothing that exists is removed when a limit is lowered; a change that changes
// nothing writes nothing.
func (s *Service) SetTenantLimits(ctx context.Context, a authz.Actor, name string, set map[string]int64, expected string) (store.Tenant, Limits, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, Limits{}, err
	}
	t, err := s.Store.GetTenant(ctx, name) // the values are checked where they are stored
	if err != nil {
		return t, Limits{}, err
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, nil, func(tx *store.Tx) error {
		cur, err := tx.GetTenantForUpdate(ctx, name)
		if err != nil {
			return err
		}
		if expected != "" && LimitsTag(cur.Limits) != expected {
			return store.ErrConflict
		}
		t = cur
		return s.setLimits(ctx, tx, a, &t, set, t.ID)
	})
	if err != nil {
		return t, Limits{}, err
	}
	l, err := s.limitsOf(ctx, t)
	return t, l, err
}

// setLimits writes the limits of set that change, and their event (tenant.limits): for each, the
// stored value before and after and the value in force after; in the tenant's events (eventTenant), or
// the server's ("") beside its creation.
func (s *Service) setLimits(ctx context.Context, tx *store.Tx, a authz.Actor, t *store.Tenant, set map[string]int64, eventTenant string) error {
	old := t.Limits
	changed := map[string]int64{}
	for n, v := range set {
		if !slices.Contains(store.LimitNames, n) {
			return fmt.Errorf("%w: unknown limit %q", store.ErrInvalid, n)
		}
		if old.Get(n) == v {
			continue // a value stored as it is (perhaps above a ceiling lowered since: it is capped) is left
		}
		if err := store.ValidLimit(n, v); err != nil {
			return err
		}
		changed[n] = v
	}
	if len(changed) == 0 {
		return nil
	}
	if err := tx.SetTenantLimits(ctx, t, changed); err != nil {
		return err
	}
	in := s.Store.Effective(t.Limits)
	f := map[string]any{}
	for n := range changed {
		f[n] = map[string]any{"old": LimitValue(old.Get(n)), "new": LimitValue(t.Limits.Get(n)), "limit": LimitInForce(in.Get(n))}
	}
	return tx.Event(ctx, eventTenant, a.String(), "tenant.limits", "tenant:"+t.Name, f)
}

// LimitValue is a stored value as answers and events show it: a number, "unlimited" or "default".
func LimitValue(v int64) any {
	switch v {
	case store.LimitDefault:
		return "default"
	case store.LimitUnlimited:
		return "unlimited"
	}
	return v
}

// LimitInForce is a value in force as answers and events show it: a number, or nil for unlimited
// bytes.
func LimitInForce(v int64) any {
	if v < 0 {
		return nil
	}
	return v
}
