package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: placing builds of the catalogue into a channel, by hand and by the channel's rules.

// placer is the placement pass's actor.
var placer = authz.Actor{Kind: authz.ActorSystem, ID: "placement"}

// PlaceOptions name what to place by hand: a version of the name from a source of the tenant (as
// answers name it: tenant, upstream:<name>, publisher:<name>).
type PlaceOptions struct {
	Source, Version string
	Private         bool // narrow the visibility (a placement never widens it)
	NotCurrent      bool
}

// Place releases a source's builds of a version of name, those the channel serves, into a signed
// channel, all or none (spec 0033). The caller needs promote on (channel, name). A channel
// administrator may place any checked build of the catalogue, and a name without a rule gets one
// (manual) first; anyone else only builds of the rule's source that are active or deprecated in a
// channel where it may publish the name (what a promotion takes), and learns nothing else (404).
func (s *Service) Place(ctx context.Context, a authz.Actor, tenant, channel, name string, o PlaceOptions) ([]store.Release, bool, error) {
	kind, err := s.Store.ReservedFor(ctx, tenant, name) // spec 0030: DuckDB's, the tenant's, its upstreams'
	if err != nil {
		return nil, false, err
	}
	res := authz.Resource{Tenant: tenant, Channel: channel, Extension: name, Reserved: kind != ""}
	if err := s.Authz.Allow(ctx, a, authz.VerbPromote, res); err != nil {
		return nil, false, err
	}
	if o.Source == "" || o.Version == "" {
		return nil, false, fmt.Errorf("%w: source and version are required", store.ErrInvalid)
	}
	admin := true
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); errors.Is(err, authz.ErrDenied) {
		admin = false
	} else if err != nil {
		return nil, false, err
	}
	missing := fmt.Errorf("%w: release", store.ErrNotFound) // what a non-administrator learns
	sc, err := s.Store.GetServeChannel(ctx, tenant, channel)
	if err != nil {
		return nil, false, err
	}
	ch := sc.Channel
	if ch.Kind != store.ChannelSigned {
		return nil, false, fmt.Errorf("%w: placement is into a signed channel", ErrState)
	}
	if _, err := activeKey(ctx, s.Store, ch.ID); err != nil {
		return nil, false, err
	}
	src, err := s.Store.ResolveSource(ctx, ch.TenantID, o.Source)
	switch {
	case errors.Is(err, store.ErrNoSource) && !admin:
		return nil, false, missing
	case errors.Is(err, store.ErrNoSource):
		return nil, false, fmt.Errorf("%w: %q is not a source of the tenant (tenant, upstream:<an upstream>, publisher:<a publisher>)",
			store.ErrInvalid, o.Source)
	case err != nil:
		return nil, false, err
	}
	rules, err := s.Store.ChannelSources(ctx, ch.ID)
	if err != nil {
		return nil, false, err
	}
	if _, ok := rules[name]; !ok && !admin {
		return nil, false, missing
	}
	builds, err := s.Store.SourceBuilds(ctx, ch.TenantID, src, name)
	if err != nil {
		return nil, false, err
	}
	purged, err := s.Store.PurgedBuilds(ctx, ch.TenantID, src, name)
	if err != nil {
		return nil, false, err
	}
	var placed map[string][]store.Placed
	if !admin { // what a promotion takes: a release the caller may publish from
		if placed, err = s.Store.BuildPlacements(ctx, ch.TenantID, src, name); err != nil {
			return nil, false, err
		}
	}
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return nil, false, err
	}
	var items []item
	for _, b := range builds {
		if b.ExtVersion != o.Version || servable(capis, b) != nil || purged[b.ID] {
			continue
		}
		if !admin && !s.mayTake(ctx, a, tenant, name, kind != "", placed[b.ID]) {
			continue
		}
		if !b.Checked {
			return nil, false, fmt.Errorf("%w: %s %s %s was added unchecked; publish it to check it", ErrState, b.Name, b.ExtVersion, b.Platform)
		}
		it := s.placement(tenant, b, src, a, "hand", o.Private)
		it.notCurrent = o.NotCurrent
		if !res.Reserved {
			it.recheck = s.reservedCheck(a, authz.VerbPromote, res)
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		if !admin {
			return nil, false, missing
		}
		return nil, false, fmt.Errorf("%w: no build of %s %s from %s that the channel serves", store.ErrNotFound, name, o.Version, o.Source)
	}
	if _, ok := rules[name]; !ok { // an administrator's placement chooses the source: a manual rule
		if err := s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
			if _, had, err := tx.ChannelSourceOf(ctx, ch.ID, name); err != nil || had {
				return err
			}
			if _, err := tx.SetChannelRule(ctx, ch.ID, name, src, store.RuleManual, a.String()); err != nil {
				return err
			}
			return tx.Event(ctx, ch.TenantID, a.String(), "source.set", "channel:"+ch.Name+"/ext:"+name,
				map[string]any{"name": name, "source": o.Source, "before": "", "arrival": false, "mode": store.RuleManual})
		}); err != nil {
			return nil, false, err
		}
	}
	return s.insert(ctx, a, ch.ID, items, placeEvent(ctx, name, o.Source, "hand"))
}

