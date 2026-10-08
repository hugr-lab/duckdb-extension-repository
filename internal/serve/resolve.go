package serve

import (
	"context"
	"fmt"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// view is what a caller may see in a channel: public releases only, or everything.
type view int

const (
	viewPublic view = iota
	viewAll         // a caller with install on the path's extension
)

func (v view) visible() release.Visible {
	if v == viewAll {
		return release.Everything
	}
	return release.PublicOnly
}

// resolution is what a binary path resolves to.
type resolution struct {
	found bool
	cand  store.Candidate
	sig   []byte // by the channel's serving key
}

// resolver resolves binary paths on the channel's snapshot (internal/release, shared with the
// index), caching the result with the serving key's signature per (path, view, channels.version,
// release_version): every request reads both counters with the channel row, so a change applies at
// the next request.
type resolver struct {
	st    *store.Store
	snaps *release.Snapshots

	mu  sync.Mutex
	res map[string]resolution
}

const maxCached = 16384

func newResolver(st *store.Store, snaps *release.Snapshots) *resolver {
	return &resolver{st: st, snaps: snaps, res: map[string]resolution{}}
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

// lookup resolves on the snapshot, which is read independently of the caller and of what exists;
// only a release the caller may see adds the signature query.
func (rv *resolver) lookup(ctx context.Context, c store.Channel, rt route, v view) (resolution, error) {
	snap, err := rv.snaps.Get(ctx, c)
	if err != nil {
		return resolution{}, err
	}
	var best *store.Candidate
	if rt.kind == routeFlat {
		best = snap.Flat(rt.duckdbVersion, rt.platform, rt.name, v.visible())
	} else {
		best = snap.Versioned(rt.duckdbVersion, rt.platform, rt.name, rt.extVersion, v.visible())
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
