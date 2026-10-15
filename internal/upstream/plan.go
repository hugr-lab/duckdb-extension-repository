package upstream

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Plan writes a site's bundle plan (spec 0014): its bundle-mode upstreams (all, or the named ones)
// as sources and cells, with what it holds. bundleKeys are the fingerprints pinned for the tenant.
func (s *Service) Plan(ctx context.Context, a authz.Actor, tenant string, names, bundleKeys []string) (bundle.Plan, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return bundle.Plan{}, err
	}
	if len(bundleKeys) == 0 {
		return bundle.Plan{}, fmt.Errorf("%w: no bundle key is pinned for tenant %s (upstreams.bundle_keys)", store.ErrInvalid, tenant)
	}
	var ups []store.Upstream
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		all, err := tx.ListUpstreams(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, n := range names {
			i := slices.IndexFunc(all, func(u store.Upstream) bool { return u.Name == n })
			if i < 0 {
				return fmt.Errorf("%w: upstream %s", store.ErrNotFound, n)
			}
			if all[i].Mode != store.ModeBundle {
				return fmt.Errorf("%w: upstream %s is not in bundle mode", store.ErrInvalid, n)
			}
		}
		for _, u := range all {
			if u.Mode == store.ModeBundle && (len(names) == 0 || slices.Contains(names, u.Name)) {
				ups = append(ups, u)
			}
		}
		return nil
	}); err != nil {
		return bundle.Plan{}, err
	}
	kinds := map[string]string{}
	vs, err := s.Store.ListDuckDBVersions(ctx)
	if err != nil {
		return bundle.Plan{}, err
	}
	for _, v := range vs {
		kinds[v.Name] = v.Kind
	}

	type held struct {
		etag, hash string
		ok         bool // every sharing upstream agrees
	}
	type merged struct {
		versions []string
		any      bool // an empty filter: any version
		held     *held
	}
	type source struct {
		kind, prefix string
		keys         []string
		cells        map[[3]string]*merged
	}
	sources := map[string]*source{}
	total := 0
	for _, u := range ups {
		key := u.Kind + "\x00" + u.Prefix
		src := sources[key]
		if src == nil {
			src = &source{kind: u.Kind, prefix: u.Prefix, cells: map[[3]string]*merged{}}
			sources[key] = src
		}
		for _, k := range u.Keys {
			if !slices.Contains(src.keys, k) {
				src.keys = append(src.keys, k)
			}
		}
		versions, err := s.Store.ChannelVersions(ctx, u.ChannelID)
		if err != nil {
			return bundle.Plan{}, err
		}
		var cells map[[3]string]store.UpstreamCell
		if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			cells, err = tx.Cells(ctx, u.ID)
			return err
		}); err != nil {
			return bundle.Plan{}, err
		}
		for _, v := range versions {
			// DuckDB publishes nothing for a development build; another kista may serve one
			if kinds[v] == "dev" && u.Kind != store.UpstreamRepo {
				continue
			}
			for _, p := range u.Platforms {
				for _, e := range u.Entries {
					ck := [3]string{v, p, e.Name}
					m := src.cells[ck]
					if m == nil {
						if total++; total > bundle.MaxCells {
							return bundle.Plan{}, fmt.Errorf("%w: the plan would hold over %d cells", store.ErrInvalid, bundle.MaxCells)
						}
						m = &merged{}
						src.cells[ck] = m
					}
					if len(e.Versions) == 0 {
						m.any = true
					}
					for _, x := range e.Versions {
						if !slices.Contains(m.versions, x) {
							m.versions = append(m.versions, x)
						}
					}
					c, ok := cells[ck]
					conditional := ok && c.BodyHash != "" && slices.Contains([]string{store.CellReleased, store.CellUnchanged,
						store.CellYanked, store.CellConflict}, c.Outcome)
					switch {
					case m.held == nil && conditional:
						m.held = &held{etag: c.ETag, hash: c.BodyHash, ok: true}
					case m.held == nil:
						m.held = &held{}
					case !conditional || c.ETag != m.held.etag || c.BodyHash != m.held.hash:
						m.held.ok = false
					}
				}
			}
		}
	}
	plan := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: s.Store.Now().UTC(), Tenant: t.Name, BundleKeys: bundleKeys}
	for _, src := range sources {
		ps := bundle.PlanSource{Kind: src.kind, Prefix: src.prefix, Keys: src.keys, Cells: []bundle.PlanCell{}}
		slices.Sort(ps.Keys)
		for ck, m := range src.cells {
			c := bundle.PlanCell{DuckDBVersion: ck[0], Platform: ck[1], Name: ck[2], Versions: []string{}}
			if !m.any {
				c.Versions = m.versions
				slices.Sort(c.Versions)
			}
			if m.held != nil && m.held.ok {
				c.ETag, c.BodyHash = m.held.etag, m.held.hash
			}
			ps.Cells = append(ps.Cells, c)
		}
		slices.SortFunc(ps.Cells, func(a, b bundle.PlanCell) int {
			if c := strings.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			if c := strings.Compare(a.Platform, b.Platform); c != 0 {
				return c
			}
			return release.CompareVersions(a.DuckDBVersion, b.DuckDBVersion)
		})
		plan.Sources = append(plan.Sources, ps)
	}
	slices.SortFunc(plan.Sources, func(a, b bundle.PlanSource) int {
		return strings.Compare(a.Kind+"\x00"+a.Prefix, b.Kind+"\x00"+b.Prefix)
	})
	if plan.Sources == nil {
		plan.Sources = []bundle.PlanSource{}
	}
	return plan, plan.Validate()
}
