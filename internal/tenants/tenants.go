// Package tenants manages tenants, channels and DuckDB versions (spec 0003).
package tenants

import (
	"context"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Service manages tenants, channels and DuckDB versions.
type Service struct {
	Store *store.Store
	Authz authz.Authorizer
}

// CreateTenant creates a tenant (server-wide action).
func (s *Service) CreateTenant(ctx context.Context, a authz.Actor, name, displayName string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, "", ""); err != nil {
		return store.Tenant{}, err
	}
	t := store.Tenant{Name: name, DisplayName: displayName}
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &t) })
	return t, err
}

// ListTenants lists tenants (server-wide action).
func (s *Service) ListTenants(ctx context.Context, a authz.Actor) ([]store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbRead, "", ""); err != nil {
		return nil, err
	}
	return s.Store.ListTenants(ctx)
}

// SetTenantState suspends or resumes a tenant (server-wide action).
func (s *Service) SetTenantState(ctx context.Context, a authz.Actor, name, state string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, "", ""); err != nil {
		return store.Tenant{}, err
	}
	if state != store.TenantActive && state != store.TenantSuspended {
		return store.Tenant{}, fmt.Errorf("%w: tenant state %q", store.ErrInvalid, state)
	}
	var t store.Tenant
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if t, err = tx.GetTenant(ctx, name); err != nil {
			return err
		}
		return tx.SetTenantState(ctx, &t, state)
	})
	return t, err
}

// AddVersion registers a DuckDB version (server-wide action). kind is release or dev; capi is the
// version's C API level, if known.
func (s *Service) AddVersion(ctx context.Context, a authz.Actor, name, kind, capi string) (store.DuckDBVersion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, "", ""); err != nil {
		return store.DuckDBVersion{}, err
	}
	if kind != "release" && kind != "dev" {
		return store.DuckDBVersion{}, fmt.Errorf("%w: version kind %q (release or dev)", store.ErrInvalid, kind)
	}
	v := store.DuckDBVersion{Name: name, Kind: kind, CAPIVersion: capi}
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.AddDuckDBVersion(ctx, &v) })
	return v, err
}

// ListVersions lists DuckDB versions.
func (s *Service) ListVersions(ctx context.Context, a authz.Actor) ([]store.DuckDBVersion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbRead, "", ""); err != nil {
		return nil, err
	}
	return s.Store.ListDuckDBVersions(ctx)
}

// CreateChannel creates a channel; its kind never changes afterwards.
func (s *Service) CreateChannel(ctx context.Context, a authz.Actor, tenant, name, kind string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, tenant, ""); err != nil {
		return store.Channel{}, err
	}
	if kind != store.ChannelSigned && kind != store.ChannelPassthrough {
		return store.Channel{}, fmt.Errorf("%w: channel kind %q (signed or passthrough)", store.ErrInvalid, kind)
	}
	c := store.Channel{Name: name, Kind: kind}
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		t, err := tx.GetTenant(ctx, tenant)
		if err != nil {
			return err
		}
		c.TenantID = t.ID
		return tx.CreateChannel(ctx, &c)
	})
	return c, err
}

// ListChannels lists a tenant's channels.
func (s *Service) ListChannels(ctx context.Context, a authz.Actor, tenant string) ([]store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbRead, tenant, ""); err != nil {
		return nil, err
	}
	if _, err := s.Store.GetTenant(ctx, tenant); err != nil {
		return nil, err
	}
	return s.Store.ListChannels(ctx, tenant)
}

// SetChannelVersions adds and removes the DuckDB versions a channel serves, under the channel lock,
// and bumps the channel's version.
func (s *Service) SetChannelVersions(ctx context.Context, a authz.Actor, tenant, channel string, add, remove []string) ([]string, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, tenant, channel); err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	err = s.Store.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
		c, err := tx.GetChannel(ctx, tenant, channel)
		if err != nil {
			return err
		}
		for _, name := range add {
			v, err := tx.GetDuckDBVersion(ctx, name)
			if err != nil {
				return err
			}
			if err := tx.AddChannelVersion(ctx, c.ID, v.ID); err != nil {
				return err
			}
		}
		for _, name := range remove {
			v, err := tx.GetDuckDBVersion(ctx, name)
			if err != nil {
				return err
			}
			if err := tx.RemoveChannelVersion(ctx, c.ID, v.ID); err != nil {
				return err
			}
		}
		return tx.BumpChannel(ctx, &c)
	})
	if err != nil {
		return nil, err
	}
	return s.Store.ChannelVersions(ctx, ch.ID)
}
