package tenants

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Extension groups (spec 0017): a tenant administrator creates them and changes their members; an
// administrator of a group (admin granted on it) reads it.

// ErrGroupGranted refuses removing an extension group that a grant names: revoke its grants first.
var ErrGroupGranted = errors.New("tenants: a grant names the extension group: revoke its grants first")

// MaxMemberChange is the most names one member change adds and removes.
const MaxMemberChange = 100

// allowed asks the authorizer: a refusal is false, any other error an error.
func (s *AuthAdmin) allowed(ctx context.Context, a authz.Actor, r authz.Resource) (bool, error) {
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, r)
	if errors.Is(err, authz.ErrDenied) {
		return false, nil
	}
	return err == nil, err
}

// readGroup authorizes reading a group: a tenant administrator, or admin on the group. It answers
// whether the caller administers the tenant (who sees grant counts).
func (s *AuthAdmin) readGroup(ctx context.Context, a authz.Actor, tenant, name string) (store.Tenant, bool, error) {
	if err := store.ValidGroupName(name); err != nil {
		return store.Tenant{}, false, fmt.Errorf("%w: extension group %s", store.ErrNotFound, name) // no group has it: missing
	}
	tenantAdmin, err := s.allowed(ctx, a, authz.Resource{Tenant: tenant})
	if err != nil {
		return store.Tenant{}, false, err
	}
	if !tenantAdmin {
		if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, ExtensionGroup: name}); err != nil {
			return store.Tenant{}, false, err
		}
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	return t, tenantAdmin, err
}

// holder gives what an actor holds in a tenant, read once (authz.Grants).
type holder interface {
	Held(ctx context.Context, a authz.Actor, tenant string) (auth.Held, error)
}

// CreateExtensionGroup adds an extension group.
func (s *AuthAdmin) CreateExtensionGroup(ctx context.Context, a authz.Actor, tenant, name, description string) (store.ExtensionGroup, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.ExtensionGroup{}, err
	}
	g := store.ExtensionGroup{TenantID: t.ID, Name: name, Description: description, CreatedBy: a.String()}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if err := tx.InsertExtensionGroup(ctx, &g); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.add", "extension-group:"+g.Name,
			map[string]any{"name": g.Name, "description": g.Description})
	})
	return g, err
}

// ListExtensionGroups lists the groups the caller may read, by name: every group for a tenant
// administrator, those it holds admin on otherwise. extension (a tenant administrator's) keeps the
// groups holding that name. tenantAdmin says whether the caller administers the tenant.
func (s *AuthAdmin) ListExtensionGroups(ctx context.Context, a authz.Actor, tenant, extension string) (gs []store.ExtensionGroup, tenantAdmin bool, err error) {
	if tenantAdmin, err = s.allowed(ctx, a, authz.Resource{Tenant: tenant}); err != nil {
		return nil, false, err
	}
	if extension != "" {
		if !tenantAdmin {
			return nil, false, authz.ErrDenied
		}
		if err := release.ValidName(extension); err != nil {
			return nil, false, err
		}
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, false, authz.ErrDenied
		}
		return nil, false, err
	}
	all, err := s.Store.ListExtensionGroups(ctx, t.ID, extension)
	if err != nil || tenantAdmin {
		return all, tenantAdmin, err
	}
	h, ok := s.Authz.(holder)
	if !ok {
		return nil, false, authz.ErrDenied
	}
	held, err := h.Held(ctx, a, tenant)
	if err != nil {
		return nil, false, err
	}
	for _, g := range all {
		if authz.Covers(held, store.VerbAdmin, authz.Resource{Tenant: tenant, ExtensionGroup: g.Name}) {
			gs = append(gs, g)
		}
	}
	if len(gs) == 0 {
		return nil, false, authz.ErrDenied // no group to read: as any management route
	}
	return gs, false, nil
}

// GetExtensionGroup reads a group (tenantAdmin as in ListExtensionGroups).
func (s *AuthAdmin) GetExtensionGroup(ctx context.Context, a authz.Actor, tenant, name string) (g store.ExtensionGroup, tenantAdmin bool, err error) {
	t, tenantAdmin, err := s.readGroup(ctx, a, tenant, name)
	if err != nil {
		return g, false, err
	}
	g, err = s.Store.GetExtensionGroup(ctx, t.ID, name)
	return g, tenantAdmin, err
}