// mayTake reports whether a caller may take a build by hand without administering the channel: it
// is active or deprecated in a channel where the caller may publish the name.
func (s *Service) mayTake(ctx context.Context, a authz.Actor, tenant, name string, reserved bool, placed []store.Placed) bool {
	for _, p := range placed {
		if p.State != store.ReleaseActive && p.State != store.ReleaseDeprecated {
			continue
		}
		if s.Authz.Allow(ctx, a, authz.VerbPublish, authz.Resource{Tenant: tenant, Channel: p.Channel, Extension: name, Reserved: reserved}) == nil {
			return true
		}
	}
	return false
}

// placement is the item that places a catalogue build of a tenant (by name): as its arrival made its
// release (its origin and original signature for a mirrored build, its visibility narrowed as a
// private upstream's credential allows now, its provenance), with who placed it and how.
func (s *Service) placement(tenant string, b store.Build, source string, a authz.Actor, how string, private bool) item {
	vis := b.Visibility
	cr := CredentialOf(store.Release{Origin: store.OriginPlacement, Provenance: b.Provenance})
	if private || cr != "" && (s.MayPublish == nil || !s.MayPublish(tenant, cr)) {
		vis = store.Private
	}
	pm := map[string]any{}
	if b.Provenance != "" {
		_ = json.Unmarshal([]byte(b.Provenance), &pm)
	}
	pm["placed_by"], pm["placed"] = a.String(), how
	prov, err := json.Marshal(pm)
	if err != nil || len(prov) > 4000 {
		prov, _ = json.Marshal(map[string]string{"placed_by": a.String(), "placed": how, "credential": cr})
	}
	it := item{b: b, visibility: vis, origin: store.OriginPlacement, provenance: string(prov), source: source}
	if b.Origin == store.OriginUpstream { // a mirror's build stays its mirror's (licences, usage, bundles)
		it.origin, it.originSignature = store.OriginUpstream, b.OriginSignature
	}
	return it
}

func placeEvent(ctx context.Context, name, source, how string) eventFn {
	return func(tx *store.Tx, c store.Channel, made []store.Release) error {
		ids := make([]string, len(made))
		for i, r := range made {
			ids[i] = r.ID
		}
		return tx.Event(ctx, c.TenantID, made[0].CreatedBy, "release.place", "channel:"+c.Name+"/ext:"+name,
			map[string]any{"releases": ids, "name": name, "version": made[0].ExtVersion, "source": source, "placed": how})
	}
}

// cell is a platform and a DuckDB version of a channel.
type cell struct{ platform, version string }

// served is what a source's releases of a name serve in a channel, per cell: the newest version any
// release has or had (any state), the newest active one, and per version whether it is active
// (held) and the narrowest visibility of its active releases.
type served struct {
	had          map[[3]string]bool // (version, platform, slot)
	newest, live map[cell]string
	held         map[string]bool
	private      map[string]bool
	capis        map[string][]store.CAPI
}

