package tenants

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// maxNamed is how many licences a refusal to set the holder names.
const maxNamed = 20

// SetTenantHolder sets the holder a tenant's licences must name (spec 0028), or clears it (holder
// ""): a server-wide action, so a tenant administrator, who may read it, cannot clear it. Setting it
// is refused while the tenant holds a licence of another holder: nothing is removed silently. A
// non-zero expected version must be the tenant's (store.ErrConflict otherwise).
func (s *Service) SetTenantHolder(ctx context.Context, a authz.Actor, name, holder string, expected int64) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, err
	}
	if holder != "" {
		if err := store.ValidHolderID(holder); err != nil {
			return store.Tenant{}, err
		}
	}
	t, err := s.Store.GetTenant(ctx, name)
	if err != nil {
		return t, err
	}
	// the licence lock first, then the tenant's row, as every licence write takes them: a write
	// under way finishes first and is seen here; one after it sees the new holder
	err = s.Store.InTxLocks(ctx, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		cur, err := tx.GetTenantForUpdate(ctx, name)
		if err != nil {
			return err
		}
		if cur.ID != t.ID {
			return store.ErrConflict // another tenant of the name: not possible, names are never reused
		}
		t = cur
		if expected != 0 && t.Version != expected {
			return store.ErrConflict
		}
		if t.HolderID == holder {
			return nil
		}
		if holder != "" {
			others, err := tx.OtherHolderLicenses(ctx, t.ID, holder)
			if err != nil {
				return err
			}
			if len(others) > 0 {
				return &Refusal{otherHolders(others)}
			}
		}
		old := t.HolderID
		if err := tx.SetTenantHolder(ctx, &t, holder); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "tenant.holder", "tenant:"+t.Name, map[string]any{"old": old, "new": holder})
	})
	return t, err
}

// otherHolders words the refusal to set the holder over licences of other holders.
func otherHolders(ls []store.License) string {
	var b strings.Builder
	b.WriteString("the tenant holds licences of another holder; remove them first:")
	for i, l := range ls {
		if i == maxNamed {
			fmt.Fprintf(&b, ", and %d more", len(ls)-maxNamed)
			break
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " %s (%s, holder %q)", l.Name, l.LicenseID, l.HolderID)
	}
	return b.String()
}
