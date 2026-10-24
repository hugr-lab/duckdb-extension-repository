package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: placing builds of the catalogue into a channel, by hand and by the channel's rules.

// ErrNoRule is a placement by hand under a name the channel has no rule for, by a caller who may
// not write one (a channel administrator sets the name's source first).
var ErrNoRule = errors.New("release: the channel has no rule for the name; its administrator sets the name's source first")

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
// channel, all or none (spec 0033). The caller needs promote on (channel, name); a name the channel
// has no rule for takes a channel administrator, and the placement writes its rule (manual).
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
	if errors.Is(err, store.ErrNoSource) {
		return nil, false, fmt.Errorf("%w: %q is not a source of the tenant (tenant, upstream:<an upstream>, publisher:<a publisher>)",
			store.ErrInvalid, o.Source)
	}
	if err != nil {
		return nil, false, err
	}
	rules, err := s.Store.ChannelSources(ctx, ch.ID)
	if err != nil {
		return nil, false, err
	}
	if _, ok := rules[name]; !ok {
		// the first arrival writes the name's rule: a channel administrator's choice (spec 0033)
		if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); err != nil {
			if errors.Is(err, authz.ErrDenied) {
				return nil, false, ErrNoRule
			}
			return nil, false, err
		}
	}
	builds, err := s.Store.SourceBuilds(ctx, ch.TenantID, src, name)
	if err != nil {
		return nil, false, err
	}
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return nil, false, err
	}
	var items []item
	for _, b := range builds {
		if b.ExtVersion != o.Version || servable(capis, b) != nil {
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
		return nil, false, fmt.Errorf("%w: no build of %s %s from %s that the channel serves", store.ErrNotFound, name, o.Version, o.Source)
	}
	return s.insert(ctx, a, ch.ID, items, placeEvent(ctx, name, o.Source, "hand"))
}

// placement is the item that places a catalogue build of a tenant (by name): as its arrival made its
// release (its visibility, narrowed as a private upstream's credential allows now; its provenance),
// with who placed it and how.
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
	return item{b: b, visibility: vis, origin: store.OriginPlacement, provenance: string(prov), source: source}
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

// PlaceRule places what a channel's rule takes (spec 0033): for an auto rule, per cell (platform,
// and DuckDB version or C API major) the newest build of its source the channel serves when it is
// newer than every release of the source there; for both modes, a build of a version the channel
// holds (an active or deprecated release of the source) in a cell it lacks. Never a build whose
// version a cell has or had (a yanked or purged one stays out). Each build is placed on its own; it
// returns how many were placed and the first failure's text ("" for none).
func (s *Service) PlaceRule(ctx context.Context, r store.DueRule) (int, string, error) {
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
	var cands []store.Candidate
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		cands, err = tx.SourceReleases(ctx, r.ChannelID, r.Source, r.Name)
		return err
	}); err != nil {
		return 0, "", err
	}
	type cell struct{ platform, slot string }
	had := map[[3]string]bool{} // (version, platform, slot) a release has or had
	newest := map[cell]string{} // the newest version a cell has or had
	held := map[string]bool{}   // versions the channel holds
	for _, c := range cands {
		k := cell{c.Platform, c.Slot}
		had[[3]string{c.ExtVersion, c.Platform, c.Slot}] = true
		if v, ok := newest[k]; !ok || CompareVersions(c.ExtVersion, v) > 0 {
			newest[k] = c.ExtVersion
		}
		if c.State == store.ReleaseActive || c.State == store.ReleaseDeprecated {
			held[c.ExtVersion] = true
		}
	}
	pick := map[[3]string]store.Build{}
	best := map[cell]store.Build{}
	for _, b := range builds {
		k := cell{b.Platform, b.Slot()}
		if !b.Checked || servable(capis, b) != nil || had[[3]string{b.ExtVersion, k.platform, k.slot}] {
			continue
		}
		if held[b.ExtVersion] {
			pick[[3]string{b.ExtVersion, k.platform, k.slot}] = b
			continue
		}
		if r.Mode != store.RuleAuto {
			continue
		}
		if v, ok := newest[k]; ok && CompareVersions(b.ExtVersion, v) <= 0 {
			continue
		}
		if cur, ok := best[k]; !ok || CompareVersions(b.ExtVersion, cur.ExtVersion) > 0 {
			best[k] = b
		}
	}
	for k, b := range best {
		pick[[3]string{b.ExtVersion, k.platform, k.slot}] = b
	}
	keys := make([][3]string, 0, len(pick))
	for k := range pick {
		keys = append(keys, k)
	}
	// oldest version first, so the newest placed is current
	slices.SortFunc(keys, func(x, y [3]string) int {
		if c := CompareVersions(x[0], y[0]); c != 0 {
			return c
		}
		return slices.Compare(x[1:], y[1:])
	})
	placed, failure := 0, ""
	for _, k := range keys {
		it := s.placement(tn.Name, pick[k], r.Source, placer, r.Mode, false)
		_, existed, err := s.insert(ctx, placer, r.ChannelID, []item{it}, placeEvent(ctx, r.Name, r.Source, r.Mode))
		switch {
		case err != nil && failure == "":
			failure = fmt.Sprintf("%s %s %s: %v", k[0], k[1], k[2], err)
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

// Once makes one pass: the due rules, or every rule (sweep).
func (p *Placer) Once(ctx context.Context, sweep bool) {
	rules, err := p.Service.Store.DueRules(ctx, sweep)
	if err != nil {
		p.Log.Error("release: listing the rules to place", "error", err)
		return
	}
	for i := 0; i < len(rules); {
		ch := rules[i].ChannelID
		j := i
		for j < len(rules) && rules[j].ChannelID == ch {
			j++
		}
		p.channel(ctx, rules[i:j])
		i = j
		if ctx.Err() != nil {
			return
		}
	}
}

func (p *Placer) channel(ctx context.Context, rules []store.DueRule) {
	lease := "placement/" + rules[0].ChannelID
	held, err := p.Service.Store.AcquireLease(ctx, lease, p.Holder, 2*time.Minute)
	if err != nil || !held {
		if err != nil {
			p.Log.Error("release: taking the placement lease", "channel", rules[0].ChannelID, "error", err)
		}
		return
	}
	defer func() { _ = p.Service.Store.ReleaseLease(context.WithoutCancel(ctx), lease, p.Holder) }()
	for _, r := range rules {
		if ok, err := p.Service.Store.AcquireLease(ctx, lease, p.Holder, 2*time.Minute); err != nil || !ok {
			return // the lease was lost: another replica goes on
		}
		start := p.Service.Store.Now()
		placed, failure, err := p.Service.PlaceRule(ctx, r)
		if err != nil {
			p.Log.Error("release: placing a rule's builds", "channel", r.ChannelID, "name", r.Name, "error", err)
			continue
		}
		if placed > 0 {
			p.Log.Info("release: placed a rule's builds", "channel", r.ChannelID, "name", r.Name, "placed", placed)
		}
		if err := p.Service.Store.SetRuleChecked(ctx, r.ChannelID, r.Name, r.Source, start.Add(-placeSkew), failure); err != nil {
			p.Log.Error("release: recording a rule's look", "channel", r.ChannelID, "name", r.Name, "error", err)
		}
	}
}