func newServed(cands []store.Candidate, capis map[string][]store.CAPI) served {
	s := served{had: map[[3]string]bool{}, newest: map[cell]string{}, live: map[cell]string{}, held: map[string]bool{},
		private: map[string]bool{}, capis: capis}
	for i := range cands {
		c := &cands[i]
		s.had[[3]string{c.ExtVersion, c.Platform, c.Slot}] = true
		for v := range capis {
			if c.State == store.ReleasePurged && c.ABI != store.ABICStruct && c.Slot == v || Serves(c, v, capis) {
				k := cell{c.Platform, v}
				if cur, ok := s.newest[k]; !ok || CompareVersions(c.ExtVersion, cur) > 0 {
					s.newest[k] = c.ExtVersion
				}
				if c.State == store.ReleaseActive {
					if cur, ok := s.live[k]; !ok || CompareVersions(c.ExtVersion, cur) > 0 {
						s.live[k] = c.ExtVersion
					}
				}
			}
		}
		if c.State == store.ReleaseActive {
			s.held[c.ExtVersion] = true
			s.private[c.ExtVersion] = s.private[c.ExtVersion] || c.Visibility == store.Private
		}
	}
	return s
}

// cells are the channel's cells a build serves.
func (s served) cells(b store.Build) []cell {
	c := &store.Candidate{Release: store.Release{Platform: b.Platform}, ABI: b.ABI, DuckDBVersion: b.DuckDBVersion, CAPI: b.CAPI}
	var out []cell
	for v := range s.capis {
		if Serves(c, v, s.capis) {
			out = append(out, cell{b.Platform, v})
		}
	}
	return out
}

// newer reports whether version is newer than what every cell has or had (live: what it serves).
func newer(version string, cells []cell, by map[cell]string) bool {
	for _, k := range cells {
		if v, ok := by[k]; ok && CompareVersions(version, v) <= 0 {
			return false
		}
	}
	return true
}

// overlap reports whether two builds serve a cell in common.
func overlap(a, b []cell) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// complete adjusts an item that completes a held version (spec 0033): not current where a newer
// version serves one of its cells, and private where the version is private in the channel.
func (s served) complete(it *item) {
	if !newer(it.b.ExtVersion, s.cells(it.b), s.live) {
		it.notCurrent = true
	}
	if s.private[it.b.ExtVersion] {
		it.visibility = store.Private
	}
}

