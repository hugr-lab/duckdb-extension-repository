package release

import (
	"context"
	"slices"
	"sync"
	"time"
	"unicode"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Snapshot is a channel's releases (every state and visibility), the C API maxima of its DuckDB
// versions and its keys, read once per (channel, channels.version, release_version) and shared by
// every caller: the DuckDB routes and the index (spec 0007) resolve on it with the same functions,
// so they cannot disagree, and the work does not depend on who asks.
type Snapshot struct {
	Channel  store.Channel
	Releases []store.Candidate       // ordered by created_at, id
	CAPIs    map[string][]store.CAPI // the channel's DuckDB versions
	Keys     []store.Key             // signed channels; a key change bumps channels.version
	// ServingKey is the serving key's fingerprint ("" without one); a move bumps release_version.
	ServingKey string
	// Blocked are the tenant's blocked body hashes (spec 0008); a block or unblock bumps
	// release_version on every signed channel of the tenant.
	Blocked map[string]bool
	// Provided holds the names the tenant's upstreams provide (spec 0009); allowlist changes bump
	// release_version on every channel of the tenant.
	Provided map[string]bool
	// Shadowed holds, for a passthrough channel, the core names the tenant replaced (spec 0009): the
	// channel does not serve them. Shadow changes bump the tenant's passthrough channels.
	Shadowed map[string]bool

	groups    map[group][]*store.Candidate // by (name, platform), in Releases order
	platforms map[string][]string          // by name, sorted
}

type group struct{ name, platform string }

// NewSnapshot indexes a channel's releases: a signed channel's, or a passthrough channel's
// (spec 0009: DuckDB's builds, served with their original signature).
func NewSnapshot(c store.Channel, rels []store.Candidate, capis map[string][]store.CAPI, keys []store.Key) *Snapshot {
	s := &Snapshot{Channel: c, Releases: rels, CAPIs: capis, Keys: keys, groups: map[group][]*store.Candidate{},
		platforms: map[string][]string{}}
	for _, k := range keys {
		if k.ID == c.ServingKeyID {
			s.ServingKey = k.Fingerprint
		}
	}
	for i := range rels {
		g := group{rels[i].Name, rels[i].Platform}
		if s.groups[g] == nil {
			s.platforms[g.name] = append(s.platforms[g.name], g.platform)
		}
		s.groups[g] = append(s.groups[g], &rels[i])
	}
	for _, ps := range s.platforms {
		slices.Sort(ps)
	}
	return s
}

// Versions returns the channel's DuckDB versions in version order.
func (s *Snapshot) Versions() []string {
	vs := make([]string, 0, len(s.CAPIs))
	for v := range s.CAPIs {
		vs = append(vs, v)
	}
	slices.SortFunc(vs, CompareVersions)
	return vs
}

// Group returns the releases of a name on a platform, in created_at order.
func (s *Snapshot) Group(name, platform string) []*store.Candidate {
	return s.groups[group{name, platform}]
}

// Platforms returns the platforms a name has releases on, sorted.
func (s *Snapshot) Platforms(name string) []string { return s.platforms[name] }

// Visible decides whether a caller sees a release.
type Visible func(*store.Candidate) bool

// PublicOnly is the view of a caller without install on the release's extension.
func PublicOnly(c *store.Candidate) bool { return c.Visibility == store.Public }

// Everything is the view of a caller with install.
func Everything(*store.Candidate) bool { return true }

// Serves reports whether a release's build serves a DuckDB version of the channel: the exact version
// for cpp and c_struct_unstable; for c_struct, a version whose C API maximum for the build's major
// accepts it.
func Serves(c *store.Candidate, version string, capis map[string][]store.CAPI) bool {
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

// serving reports whether the channel serves name at all: a signed channel with a serving key, or
// a passthrough channel (no keys) for a name the tenant has not replaced.
func (s *Snapshot) serving(name string) bool {
	if s.Channel.Kind == store.ChannelPassthrough {
		return !s.Shadowed[name]
	}
	return s.Channel.ServingKeyID != ""
}

// Flat is what the flat path serves for (DuckDB version, platform, name) in a view: among the active
// releases with a seq that serve the version, the highest seq. A signed channel without a serving
// key serves nothing.
func (s *Snapshot) Flat(version, platform, name string, vis Visible) *store.Candidate {
	if !s.serving(name) {
		return nil
	}
	var best *store.Candidate
	for _, c := range s.groups[group{name, platform}] {
		if c.State != store.ReleaseActive || c.Seq == 0 || !vis(c) || !Serves(c, version, s.CAPIs) {
			continue
		}
		if best == nil || c.Seq > best.Seq {
			best = c
		}
	}
	return best
}

// Versioned is what the versioned path serves for (DuckDB version, platform, name, extension version)
// in a view, among releases in the given states (active and deprecated for the DuckDB routes): for
// c_struct, the highest C API major the DuckDB version accepts. A signed channel without a serving
// key serves nothing.
func (s *Snapshot) Versioned(version, platform, name, extVersion string, vis Visible, states ...string) *store.Candidate {
	if !s.serving(name) {
		return nil
	}
	if len(states) == 0 {
		states = []string{store.ReleaseActive, store.ReleaseDeprecated}
	}
	var best *store.Candidate
	for _, c := range s.groups[group{name, platform}] {
		if c.ExtVersion != extVersion || !slices.Contains(states, c.State) || !vis(c) || !Serves(c, version, s.CAPIs) {
			continue
		}
		if best == nil || better(c, best) {
			best = c
		}
	}
	return best
}

func better(a, b *store.Candidate) bool {
	am, bm := major(a), major(b)
	if am != bm {
		return am > bm
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

func major(c *store.Candidate) int {
	if c.CAPI == nil {
		return -1
	}
	return c.CAPI.Major
}

// CompareVersions orders version strings with their digit runs compared as numbers ("v1.9.0" before
// "v1.10.0"), then bytewise.
func CompareVersions(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			ei, ej := i, j
			for ei < len(a) && isDigit(a[ei]) {
				ei++
			}
			for ej < len(b) && isDigit(b[ej]) {
				ej++
			}
			if c := compareNumbers(a[i:ei], b[j:ej]); c != 0 {
				return c
			}
			i, j = ei, ej
			continue
		}
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case len(a)-i < len(b)-j:
		return -1
	case len(a)-i > len(b)-j:
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func isDigit(c byte) bool { return c < 0x80 && unicode.IsDigit(rune(c)) }

func compareNumbers(a, b string) int {
	for len(a) > 1 && a[0] == '0' {
		a = a[1:]
	}
	for len(b) > 1 && b[0] == '0' {
		b = b[1:]
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Snapshots caches the newest snapshot of each channel, building it once however many requests ask
// at the same time.
type Snapshots struct {
	Store *store.Store

	mu    sync.Mutex
	cache map[string]*entry // channel id
}

type entry struct {
	version, releaseVersion int64
	done                    chan struct{}
	snap                    *Snapshot
	err                     error
}

// covers reports whether the entry was built for counters at least the caller's: a snapshot read
// after a newer change is as good as one for the caller's row.
func (e *entry) covers(c store.Channel) bool {
	return e.version >= c.Version && e.releaseVersion >= c.ReleaseVersion
}

const maxSnapshots = 4096

// Get returns a channel's snapshot for the counters the caller read with the channel row.
func (s *Snapshots) Get(ctx context.Context, c store.Channel) (*Snapshot, error) {
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]*entry{}
	}
	e := s.cache[c.ID]
	if e == nil || !e.covers(c) {
		e = &entry{version: c.Version, releaseVersion: c.ReleaseVersion, done: make(chan struct{})}
		if len(s.cache) >= maxSnapshots {
			for id := range s.cache { // drop one: the cache is a speed-up, not a record
				delete(s.cache, id)
				break
			}
		}
		s.cache[c.ID] = e
		s.mu.Unlock()
		// one caller's cancellation must not fail the others waiting on the same build
		e.snap, e.err = s.build(context.WithoutCancel(ctx), c)
		close(e.done)
		if e.err != nil {
			s.mu.Lock()
			if s.cache[c.ID] == e {
				delete(s.cache, c.ID)
			}
			s.mu.Unlock()
		}
		return e.snap, e.err
	}
	s.mu.Unlock()
	select {
	case <-e.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	return e.snap, nil
}

func (s *Snapshots) build(ctx context.Context, c store.Channel) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rels, err := s.Store.ChannelReleases(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	capis, err := s.Store.ChannelCAPIs(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	var keys []store.Key
	if c.Kind == store.ChannelSigned {
		if keys, err = s.Store.ListKeys(ctx, c.ID); err != nil {
			return nil, err
		}
	}
	snap := NewSnapshot(c, rels, capis, keys)
	if snap.Blocked, err = s.Store.BlockedHashes(ctx, c.TenantID); err != nil {
		return nil, err
	}
	if snap.Provided, err = s.Store.ProvidedNames(ctx, c.TenantID); err != nil {
		return nil, err
	}
	if c.Kind == store.ChannelPassthrough {
		shadows, err := s.Store.ListShadows(ctx, c.TenantID)
		if err != nil {
			return nil, err
		}
		snap.Shadowed = map[string]bool{}
		for _, x := range shadows {
			snap.Shadowed[x.Name] = true
		}
	}
	return snap, nil
}

// Shadows is what a release shadows (spec 0008, 0009): a reserved name's owner, or "upstream" for a
// replacement of a name the tenant's upstreams provide; nothing for an upstream's build (mirrored or
// promoted).
func Shadows(c store.Candidate, provided map[string]bool) string {
	if !c.Replacement() {
		return ""
	}
	if k := reserved.Kind(c.Name); k != "" {
		return k
	}
	if provided[c.Name] && c.Replacement() {
		return "upstream"
	}
	return ""
}
