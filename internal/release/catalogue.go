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
	Withdrawn    []string          // the versions with a withdrawn build (spec 0033 phase B), newest first
	// PublisherID is a distributor's publisher's id, for a distribution admin (spec 0033 phase B: a
	// name two of them had is publisher-id:<id> as a source)
	PublisherID string
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

// serverActor reports whether an actor is a server administrator (a server token, the CLI): it names a
// tenant's distributors' publishers as a distribution admin does (spec 0033 phase B).
func serverActor(a authz.Actor) bool { return a.Kind == authz.ActorServer || a.Kind == authz.ActorOS }

func (s *Service) mayAdminTenant(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, tenant)
}

// Catalogue lists a tenant's catalogue of a package type (spec 0034): every (source, name), or those
// of one source or name (as answers name them), by source and name. A distribution admin reads its
// distributors' publishers' entries only (removed ones too), without rules: it reads no channel
// (spec 0033 phase B). A distributor's publisher reads its own entries only, in the distribution
// admin's view (spec 0037: a signed request; source, when named, is its own).
func (s *Service) Catalogue(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) ([]CatalogueEntry, error) {
	var t store.Tenant
	var only map[string]store.DistributorSource
	if a.Kind == authz.ActorPublisher {
		var d store.DistributorSource
		var err error
		if t, d, err = s.publisherSource(ctx, a, tenant); err != nil {
			return nil, err
		}
		if source != "" && !ownSource(d, source) {
			return nil, authz.ErrDenied // another source: refused (the same 404), and audited
		}
		source, only = "", map[string]store.DistributorSource{d.Source: d}
	} else {
		var err error
		if t, err = s.mayReadCatalogue(ctx, a, tenant); err != nil {
			return nil, err
		}
		if only, err = s.distributorSources(ctx, a, t); err != nil {
			return nil, err
		}
	}
	vers, err := s.Store.SourceVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	var labels map[string]string
	var rules []store.Rule
	ids := only // the distributors' publishers' ids an answer shows (spec 0033 phase B)
	if only != nil {
		labels = map[string]string{}
		for src, d := range only {
			labels[src] = d.Label
		}
	} else {
		if labels, err = s.Store.SourceLabels(ctx, t.ID); err != nil {
			return nil, err
		}
		if rules, err = s.Store.TenantRules(ctx, t.ID, packageType); err != nil {
			return nil, err
		}
		if serverActor(a) { // a server administrator names the distributors' publishers as a distribution admin does
			if ids, err = s.Store.DistributorSources(ctx, t.ID); err != nil {
				return nil, err
			}
			for src, d := range ids {
				labels[src] = d.Label
			}
		}
	}
	held, err := s.Store.HeldVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Store.RevokedVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	withdrawn, err := s.Store.WithdrawnVersions(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	newestFirst := func(x, y string) int { return CompareVersions(y, x) }
	byKey := map[[2]string]*CatalogueEntry{}
	var out []*CatalogueEntry
	for k, vs := range vers {
		if _, ok := only[k[0]]; only != nil && !ok {
			continue // a distribution admin's are the distributors' publishers
		}
		label := store.Label(labels, k[0])
		if source != "" && label != source || name != "" && k[1] != name {
			continue
		}
		hv, rv, wv := held[k], revoked[k], withdrawn[k]
		for _, l := range [][]string{vs, hv, rv, wv} {
			slices.SortFunc(l, newestFirst)
		}
		e := &CatalogueEntry{Source: label, Name: k[1], Versions: vs, Rules: map[string]string{}, Held: hv, Revoked: rv, Withdrawn: wv,
			PublisherID: ids[k[0]].PublisherID}
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
// released; an entry the tenant has no build of (of the type) is not found. A distribution admin
// reads its distributors' publishers' entries only (removed ones too), without where they are
// released (spec 0033 phase B: it reads no channel); a distributor's publisher its own entries only,
// so too (spec 0037: a signed request; source is its own).
func (s *Service) CatalogueVersions(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) ([]CatalogueVersion, error) {
	var t store.Tenant
	var err error
	resolve := s.Store.ResolveSource
	if a.Kind == authz.ActorPublisher {
		var d store.DistributorSource
		if t, d, err = s.publisherSource(ctx, a, tenant); err != nil {
			return nil, err
		}
		if !ownSource(d, source) {
			return nil, authz.ErrDenied // another source: refused (the same 404), and audited
		}
		resolve = func(context.Context, string, string) (string, error) { return d.Source, nil }
	} else if t, err = s.mayReadCatalogue(ctx, a, tenant); err != nil {
		return nil, err
	}
	switch {
	case a.Kind == authz.ActorDistributionAdmin:
		resolve = s.Store.ResolveDistributorSource
	case serverActor(a): // a distributor's publisher (removed or not) as a distribution admin names it, else any source
		resolve = func(ctx context.Context, tenantID, label string) (string, error) {
			src, err := s.Store.ResolveDistributorSource(ctx, tenantID, label)
			if errors.Is(err, store.ErrNoSource) {
				return s.Store.ResolveSource(ctx, tenantID, label)
			}
			return src, err
		}
	}
	src, err := resolve(ctx, t.ID, source)
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
	var placed map[string][]store.Placed
	if a.Kind != authz.ActorDistributionAdmin && a.Kind != authz.ActorPublisher {
		if placed, err = s.Store.BuildPlacements(ctx, t.ID, packageType, src, name); err != nil {
			return nil, err
		}
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
