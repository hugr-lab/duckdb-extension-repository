// Package tenants manages tenants, channels and DuckDB versions (spec 0003).
package tenants

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
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
// which must be configured and never changes, with the holder its licences must name (spec 0028;
// empty: none).
func (s *Service) CreateTenant(ctx context.Context, a authz.Actor, name, displayName, domain, holderID string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, err
	}
	if domain == "" {
		domain = store.DefaultDomain
	}
	if s.HasDomain == nil || !s.HasDomain(domain) {
		return store.Tenant{}, fmt.Errorf("%w: storage domain %q is not configured", store.ErrInvalid, domain)
	}
	t := store.Tenant{Name: name, DisplayName: displayName, StorageDomain: domain, HolderID: holderID}
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &t); err != nil {
			return err
		}
		fields := map[string]any{"name": t.Name, "display_name": t.DisplayName, "storage_domain": t.StorageDomain}
		if t.HolderID != "" {
			fields["holder_id"] = t.HolderID
		}
		return tx.Event(ctx, "", a.String(), "tenant.create", "tenant:"+t.Name, fields)
	})
	if errors.Is(err, store.ErrExists) {
		// spec 0019: a tombstone keeps its name for good
		if old, gerr := s.Store.GetTenantAny(ctx, name); gerr == nil && !old.DeletingAt.IsZero() {
			return t, &Refusal{"the name belonged to a deleted tenant and is never reused"}
		}
	}
	return t, err
}

// ListTenants lists tenants in the given phases, every one but deleted when none is given
// (server-wide action).
func (s *Service) ListTenants(ctx context.Context, a authz.Actor, phases ...string) ([]store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return nil, err
	}
	for _, p := range phases {
		if !ValidPhase(p) {
			return nil, fmt.Errorf("%w: tenant state %q (active, suspended, deleting or deleted)", store.ErrInvalid, p)
		}
	}
	ts, err := s.Store.ListTenants(ctx)
	return Phases(ts, phases...), err
}

// GetTenant reads a tenant (its administrators may read it).
func (s *Service) GetTenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// TenantState is a tenant as a distribution admin reads it (spec 0037), so a distributor can check
// that the tenant is its customer's before it writes there: active or suspended (the role is
// server-wide), never one whose deletion started (not found). Each read is a tenant.read event in the
// tenant, a suspended one's too.
func (s *Service) TenantState(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if a.Kind != authz.ActorDistributionAdmin {
		return store.Tenant{}, authz.ErrDenied
	}
	// distribute on the server, not on the tenant: the tenant's resource is an active tenant only
	// (authz's activeTenant), and a suspended one is read too
	if err := s.Authz.Allow(ctx, a, authz.VerbDistribute, authz.Server); err != nil {
		return store.Tenant{}, err
	}
	// a closed tenant is not found (and still a deleting one, for the callers that tell)
	gone := func(err error) (store.Tenant, error) {
		return store.Tenant{}, fmt.Errorf("%w: tenant %s: %w", store.ErrNotFound, name, err)
	}
	t, err := s.Store.GetTenant(ctx, name)
	switch {
	case errors.Is(err, store.ErrDeleting) || errors.Is(err, store.ErrDeleted):
		return gone(err)
	case err != nil:
		return store.Tenant{}, err
	}
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		return tx.Event(ctx, t.ID, a.String(), "tenant.read", "tenant:"+t.Name, map[string]any{"state": t.State})
	})
	if errors.Is(err, store.ErrDeleting) { // its deletion started meanwhile
		return gone(err)
	}
	return t, err
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
		if t, err = tx.GetTenantForUpdate(ctx, name); err != nil {
			return err // a deleting tenant is refused: never resumed (spec 0019)
		}
		if expected != 0 && t.Version != expected {
			return store.ErrConflict
		}
		if t.State == state {
			return nil // suspending a suspended tenant keeps when it was suspended
		}
		if err := tx.SetTenantState(ctx, &t, state); err != nil {
			return err
		}
		kind := audit.Kind("tenant.resume")
		if state == store.TenantSuspended {
			kind = "tenant.suspend"
		}
		return tx.Event(ctx, t.ID, a.String(), kind, "tenant:"+t.Name, nil)
	})
	return t, err
}

// AddVersion registers a DuckDB version (server-wide action). kind is release or dev; capis are
// the maximum C API version it accepts for each major (spec 0006: v1.5.6 and v2.0.0 on the pin).
// CoreExtensions are a DuckDB version's core extension names, and the commit of DuckDB's source
// they were read at ("" when the caller does not say) (spec 0030). Force reserves names tenants have
// releases under, as an administrator's reservation does (Reserve).
type CoreExtensions struct {
	Names  []string
	Commit string
	Force  bool
}

