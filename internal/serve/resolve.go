package serve

import (
	"context"
	"fmt"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// view is what a caller may see in a channel: public releases only, or everything.
type view int

const (
	viewPublic view = iota
	viewAll         // phase 2: a caller with install
)

// resolution is what a binary path resolves to.
type resolution struct {
	found bool
	cand  store.Candidate
	sig   []byte // by the channel's serving key
}

// resolver resolves binary paths, caching per (path, view, channels.version, release_version):
// every request reads both counters with the channel row, so a change applies at the next request.
type resolver struct {
	st *store.Store

	mu    sync.Mutex
	res   map[string]resolution
	capis map[string]map[string][]store.CAPI // (channel id, channels.version) -> the channel's versions
}

const maxCached = 16384

func newResolver(st *store.Store) *resolver {
	return &resolver{st: st, res: map[string]resolution{}, capis: map[string]map[string][]store.CAPI{}}
}

func (rv *resolver) channelCAPIs(ctx context.Context, c store.Channel) (map[string][]store.CAPI, error) {
	k := fmt.Sprintf("%s|%d", c.ID, c.Version)
	rv.mu.Lock()
	m, ok := rv.capis[k]
	rv.mu.Unlock()
	if ok {
		return m, nil
	}
	m, err := rv.st.ChannelCAPIs(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	rv.mu.Lock()
	if len(rv.capis) >= maxCached {
		clear(rv.capis)
	}
	rv.capis[k] = m
	rv.mu.Unlock()
	return m, nil
}

// serves reports whether a candidate serves a DuckDB version of the channel.
func serves(c store.Candidate, version string, capis map[string][]store.CAPI) bool {
	maxima, ok := capis[version]
	if !ok {
		return false // the channel does not serve that DuckDB version
	}
	if c.ABI != store.ABICStruct {
		return c.DuckDBVersion == version
	}
	if c.CAPI == nil {
		return false
	}
	for _, m := range maxima {
		if m.Accepts(*c.CAPI) {
			return true
		}
	}
	return false
}

func (rv *resolver) resolve(ctx context.Context, sc store.ServeChannel, rt route, v view) (resolution, error) {
	c := sc.Channel
	key := fmt.Sprintf("%s|%d|%d|%d|%d|%s|%s|%s|%s", c.ID, v, c.Version, c.ReleaseVersion, rt.kind, rt.name, rt.extVersion,
		rt.duckdbVersion, rt.platform)
	rv.mu.Lock()
	res, ok := rv.res[key]
	rv.mu.Unlock()
	if ok {
		return res, nil
	}
	res, err := rv.lookup(ctx, c, rt, v)
	if err != nil {
		return resolution{}, err
	}
	rv.mu.Lock()
	if len(rv.res) >= maxCached {
		clear(rv.res)
	}
	rv.res[key] = res
	rv.mu.Unlock()
	return res, nil
}

// lookup does the same queries whether or not a path the caller may not see exists: the candidates
// (public only for a public view, so private rows are never read) and the channel's versions; only a
// release the caller may see adds the signature query.
func (rv *resolver) lookup(ctx context.Context, c store.Channel, rt route, v view) (resolution, error) {
	capis, err := rv.channelCAPIs(ctx, c)
	if err != nil {
		return resolution{}, err
	}
	var cands []store.Candidate
	if rt.kind == routeFlat {
		cands, err = rv.st.FlatCandidates(ctx, c.ID, rt.name, rt.platform, v == viewPublic)
	} else {
		cands, err = rv.st.VersionedCandidates(ctx, c.ID, rt.name, rt.extVersion, rt.platform, v == viewPublic)
	}
	if err != nil {
		return resolution{}, err
	}
	var best *store.Candidate
	for i := range cands {
		cd := &cands[i]
		if !serves(*cd, rt.duckdbVersion, capis) {
			continue
		}
		switch {
		case best == nil:
			best = cd
		case rt.kind == routeFlat:
			// candidates come highest seq first
		case cd.CAPI != nil && best.CAPI != nil && cd.CAPI.Major > best.CAPI.Major:
			best = cd // versioned c_struct: the highest C API major the DuckDB version supports
		}
	}
	if best == nil || c.ServingKeyID == "" {
		return resolution{}, nil
	}
	sig, err := rv.st.Signature(ctx, best.ID, c.ServingKeyID)
	if err != nil {
		// the invariant says this cannot happen; fail closed
		return resolution{}, fmt.Errorf("serve: release %s has no signature by the serving key: %w", best.ID, err)
	}
	return resolution{found: true, cand: *best, sig: sig}, nil
}