// PlaceRule places what a channel's rule takes (spec 0033): for an auto rule, per cell the newest
// build of its source the channel serves when it is newer than every version the cells it serves
// have or had; for both modes, a build of a version the channel holds (an active release of the
// source) in a slot it lacks, not current where a newer version serves and private where the version
// is. Never a build whose slot has or had the version (a yanked or purged one stays out), nor one
// purged for good. Each build is placed on its own, renewing the caller's lease first; it returns how
// many were placed and the first failure's text ("" for none).
func (s *Service) PlaceRule(ctx context.Context, r store.DueRule, renew func(context.Context) error) (int, string, error) {
	if r.Mode == store.RuleFollow { // a follow's rule: auto while the channel follows the source
		following, err := s.Store.Following(ctx, r.ChannelID, r.Source)
		if err != nil {
			return 0, "", err
		}
		r.Mode = store.RuleManual
		if following {
			r.Mode = store.RuleAuto
		}
	}
	builds, err := s.Store.SourceBuilds(ctx, r.TenantID, r.Source, r.Name)
	if err != nil || len(builds) == 0 {
		return 0, "", err
	}
	tn, err := s.Store.GetTenantAnyByID(ctx, r.TenantID)
	if err != nil {
		return 0, "", err
	}
	capis, err := s.Store.ChannelCAPIs(ctx, r.ChannelID)
	if err != nil {
		return 0, "", err
	}
	purged, err := s.Store.PurgedBuilds(ctx, r.TenantID, r.Source, r.Name)
	if err != nil {
		return 0, "", err
	}
	var cands []store.Candidate
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		cands, err = tx.SourceReleases(ctx, r.ChannelID, r.Source, r.Name)
		return err
	}); err != nil {
		return 0, "", err
	}
	label, err := s.Store.SourceLabel(ctx, r.Source)
	if err != nil {
		return 0, "", err
	}
	sv := newServed(cands, capis)
	var items []item
	best := map[[2]string]store.Build{} // (platform, slot) → the newest new version
	for _, b := range builds {
		if !b.Checked || servable(capis, b) != nil || purged[b.ID] || sv.had[[3]string{b.ExtVersion, b.Platform, b.Slot()}] {
			continue
		}
		if sv.held[b.ExtVersion] {
			it := s.placement(tn.Name, b, r.Source, placer, r.Mode, false)
			sv.complete(&it)
			items = append(items, it)
			continue
		}
		if r.Mode != store.RuleAuto || !newer(b.ExtVersion, sv.cells(b), sv.newest) {
			continue
		}
		k := [2]string{b.Platform, b.Slot()}
		if cur, ok := best[k]; !ok || CompareVersions(b.ExtVersion, cur.ExtVersion) > 0 {
			best[k] = b
		}
	}
	// a new version is placed only where no newer one placed with it serves the same cell (a C API
	// build serves the DuckDB versions an exact-version build of another slot does)
	for k, b := range best {
		cells := sv.cells(b)
		shadowed := false
		for k2, o := range best {
			if k2 != k && CompareVersions(o.ExtVersion, b.ExtVersion) > 0 && overlap(cells, sv.cells(o)) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			items = append(items, s.placement(tn.Name, b, r.Source, placer, r.Mode, false))
		}
	}
	// oldest version first, so the newest placed is current
	slices.SortFunc(items, func(x, y item) int {
		if c := CompareVersions(x.b.ExtVersion, y.b.ExtVersion); c != 0 {
			return c
		}
		return strings.Compare(x.b.Platform+"/"+x.b.Slot(), y.b.Platform+"/"+y.b.Slot())
	})
	placed, failure := 0, ""
	for _, it := range items {
		if renew != nil {
			if err := renew(ctx); err != nil {
				return placed, failure, err
			}
		}
		_, existed, err := s.insert(ctx, placer, r.ChannelID, []item{it}, placeEvent(ctx, r.Name, label, r.Mode))
		switch {
		case err != nil && failure == "":
			failure = fmt.Sprintf("%s %s %s: %v", it.b.ExtVersion, it.b.Platform, it.b.Slot(), err)
		case err == nil && !existed:
			placed++
		}
	}
	return placed, failure, nil
}

// Placer runs the placement pass (spec 0033): every Interval, the rules whose source has builds
// the pass has not looked at, and every SweepEvery all of them; a channel's rules on one replica at a
// time.
type Placer struct {
	Service    *Service
	Holder     string // this replica
	Interval   time.Duration
	SweepEvery time.Duration
	Log        *slog.Logger
}

var errLeaseLost = errors.New("release: the placement lease was lost")

// placeSkew is how far back the pass's look starts: a build whose transaction began before a look
// and committed after it is still found by the next.
const placeSkew = 5 * time.Minute

