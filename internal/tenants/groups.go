package tenants

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
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
		if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, PackageGroup: name}); err != nil {
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

// CreatePackageGroup adds an extension group.
func (s *AuthAdmin) CreatePackageGroup(ctx context.Context, a authz.Actor, tenant, name, description string) (store.PackageGroup, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.PackageGroup{}, err
	}
	g := store.PackageGroup{TenantID: t.ID, Name: name, Description: description, CreatedBy: a.String()}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if err := tx.InsertPackageGroup(ctx, &g); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.add", "extension-group:"+g.Name,
			map[string]any{"name": g.Name, "description": g.Description})
	})
	return g, err
}

// ListPackageGroups lists the groups the caller may read, by name: every group for a tenant
// administrator, those it holds admin on otherwise. extension (a tenant administrator's) keeps the
// groups holding that package, of packageType ("": the default). tenantAdmin says whether the
// caller administers the tenant.
func (s *AuthAdmin) ListPackageGroups(ctx context.Context, a authz.Actor, tenant, packageType, extension string) (gs []store.PackageGroup, tenantAdmin bool, err error) {
	if packageType == "" {
		packageType = pkgtype.Default
	}
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
	all, err := s.Store.ListPackageGroups(ctx, t.ID, packageType, extension)
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
		if authz.Covers(held, store.VerbAdmin, authz.Resource{Tenant: tenant, PackageGroup: g.Name}) {
			gs = append(gs, g)
		}
	}
	if len(gs) == 0 {
		return nil, false, authz.ErrDenied // no group to read: as any management route
	}
	return gs, false, nil
}

// GetPackageGroup reads a group (tenantAdmin as in ListPackageGroups).
func (s *AuthAdmin) GetPackageGroup(ctx context.Context, a authz.Actor, tenant, name string) (g store.PackageGroup, tenantAdmin bool, err error) {
	t, tenantAdmin, err := s.readGroup(ctx, a, tenant, name)
	if err != nil {
		return g, false, err
	}
	g, err = s.Store.GetPackageGroup(ctx, t.ID, name)
	return g, tenantAdmin, err
}

// PackageGroupMembers lists a group's members by name.
func (s *AuthAdmin) PackageGroupMembers(ctx context.Context, a authz.Actor, tenant, name string) ([]store.GroupMember, error) {
	t, _, err := s.readGroup(ctx, a, tenant, name)
	if err != nil {
		return nil, err
	}
	g, err := s.Store.GetPackageGroup(ctx, t.ID, name)
	if err != nil {
		return nil, err
	}
	return s.Store.GroupMembers(ctx, g.ID)
}

// SetPackageGroupDescription changes a group's description (expected: its version, 0 for any).
func (s *AuthAdmin) SetPackageGroupDescription(ctx context.Context, a authz.Actor, tenant, name, description string, expected int64) (store.PackageGroup, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.PackageGroup{}, err
	}
	var g store.PackageGroup
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if g, err = tx.GetPackageGroup(ctx, t.ID, name); err != nil {
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

// RemovePackageGroup removes a group and its members (expected: its version, 0 for any). A group
// that a grant names is refused: removing it would take rights silently. The members leave first,
// in events of at most MaxMemberChange names, so the log keeps them.
func (s *AuthAdmin) RemovePackageGroup(ctx context.Context, a authz.Actor, tenant, name string, expected int64) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		g, err := tx.GetPackageGroup(ctx, t.ID, name)
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
		if err := tx.DeletePackageGroup(ctx, g, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.remove", "extension-group:"+g.Name,
			map[string]any{"name": g.Name, "description": g.Description, "members": len(ms)})
	})
}

// ChangePackageGroupMembers adds and removes names of one package type ("": the default; spec 0034:
// members are (type, name)), together 1 to MaxMemberChange, skipping names already present or not
// members, and answers what changed. Only a tenant administrator changes members: a group's
// administrator would widen their own rights.
func (s *AuthAdmin) ChangePackageGroupMembers(ctx context.Context, a authz.Actor, tenant, name, packageType string, add, remove []string) (added, removed []string, err error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, nil, err
	}
	if packageType, err = pkgtype.Parse(packageType); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
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
	ta, err := s.Store.GetTenantAuth(ctx, t.ID) // the group's publisher grants (grants never change)
	if err != nil {
		return nil, nil, err
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		g, err := tx.GetPackageGroup(ctx, t.ID, name)
		if err != nil {
			return err
		}
		// spec 0021: the members widen or narrow a marked publisher's grants on the group, which
		// are the server administrator's
		for _, gr := range ta.GroupGrants {
			if gr.PackageGroupID != g.ID || gr.PublisherName == "" {
				continue
			}
			p, err := tx.GetPublisher(ctx, t.ID, gr.PublisherName)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := s.marked(ctx, a, p); err != nil {
				return err
			}
		}
		keys := func(ns []string) []pkgtype.Key {
			out := make([]pkgtype.Key, len(ns))
			for i, n := range ns {
				out[i] = pkgtype.Key{Type: packageType, Name: n}
			}
			return out
		}
		names := func(ks []pkgtype.Key) []string {
			var out []string
			for _, k := range ks {
				out = append(out, k.Name)
			}
			return out
		}
		ka, kr, err := tx.ChangeGroupMembers(ctx, g, keys(add), keys(remove), a.String())
		if err != nil {
			return err
		}
		added, removed = names(ka), names(kr)
		if len(added)+len(removed) == 0 {
			return nil
		}
		f := map[string]any{"added": nonNilNames(added), "removed": nonNilNames(removed)}
		if packageType != pkgtype.Default { // spec 0034: when it is not the default
			f["package_type"] = packageType
		}
		return tx.Event(ctx, t.ID, a.String(), "extension_group.members", "extension-group:"+g.Name, f)
	})
	return added, removed, err
}

func nonNilNames(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
