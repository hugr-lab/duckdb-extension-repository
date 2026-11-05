package tenants

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0043: the channels' order, a strict chain per package type (nightly → staging → prod), for
// the console's display only: no placement, promotion, hold, grant or serving answer reads it.

// OrderTag is an order's ETag: a hash of its edges, by channel (a PUT's If-Match).
func OrderTag(edges []store.OrderEdge) string {
	h := sha256.New()
	for _, e := range edges {
		fmt.Fprintf(h, "%s\x00%s\n", e.Channel, e.Next)
	}
	return `"o` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// orderType is the package type an order is of: "" the default.
func orderType(packageType string) (string, error) {
	t, err := pkgtype.Parse(packageType)
	if err != nil {
		return "", fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	return t, nil
}

// mayReadOrder admits, for the order of a package type, the tenant's administrators and auditors
// (audit on the tenant, which admin implies) and the administrators of its channels of that type
// (admin on one of them): a channel's administrator of another type reads none of this type's.
func (s *Service) mayReadOrder(ctx context.Context, a authz.Actor, tenant, packageType string) error {
	err := s.Authz.Allow(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant})
	if !errors.Is(err, authz.ErrDenied) {
		return err
	}
	chs, lerr := s.Store.ListChannels(ctx, tenant)
	if lerr != nil {
		return lerr
	}
	for _, c := range chs {
		if c.PackageType != packageType {
			continue
		}
		cerr := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: c.Name})
		if !errors.Is(cerr, authz.ErrDenied) {
			return cerr
		}
	}
	return err
}

// ChannelOrder reads the order of a tenant's channels of a package type ("" the default), by
// channel: its administrators, its channels' administrators and its auditors.
func (s *Service) ChannelOrder(ctx context.Context, a authz.Actor, tenant, packageType string) ([]store.OrderEdge, error) {
	typ, err := orderType(packageType)
	if err != nil {
		return nil, err
	}
	if err := s.mayReadOrder(ctx, a, tenant, typ); err != nil {
		return nil, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return s.Store.ChannelOrder(ctx, t.ID, typ)
}

// SetChannelOrder replaces the order of a tenant's channels of a package type with edges (none
// clears it): the tenant's administrators only (it spans channels). expected, when not "", is the
// order's ETag as read (store.ErrConflict when it no longer matches). It runs under the tenant's row
// lock, which channels are created under (spec 0038), and writes channel.order when the order
// changed. It answers the order now in force.
func (s *Service) SetChannelOrder(ctx context.Context, a authz.Actor, tenant, packageType string, edges []store.OrderEdge, expected string) ([]store.OrderEdge, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return nil, err
	}
	typ, err := orderType(packageType)
	if err != nil {
		return nil, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var after []store.OrderEdge
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, nil, func(tx *store.Tx) error {
		if expected != "" {
			cur, err := tx.ChannelOrder(ctx, t.ID, typ)
			if err != nil {
				return err
			}
			if OrderTag(cur) != expected {
				return store.ErrConflict
			}
		}
		before, changed, err := tx.SetChannelOrder(ctx, t.ID, typ, edges, a.String())
		if err != nil {
			return err
		}
		if after = before; !changed {
			return nil
		}
		if after, err = tx.ChannelOrder(ctx, t.ID, typ); err != nil {
			return err
		}
		f := map[string]any{"before": nonNilEdges(before), "after": nonNilEdges(after)}
		if typ != pkgtype.Default {
			f["package_type"] = typ
		}
		return tx.Event(ctx, t.ID, a.String(), "channel.order", "tenant:"+t.Name, f)
	})
	return nonNilEdges(after), err
}

func nonNilEdges(e []store.OrderEdge) []store.OrderEdge {
	if e == nil {
		return []store.OrderEdge{}
	}
	return e
}
