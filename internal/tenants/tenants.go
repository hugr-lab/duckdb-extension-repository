// Package tenants manages tenants, channels and DuckDB versions (spec 0003).
package tenants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Service manages tenants, channels and DuckDB versions.
type Service struct {
	Store *store.Store
	Authz authz.Authorizer
	// HasDomain reports whether a storage domain is configured (spec 0005).
	HasDomain func(string) bool
}

// CreateTenant creates a tenant (server-wide action) in a storage domain (empty: the default one),
// which must be configured and never changes.
func (s *Service) CreateTenant(ctx context.Context, a authz.Actor, name, displayName, domain string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, err
	}
	if domain == "" {
		domain = store.DefaultDomain
	}
	if s.HasDomain == nil || !s.HasDomain(domain) {
		return store.Tenant{}, fmt.Errorf("%w: storage domain %q is not configured", store.ErrInvalid, domain)
	}
	t := store.Tenant{Name: name, DisplayName: displayName, StorageDomain: domain}
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &t) })
	return t, err
}

// ListTenants lists tenants (server-wide action).
func (s *Service) ListTenants(ctx context.Context, a authz.Actor) ([]store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return nil, err
	}
	return s.Store.ListTenants(ctx)
}

// GetTenant reads a tenant (its administrators may read it).
func (s *Service) GetTenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// SetTenantState suspends or resumes a tenant (server-wide action). A non-zero expected version must
// be the tenant's (store.ErrConflict otherwise).
func (s *Service) SetTenantState(ctx context.Context, a authz.Actor, name, state string, expected int64) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
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
		if expected != 0 && t.Version != expected {
			return store.ErrConflict
		}
		return tx.SetTenantState(ctx, &t, state)
	})
	return t, err
}

// AddVersion registers a DuckDB version (server-wide action). kind is release or dev; capis are
// the maximum C API version it accepts for each major (spec 0006: v1.5.6 and v2.0.0 on the pin).
func (s *Service) AddVersion(ctx context.Context, a authz.Actor, name, kind string, capis []string) (store.DuckDBVersion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.DuckDBVersion{}, err
	}
	if kind != "release" && kind != "dev" {
		return store.DuckDBVersion{}, fmt.Errorf("%w: version kind %q (release or dev)", store.ErrInvalid, kind)
	}
	parsed, err := parseCAPIs(capis)
	if err != nil {
		return store.DuckDBVersion{}, err
	}
	v := store.DuckDBVersion{Name: name, Kind: kind, CAPIs: parsed}
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.AddDuckDBVersion(ctx, &v); err != nil {
			return err
		}
		for _, c := range parsed {
			if err := tx.AddCAPI(ctx, v.ID, c); err != nil {
				return err
			}
		}
		return nil
	})
	return v, err
}

func parseCAPIs(capis []string) ([]store.CAPI, error) {
	var out []store.CAPI
	seen := map[int]bool{}
	for _, s := range capis {
		c, err := store.ParseCAPI(s)
		if err != nil {
			return nil, err
		}
		if seen[c.Major] {
			return nil, fmt.Errorf("%w: C API major %d given twice", store.ErrInvalid, c.Major)
		}
		seen[c.Major] = true
		out = append(out, c)
	}
	return out, nil
}

// AddVersionCAPI records the maximum C API of one more major for a DuckDB version (server-wide
// action). Existing majors are facts about the engine and never change.
func (s *Service) AddVersionCAPI(ctx context.Context, a authz.Actor, name, capi string) (store.DuckDBVersion, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.DuckDBVersion{}, err
	}
	c, err := store.ParseCAPI(capi)
	if err != nil {
		return store.DuckDBVersion{}, err
	}
	var v store.DuckDBVersion
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if v, err = tx.GetDuckDBVersion(ctx, name); err != nil {
			return err
		}
		// a version added before migration 0003 has only the legacy single value: keep it as a row,
		// or adding a second major would make its readers drop it
		rows, err := tx.VersionCAPIs(ctx, v.ID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			if legacy, err := store.ParseCAPI(strings.TrimSpace(v.CAPIVersion)); err == nil && legacy.Major != c.Major {
				if err := tx.AddCAPI(ctx, v.ID, legacy); err != nil {
					return err
				}
			}
		}
		if err := tx.AddCAPI(ctx, v.ID, c); err != nil {
			return err
		}
		if v.CAPIs, err = tx.VersionCAPIs(ctx, v.ID); err != nil {
			return err
		}
		return tx.BumpChannelsServing(ctx, v.ID)
	})
	return v, err
}

// ListVersions lists DuckDB versions (public: /api/v1/duckdb-versions).
func (s *Service) ListVersions(ctx context.Context) ([]store.DuckDBVersion, error) {
	return s.Store.ListDuckDBVersions(ctx)
}

// CreateChannel creates a channel; its kind never changes afterwards.
func (s *Service) CreateChannel(ctx context.Context, a authz.Actor, tenant, name, kind string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
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
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return nil, err
	}
	if _, err := s.Store.GetTenant(ctx, tenant); err != nil {
		return nil, err
	}
	return s.Store.ListChannels(ctx, tenant)
}

// ChannelVersions lists the DuckDB versions a channel serves.
func (s *Service) ChannelVersions(ctx context.Context, a authz.Actor, tenant, channel string) ([]string, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ChannelVersions(ctx, ch.ID)
}

// SetChannelVersions adds and removes the DuckDB versions a channel serves, under the channel lock,
// and bumps the channel's version.
func (s *Service) SetChannelVersions(ctx context.Context, a authz.Actor, tenant, channel string, add, remove []string) ([]string, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); err != nil {
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
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: DuckDB version %q is not registered", store.ErrInvalid, name)
			}
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