// Run loops until ctx is done.
func (p *Placer) Run(ctx context.Context) {
	if p.Interval == 0 {
		p.Interval = time.Minute
	}
	if p.SweepEvery == 0 {
		p.SweepEvery = time.Hour
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	last := time.Time{}
	for {
		sweep := time.Since(last) >= p.SweepEvery
		if sweep {
			last = time.Now()
		}
		p.Once(ctx, sweep)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes one pass: the due follows and rules, or every one (sweep); a sweep also lets the
// collector take landed builds the catalogue keeps no more (KeepLanded).
func (p *Placer) Once(ctx context.Context, sweep bool) {
	if sweep {
		if n, err := p.Service.TrimLanded(ctx); err != nil {
			p.Log.Error("release: trimming landed builds", "error", err)
		} else if n > 0 {
			p.Log.Info("release: landed builds left to the collector", "builds", n)
		}
	}
	follows, err := p.Service.Store.DueFollows(ctx, sweep)
	if err != nil {
		p.Log.Error("release: listing the follows to place", "error", err)
		return
	}
	rules, err := p.Service.Store.DueRules(ctx, sweep)
	if err != nil {
		p.Log.Error("release: listing the rules to place", "error", err)
		return
	}
	byFollow, byRule := map[string][]store.DueFollow{}, map[string][]store.DueRule{}
	var channels []string
	for _, f := range follows {
		if byFollow[f.ChannelID] == nil && byRule[f.ChannelID] == nil {
			channels = append(channels, f.ChannelID)
		}
		byFollow[f.ChannelID] = append(byFollow[f.ChannelID], f)
	}
	for _, r := range rules {
		if byFollow[r.ChannelID] == nil && byRule[r.ChannelID] == nil {
			channels = append(channels, r.ChannelID)
		}
		byRule[r.ChannelID] = append(byRule[r.ChannelID], r)
	}
	for _, ch := range channels {
		p.channel(ctx, ch, byFollow[ch], byRule[ch])
		if ctx.Err() != nil {
			return
		}
	}
}

func (p *Placer) channel(ctx context.Context, channelID string, follows []store.DueFollow, rules []store.DueRule) {
	lease := "placement/" + channelID
	held, err := p.Service.Store.AcquireLease(ctx, lease, p.Holder, 2*time.Minute)
	if err != nil || !held {
		if err != nil {
			p.Log.Error("release: taking the placement lease", "channel", channelID, "error", err)
		}
		return
	}
	defer func() { _ = p.Service.Store.ReleaseLease(context.WithoutCancel(ctx), lease, p.Holder) }()
	renew := func(ctx context.Context) error {
		if ok, err := p.Service.Store.AcquireLease(ctx, lease, p.Holder, 2*time.Minute); err != nil || !ok {
			return errLeaseLost
		}
		return nil
	}
	for _, f := range follows {
		if renew(ctx) != nil {
			return // the lease was lost: another replica goes on
		}
		start := p.Service.Store.Now()
		placed, skips, failure, err := p.Service.PlaceFollow(ctx, f, renew)
		if errors.Is(err, errLeaseLost) {
			return
		}
		if err != nil {
			p.Log.Error("release: placing a follow's names", "channel", channelID, "source", f.Source, "error", err)
			continue
		}
		if placed > 0 || failure != "" {
			p.Log.Info("release: placed a follow's names", "channel", channelID, "source", f.Source, "placed", placed, "failure", failure)
		}
		if err := p.Service.Store.FollowLook(ctx, f, start, start.Add(-placeSkew), skips); err != nil {
			p.Log.Error("release: recording a follow's look", "channel", channelID, "source", f.Source, "error", err)
		}
	}
	for _, r := range rules {
		if ok, err := p.Service.Store.AcquireLease(ctx, lease, p.Holder, 2*time.Minute); err != nil || !ok {
			return // the lease was lost: another replica goes on
		}
		start := p.Service.Store.Now()
		placed, failure, err := p.Service.PlaceRule(ctx, r, renew)
		if errors.Is(err, errLeaseLost) {
			return // another replica goes on
		}
		if err != nil {
			p.Log.Error("release: placing a rule's builds", "channel", r.ChannelID, "name", r.Name, "error", err)
			if failure == "" {
				failure = err.Error()
			}
		}
		if placed > 0 {
			p.Log.Info("release: placed a rule's builds", "channel", r.ChannelID, "name", r.Name, "placed", placed)
		}
		if err := p.Service.Store.SetRuleChecked(ctx, r, start, start.Add(-placeSkew), failure); err != nil {
			p.Log.Error("release: recording a rule's look", "channel", r.ChannelID, "name", r.Name, "error", err)
		}
	}
}