// ExtensionGroupMembers lists a group's members by name.
func (s *AuthAdmin) ExtensionGroupMembers(ctx context.Context, a authz.Actor, tenant, name string) ([]store.GroupMember, error) {
	t, _, err := s.readGroup(ctx, a, tenant, name)
	if err != nil {
		return nil, err
	}
	g, err := s.Store.GetExtensionGroup(ctx, t.ID, name)
	if err != nil {
		return nil, err
	}
	return s.Store.GroupMembers(ctx, g.ID)
}

// SetExtensionGroupDescription changes a group's description (expected: its version, 0 for any).
func (s *AuthAdmin) SetExtensionGroupDescription(ctx context.Context, a authz.Actor, tenant, name, description string, expected int64) (store.ExtensionGroup, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.ExtensionGroup{}, err
	}
	var g store.ExtensionGroup
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if g, err = tx.GetExtensionGroup(ctx, t.ID, name); err != nil {
			return err
		}
		old, version := g.Description, g.Version
		if err := tx.SetGroupDescription(ctx, &g, description, expected); err != nil || g.Version == version {
			return err // a stale version, or no change: no event
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.change", "extension-group:"+g.Name,
			map[string]any{"description": map[string]string{"old": old, "new": description}})
	})
	return g, err
}

// RemoveExtensionGroup removes a group and its members (expected: its version, 0 for any). A group
// that a grant names is refused: removing it would take rights silently. The members leave first,
// in events of at most MaxMemberChange names, so the log keeps them.
func (s *AuthAdmin) RemoveExtensionGroup(ctx context.Context, a authz.Actor, tenant, name string, expected int64) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		g, err := tx.GetExtensionGroup(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if granted, err := tx.GroupGranted(ctx, g); err != nil {
			return err
		} else if granted {
			return ErrGroupGranted
		}
		ms, err := tx.GroupMembers(ctx, g.ID)
		if err != nil {
			return err
		}
		for i := 0; i < len(ms); i += MaxMemberChange {
			var names []string
			for _, m := range ms[i:min(i+MaxMemberChange, len(ms))] {
				names = append(names, m.Name)
			}
			if err := tx.Event(ctx, t.ID, a.String(), "extension_group.members", "extension-group:"+g.Name,
				map[string]any{"added": []string{}, "removed": names}); err != nil {
				return err
			}
		}
		if err := tx.DeleteExtensionGroup(ctx, g, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.remove", "extension-group:"+g.Name,
			map[string]any{"name": g.Name, "description": g.Description, "members": len(ms)})
	})
}

// ChangeExtensionGroupMembers adds and removes names (together 1 to MaxMemberChange), skipping
// names already present or not members, and answers what changed. Only a tenant administrator
// changes members: a group's administrator would widen their own rights.
func (s *AuthAdmin) ChangeExtensionGroupMembers(ctx context.Context, a authz.Actor, tenant, name string, add, remove []string) (added, removed []string, err error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, nil, err
	}
	if n := len(add) + len(remove); n == 0 || n > MaxMemberChange {
		return nil, nil, fmt.Errorf("%w: a member change names 1 to %d extensions", store.ErrInvalid, MaxMemberChange)
	}
	for _, n := range slices.Concat(add, remove) {
		if err := release.ValidName(n); err != nil {
			return nil, nil, err
		}
		if slices.Contains(add, n) && slices.Contains(remove, n) {
			return nil, nil, fmt.Errorf("%w: %s is both added and removed", store.ErrInvalid, n)
		}
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		g, err := tx.GetExtensionGroup(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if added, removed, err = tx.ChangeGroupMembers(ctx, g, add, remove, a.String()); err != nil {
			return err
		}
		if len(added)+len(removed) == 0 {
			return nil
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.members", "extension-group:"+g.Name,
			map[string]any{"added": nonNilNames(added), "removed": nonNilNames(removed)})
	})
	return added, removed, err
}

func nonNilNames(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