var (
	coreNameRE   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	coreCommitRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

func (c CoreExtensions) check() error {
	if len(c.Names) == 0 {
		return fmt.Errorf("%w: core_extensions: the version's core extension names (kista duckdb core-extensions <ref>)", store.ErrInvalid)
	}
	for _, n := range c.Names {
		if !coreNameRE.MatchString(n) {
			return fmt.Errorf("%w: core_extensions: %q is not an extension name", store.ErrInvalid, n)
		}
	}
	if c.Commit != "" && !coreCommitRE.MatchString(c.Commit) {
		return fmt.Errorf("%w: core_extensions_commit: %q is not a commit", store.ErrInvalid, c.Commit)
	}
	return nil
}

// reserveCore reserves a version's core names (spec 0030) and returns those that were new.
func reserveCore(ctx context.Context, tx *store.Tx, actor, version string, c CoreExtensions) ([]string, error) {
	src := "version:" + version
	if c.Commit != "" {
		src += "@" + c.Commit
	}
	names := slices.Clone(c.Names)
	slices.Sort(names)
	var added []string
	for _, n := range slices.Compact(names) {
		changed, err := tx.ReserveName(ctx, pkgtype.DuckDBExtension, n, store.ReservedCore, src) // DuckDB's core names
		if err != nil {
			return nil, err
		}
		if changed {
			added = append(added, n)
		}
	}
	if len(added) > 0 {
		if err := tx.BumpReservedVersion(ctx); err != nil {
			return nil, err
		}
		if err := tx.Event(ctx, "", actor, "reserved.add", "version:"+version, map[string]any{"version": version, "kind": store.ReservedCore,
			"names": added, "source": src}); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// AddVersion registers a DuckDB version with its C APIs and its core extensions, which it reserves
// (spec 0030); it returns the names that were not reserved before.
func (s *Service) AddVersion(ctx context.Context, a authz.Actor, name, kind string, capis []string, core CoreExtensions) (store.DuckDBVersion, []string, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.DuckDBVersion{}, nil, err
	}
	if kind != "release" && kind != "dev" {
		return store.DuckDBVersion{}, nil, fmt.Errorf("%w: version kind %q (release or dev)", store.ErrInvalid, kind)
	}
	parsed, err := parseCAPIs(capis)
	if err != nil {
		return store.DuckDBVersion{}, nil, err
	}
	if err := core.check(); err != nil {
		return store.DuckDBVersion{}, nil, err
	}
	if err := s.refuseHeld(ctx, core.Names, store.ReservedCore, core.Force); err != nil {
		return store.DuckDBVersion{}, nil, err
	}
	v := store.DuckDBVersion{Name: name, Kind: kind, CAPIs: parsed}
	var added []string
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.AddDuckDBVersion(ctx, &v); err != nil {
			return err
		}
		for _, c := range parsed {
			if err := tx.AddCAPI(ctx, v.ID, c); err != nil {
				return err
			}
		}
		if err := tx.Event(ctx, "", a.String(), "version.add", "version:"+v.Name, map[string]any{"version": v.Name, "kind": kind, "c_apis": capis}); err != nil {
			return err
		}
		added, err = reserveCore(ctx, tx, a.String(), v.Name, core)
		return err
	})
	return v, added, err
}

// AddVersionCore reserves more core extensions of a registered version (spec 0030): those its
// registration did not name, or a version registered before core names were taken. It returns the
// names that were not reserved before.
func (s *Service) AddVersionCore(ctx context.Context, a authz.Actor, name string, core CoreExtensions) ([]string, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return nil, err
	}
	if err := core.check(); err != nil {
		return nil, err
	}
	if err := s.refuseHeld(ctx, core.Names, store.ReservedCore, core.Force); err != nil {
		return nil, err
	}
	var added []string
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if _, err := tx.GetDuckDBVersion(ctx, name); err != nil {
			return err
		}
		var err error
		added, err = reserveCore(ctx, tx, a.String(), name, core)
		return err
	})
	return added, err
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
		if err := tx.Event(ctx, "", a.String(), "version.c_apis", "version:"+v.Name, map[string]any{"version": v.Name, "c_api": capi}); err != nil {
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
func (s *Service) CreateChannel(ctx context.Context, a authz.Actor, tenant, name, kind, packageType string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Channel{}, err
	}
	if kind != store.ChannelSigned && kind != store.ChannelPassthrough {
		return store.Channel{}, fmt.Errorf("%w: channel kind %q (signed or passthrough)", store.ErrInvalid, kind)
	}
	// spec 0034: a channel serves one package type, set here and never changed (default
	// duckdb_extension; a passthrough channel's is that)
	packageType, err := pkgtype.Parse(packageType)
	if err != nil {
		return store.Channel{}, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	c := store.Channel{Name: name, Kind: kind, PackageType: packageType}
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		t, err := tx.GetTenant(ctx, tenant)
		if err != nil {
			return err
		}
		c.TenantID = t.ID
		if err := tx.CreateChannel(ctx, &c); err != nil {
			return err
		}
		f := map[string]any{"name": c.Name, "kind": c.Kind}
		if c.PackageType != pkgtype.Default {
			f["package_type"] = c.PackageType
		}
		return tx.Event(ctx, t.ID, a.String(), "channel.create", "channel:"+c.Name, f)
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
	chs, err := s.Store.ListChannels(ctx, tenant)
	return store.KnownChannels(chs), err // spec 0034: a type this binary does not know is read as nothing
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
		if len(add) > 0 { // the channel's mirrors fetch the new versions now (spec 0009)
			if err := tx.MakeDue(ctx, "", c.ID); err != nil {
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
		if err := tx.Event(ctx, c.TenantID, a.String(), "channel.versions", "channel:"+c.Name,
			map[string]any{"added": add, "removed": remove}); err != nil {
			return err
		}
		return tx.BumpChannel(ctx, &c)
	})
	if err != nil {
		return nil, err
	}
	return s.Store.ChannelVersions(ctx, ch.ID)
}
