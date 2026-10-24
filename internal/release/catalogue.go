package release

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a tenant's catalogue as its administrators read it.

// CatalogueEntry is a name from a source in a tenant's catalogue: its versions (newest first), and
// the channels whose rule for the name takes the source, with the rule's mode.
type CatalogueEntry struct {
	Source, Name string
	Versions     []string
	Rules        map[string]string // channel → mode
	Held         []string          // the versions with a held build (spec 0033 phase A2), newest first
	Revoked      []string          // the versions with a revoked build (spec 0033 phase A2), newest first
}

// CatalogueBuild is a build of an entry: what it is, how it came, and where it is released.
type CatalogueBuild struct {
	store.Build
	Placed []store.Placed
}

// CatalogueVersion is a version of an entry with its builds.
type CatalogueVersion struct {
	Version string
	Builds  []CatalogueBuild
}

func (s *Service) mayAdminTenant(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, tenant)
}

// Catalogue lists a tenant's catalogue of a package type (spec 0034): every (source, name), or those
// of one source or name (as answers name them), by source and name.
func (s *Service) Catalogue(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) ([]CatalogueEntry, error) {
	t, err := s.mayAdminTenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	vers, err := s.Store.SourceVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	labels, err := s.Store.SourceLabels(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	rules, err := s.Store.TenantRules(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	held, err := s.Store.HeldVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Store.RevokedVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	byKey := map[[2]string]*CatalogueEntry{}
	var out []*CatalogueEntry
	for k, vs := range vers {
		label := store.Label(labels, k[0])
		if source != "" && label != source || name != "" && k[1] != name {
			continue
		}
		slices.SortFunc(vs, func(x, y string) int { return CompareVersions(y, x) })
		hv, rv := held[k], revoked[k]
		slices.SortFunc(hv, func(x, y string) int { return CompareVersions(y, x) })
		slices.SortFunc(rv, func(x, y string) int { return CompareVersions(y, x) })
		e := &CatalogueEntry{Source: label, Name: k[1], Versions: vs, Rules: map[string]string{}, Held: hv, Revoked: rv}
		byKey[k] = e
		out = append(out, e)
	}
	for _, r := range rules {
		if e := byKey[[2]string{r.Source, r.Name}]; e != nil {
			e.Rules[r.Channel] = r.Mode
		}
	}
	slices.SortFunc(out, func(x, y *CatalogueEntry) int {
		if c := strings.Compare(x.Source, y.Source); c != 0 {
			return c
		}
		return strings.Compare(x.Name, y.Name)
	})
	res := make([]CatalogueEntry, len(out))
	for i, e := range out {
		res[i] = *e
	}
	return res, nil
}

// CatalogueVersions are an entry's versions (newest first) with their builds and where each is
// released; an entry the tenant has no build of (of the type) is not found.
func (s *Service) CatalogueVersions(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) ([]CatalogueVersion, error) {
	t, err := s.mayAdminTenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	src, err := s.Store.ResolveSource(ctx, t.ID, source)
	if errors.Is(err, store.ErrNoSource) {
		// a removed source's entries are not reached by name
		return nil, fmt.Errorf("%w: no source %q in the tenant", store.ErrNotFound, source)
	}
	if err != nil {
		return nil, err
	}
	builds, err := s.Store.SourceBuilds(ctx, t.ID, packageType, src, name)
	if err != nil {
		return nil, err
	}
	if len(builds) == 0 {
		return nil, fmt.Errorf("%w: no build of %s from %s", store.ErrNotFound, name, source)
	}
	placed, err := s.Store.BuildPlacements(ctx, t.ID, packageType, src, name)
	if err != nil {
		return nil, err
	}
	byVer := map[string]*CatalogueVersion{}
	var out []*CatalogueVersion
	for _, b := range builds {
		v := byVer[b.PackageVersion]
		if v == nil {
			v = &CatalogueVersion{Version: b.PackageVersion}
			byVer[b.PackageVersion] = v
			out = append(out, v)
		}
		v.Builds = append(v.Builds, CatalogueBuild{Build: b, Placed: placed[b.ID]})
	}
	slices.SortFunc(out, func(x, y *CatalogueVersion) int { return CompareVersions(y.Version, x.Version) })
	res := make([]CatalogueVersion, len(out))
	for i, v := range out {
		slices.SortFunc(v.Builds, func(x, y CatalogueBuild) int {
			if c := strings.Compare(x.Platform, y.Platform); c != 0 {
				return c
			}
			return strings.Compare(x.Slot(), y.Slot())
		})
		res[i] = *v
	}
	return res, nil
}
