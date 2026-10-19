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

// PlanTargets names what a plan asks for: with neither, every active bundle-mode upstream and feed;
// with only one of the two, the other kind is left out. Licenses asks for the licences of the names
// whose files come, of LicenseHolder only when it is set (spec 0021).
type PlanTargets struct {
	Upstreams, Feeds []string
	Licenses         bool
	LicenseHolder    string
}

// Plan writes a site's bundle plan (spec 0014): its active bundle-mode upstreams as sources and
// cells, with what it holds, and its active bundle-mode feeds with the bodies they offer and the last
// seq received (phase 1b). bundleKeys are the fingerprints pinned for the tenant.
func (s *Service) Plan(ctx context.Context, a authz.Actor, tenant string, sel PlanTargets, bundleKeys []string) (bundle.Plan, error) {
	names := sel.Upstreams
	noUpstreams := len(sel.Upstreams) == 0 && len(sel.Feeds) > 0
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
			if all[i].State != store.UpstreamActive {
				return fmt.Errorf("%w: upstream %s is paused: an import would not fill it", store.ErrInvalid, n)
			}
		}
		// paused ones are left out: an import fills active upstreams only
		for _, u := range all {
			if !noUpstreams && u.Mode == store.ModeBundle && u.State == store.UpstreamActive && (len(names) == 0 || slices.Contains(names, u.Name)) {
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
	plan := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: s.Store.Now().UTC(), Tenant: t.Name, BundleKeys: bundleKeys,
		Licenses: sel.Licenses, LicenseHolder: sel.LicenseHolder}
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
	if len(sel.Upstreams) == 0 || len(sel.Feeds) > 0 {
		if plan.Feeds, err = s.planFeeds(ctx, t, sel.Feeds, ups, plan.Sources); err != nil {
			return bundle.Plan{}, err
		}
	}
	return plan, plan.Validate()
}

// planFeeds lists a plan's feeds (spec 0014 phase 1b): the tenant's active bundle-mode feeds (all,
// or the named ones), merged by URL, each with the bodies it offers now and the last seq received
// (the lowest among feeds of one URL, so no feed's range starts above its cursor), and the plan's
// sources whose files it may then be offered.
func (s *Service) planFeeds(ctx context.Context, t store.Tenant, names []string, ups []store.Upstream, sources []bundle.PlanSource) ([]bundle.PlanFeed, error) {
	var feeds []store.Feed
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		all, err := tx.ListFeeds(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, n := range names {
			i := slices.IndexFunc(all, func(f store.Feed) bool { return f.Name == n })
			switch {
			case i < 0:
				return fmt.Errorf("%w: feed %s", store.ErrNotFound, n)
			case all[i].Mode != store.FeedBundle:
				return fmt.Errorf("%w: feed %s is not in bundle mode", store.ErrInvalid, n)
			case all[i].State != store.FeedActive:
				return fmt.Errorf("%w: feed %s is paused: an import would not fill it", store.ErrInvalid, n)
			}
		}
		for _, f := range all {
			if f.Mode == store.FeedBundle && f.State == store.FeedActive && (len(names) == 0 || slices.Contains(names, f.Name)) {
				feeds = append(feeds, f)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	index := map[string]int{}
	for i, src := range sources {
		index[src.Kind+"\x00"+src.Prefix] = i
	}
	byURL := map[string]*bundle.PlanFeed{}
	every := map[string]bool{} // a feed of the URL accepts every kind
	cursors := map[string]map[string]int64{}
	total := 0
	for _, f := range feeds {
		pf := byURL[f.URL]
		if pf == nil {
			pf = &bundle.PlanFeed{URL: f.URL}
			byURL[f.URL], cursors[f.URL] = pf, map[string]int64{}
		}
		pf.Keys = union(pf.Keys, f.Keys)
		if len(f.Kinds) == 0 {
			every[f.URL] = true
		}
		pf.Kinds = union(pf.Kinds, f.Kinds)
		for _, u := range ups { // the sources whose new bodies this feed's offer takes
			if f.Offer == store.OfferChannels && !slices.Contains(f.Channels, u.ChannelID) {
				continue
			}
			if i, ok := index[u.Kind+"\x00"+u.Prefix]; ok && !slices.Contains(pf.Sources, i) {
				pf.Sources = append(pf.Sources, i)
			}
		}
		bodies, err := s.Store.OfferedBodies(ctx, f)
		if err != nil {
			return nil, err
		}
		cur, err := s.Store.FeedCursors(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range bodies {
			seq, seen := cursors[f.URL][b]
			if !seen {
				if total++; total > bundle.MaxFeedBodies {
					return nil, fmt.Errorf("%w: the plan would hold over %d feed bodies (feed %s)", store.ErrInvalid, bundle.MaxFeedBodies, f.Name)
				}
				seq = cur[b]
			}
			cursors[f.URL][b] = min(seq, cur[b])
		}
	}
	var out []bundle.PlanFeed
	for u, pf := range byURL {
		if every[u] {
			pf.Kinds = []string{}
		}
		slices.Sort(pf.Sources)
		for b, seq := range cursors[u] {
			pf.Bodies = append(pf.Bodies, bundle.PlanBody{BodyHash: b, Seq: seq})
		}
		slices.SortFunc(pf.Bodies, func(a, b bundle.PlanBody) int { return strings.Compare(a.BodyHash, b.BodyHash) })
		if pf.Sources == nil {
			pf.Sources = []int{}
		}
		if pf.Bodies == nil {
			pf.Bodies = []bundle.PlanBody{}
		}
		out = append(out, *pf)
	}
	slices.SortFunc(out, func(a, b bundle.PlanFeed) int { return strings.Compare(a.URL, b.URL) })
	return out, nil
}

// union adds ys to xs, sorted, each once.
func union(xs, ys []string) []string {
	out := slices.Concat(xs, ys)
	slices.Sort(out)
	return slices.Compact(out)
}
