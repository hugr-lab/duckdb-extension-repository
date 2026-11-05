package release

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0031: switching a name's source in a channel.

// SourceView is a name's source in a channel as answers show it: the selected source ("": none),
// who set it and when, and the sources that have releases of the name in the channel, each with
// its count. Spec 0033: the rule's mode, the newest version of the source the channel holds
// (Current, an active or deprecated release) and the newest available in the catalogue (Newest: a
// version with a held build is not, phase A2, nor one withdrawn, phase B, nor one revoked, which the
// pass never places either, phase C), and the placement pass's last outcome. Spec 0041: what waits
// under a rule with a mode (Waiting: the newest version it could take that the channel has not had,
// as the pass's candidates), and, under a rule placing automatically, when its next placement can
// happen while it waits for a hold (WaitsUntil, zero for none).
type SourceView struct {
	Name      string
	Source    string
	SetBy     string
	SetAt     time.Time
	Available map[string]int
	Mode      string
	Current   string
	Newest    string
	Outcome   string
	OutcomeAt time.Time
	// spec 0041
	Waiting    string
	WaitsUntil time.Time
}

// mayAdminChannel allows a channel's administrators (admin on the channel; tenant administrators).
func (s *Service) mayAdminChannel(ctx context.Context, a authz.Actor, tenant, channel string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); err != nil {
		return store.Channel{}, err
	}
	return s.Store.GetChannel(ctx, tenant, channel)
}

// sourceOfType refuses a rule or a follow of a channel on a source of another package type (spec
// 0034: an upstream's names are of its home channel's type; 409).
func (s *Service) sourceOfType(ctx context.Context, ch store.Channel, src, label string) error {
	t, err := s.Store.SourceType(ctx, src)
	if err != nil || t == "" || t == ch.PackageType {
		return err
	}
	return fmt.Errorf("%w: %s brings %s, the channel %s serves %s", ErrPackageType, label, t, ch.Name, ch.PackageType)
}

// Sources lists a channel's names with their sources: every name with a selected source or a
// release, by name.
func (s *Service) Sources(ctx context.Context, a authz.Actor, tenant, channel string) ([]SourceView, error) {
	return s.sources(ctx, a, tenant, channel, "")
}

// sources is Sources, of one name only ("": every name).
func (s *Service) sources(ctx context.Context, a authz.Actor, tenant, channel, only string) ([]SourceView, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return nil, err
	}
	sel, err := s.Store.ChannelSources(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	avail, err := s.Store.ChannelReleaseSources(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	labels, err := s.Store.SourceLabels(ctx, ch.TenantID)
	if err != nil {
		return nil, err
	}
	catalogue, err := s.Store.SourceVersions(ctx, ch.TenantID, ch.PackageType)
	if err != nil {
		return nil, err
	}
	held, err := s.Store.HeldVersions(ctx, ch.TenantID, ch.PackageType)
	if err != nil {
		return nil, err
	}
	withdrawn, err := s.Store.WithdrawnVersions(ctx, ch.TenantID, ch.PackageType)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Store.RevokedVersions(ctx, ch.TenantID, ch.PackageType)
	if err != nil {
		return nil, err
	}
	rels, err := s.Store.ListReleases(ctx, ch.ID, only)
	if err != nil {
		return nil, err
	}
	current := map[string]string{} // the newest version of the selected source the channel holds
	for _, r := range rels {
		if r.Source != sel[r.Name].Source || r.State != store.ReleaseActive && r.State != store.ReleaseDeprecated || r.Withdrawn {
			continue
		}
		if v, ok := current[r.Name]; !ok || CompareVersions(r.PackageVersion, v) > 0 {
			current[r.Name] = r.PackageVersion
		}
	}
	w, err := s.waitsOf(ctx, ch, only)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for n := range sel {
		names[n] = only == "" || n == only
	}
	for n := range avail {
		names[n] = only == "" || n == only
	}
	out := make([]SourceView, 0, len(names))
	for n, ok := range names {
		if !ok {
			continue
		}
		v := SourceView{Name: n, Available: map[string]int{}}
		if x, ok := sel[n]; ok {
			v.Source, v.SetBy, v.SetAt = store.Label(labels, x.Source), x.SetBy, x.SetAt
			v.Mode, v.Outcome, v.OutcomeAt, v.Current = x.Mode, x.Outcome, x.OutcomeAt, current[n]
			for _, ver := range catalogue[[2]string{x.Source, n}] {
				k := [2]string{x.Source, n}
				if _, h := held[k][ver]; h || slices.Contains(withdrawn[k], ver) || slices.Contains(revoked[k], ver) {
					continue
				}
				if v.Newest == "" || CompareVersions(ver, v.Newest) > 0 {
					v.Newest = ver
				}
			}
		}
		for src, k := range avail[n] {
			v.Available[store.Label(labels, src)] += k
		}
		if x, ok := sel[n]; ok && x.Mode != store.RuleNone {
			if err := w.of(ctx, x, &v); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y SourceView) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// waits are a channel's rules' reads for their Waiting and WaitsUntil (spec 0041), read at once
// (store.ChannelRuleReads), so a channel's answer costs the same statements however many rules it
// has; each rule's look is then the placement pass's (lookOf).
type waits struct {
	ch        store.Channel
	now       time.Time
	capis     map[string][]store.CAPI
	reads     store.RuleReads
	following map[string]bool // a follow's rules place automatically while the channel follows the source
	blocked   map[string]bool // the tenant's blocked bodies
	upstream  map[string]map[string]bool
	store     *store.Store
}

// waitsOf reads a channel's rules (of one name, or every name: "") for their waits.
func (s *Service) waitsOf(ctx context.Context, ch store.Channel, name string) (*waits, error) {
	w := &waits{ch: ch, now: s.Store.Now(), following: map[string]bool{}, upstream: map[string]map[string]bool{}, store: s.Store}
	fs, err := s.Store.Follows(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		w.following[f.Source] = true
	}
	if w.capis, err = s.Store.ChannelCAPIs(ctx, ch.ID); err != nil {
		return nil, err
	}
	if w.reads, err = s.Store.ChannelRuleReads(ctx, ch, name); err != nil {
		return nil, err
	}
	if w.blocked, err = s.Store.BlockedHashes(ctx, ch.TenantID); err != nil {
		return nil, err
	}
	return w, nil
}

// of fills a rule's Waiting and WaitsUntil: WaitsUntil only for a rule placing automatically (auto,
// or a follow's while the channel follows its source), since a channel's hold applies to automatic
// placements only.
func (w *waits) of(ctx context.Context, rule store.ChannelSource, v *SourceView) error {
	builds, rels := w.reads.Of(rule.Source, rule.Name)
	if len(builds) == 0 {
		return nil
	}
	lk := lookOf(w.ch, w.capis, builds, w.reads.Purged, rels)
	auto := rule.Mode == store.RuleAuto || rule.Mode == store.RuleFollow && w.following[rule.Source]
	var err error
	if v.Waiting, err = waitingOf(ctx, rule.Source, lk, w.now, auto, w.isBlocked); err != nil {
		return err
	}
	if auto {
		v.WaitsUntil = lk.waitsUntil(w.now)
	}
	return nil
}

// isBlocked reads an upstream's blocks once, for the first of its rules that asks.
func (w *waits) isBlocked(ctx context.Context, source, hash string) (bool, error) {
	if w.blocked[hash] {
		return true, nil
	}
	id, ok := strings.CutPrefix(source, store.SourceUpstreamPrefix)
	if !ok {
		return false, nil
	}
	ub, read := w.upstream[id]
	if !read {
		var err error
		if ub, err = w.store.UpstreamBlockedHashes(ctx, id); err != nil {
			return false, err
		}
		w.upstream[id] = ub
	}
	return ub[hash], nil
}

// Source is one name's view (Sources); a name with neither a source nor a release is not found.
func (s *Service) Source(ctx context.Context, a authz.Actor, tenant, channel, name string) (SourceView, error) {
	vs, err := s.sources(ctx, a, tenant, channel, name)
	if err != nil {
		return SourceView{}, err
	}
	for _, v := range vs {
		if v.Name == name {
			return v, nil
		}
	}
	return SourceView{}, fmt.Errorf("%w: no source or release of %s in the channel", store.ErrNotFound, name)
}

// SetSource selects the source a channel serves a name from (tenant, upstream:<name> or
// publisher:<name> of the tenant), whether it has releases there yet
// or not: its stored releases are served at once, the others' stay stored and unserved. mode is
// the rule's (spec 0033: auto or manual; "" keeps it, none for a new rule); the placement pass
// places what the rule takes. The tenant's own source takes manual rules only.
func (s *Service) SetSource(ctx context.Context, a authz.Actor, tenant, channel, name, label, mode string) (SourceView, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return SourceView{}, err
	}
	if err := ValidName(name); err != nil {
		return SourceView{}, err
	}
	if mode != "" {
		if err := store.ValidRuleMode(mode); err != nil {
			return SourceView{}, err
		}
		if ch.Kind != store.ChannelSigned {
			return SourceView{}, fmt.Errorf("%w: a %s channel's rules take no mode: its mirrors fill it", store.ErrInvalid, ch.Kind)
		}
	}
	if label == store.SourceTenant && mode == store.RuleAuto {
		return SourceView{}, fmt.Errorf("%w: the tenant's own source is everyone who may publish in it: its rules are not automatic", store.ErrInvalid)
	}
	src, err := s.Store.ResolveSource(ctx, ch.TenantID, label)
	if errors.Is(err, store.ErrNoSource) {
		return SourceView{}, fmt.Errorf("%w: %q is not a source of the tenant (tenant, upstream:<an upstream>, publisher:<a publisher>)",
			store.ErrInvalid, label)
	}
	if err != nil {
		return SourceView{}, err
	}
	if err := s.sourceOfType(ctx, ch, src, label); err != nil {
		return SourceView{}, err
	}
	err = s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		if _, err := tx.GetChannel(ctx, tenant, channel); err != nil { // a deleting tenant's refuses (spec 0019)
			return err
		}
		// the source read outside: still there under the lock (an upstream or publisher removed since)
		if label, err := tx.SourceLabel(ctx, src); err != nil {
			return err
		} else if strings.HasSuffix(label, "(removed)") {
			return fmt.Errorf("%w: %q was removed", store.ErrInvalid, label)
		}
		if mode == store.RuleNone {
			cur, _, err := tx.ChannelSourceOf(ctx, ch.ID, name)
			if err != nil {
				return err
			}
			switch {
			case src == store.SourceTenant && cur.Mode == store.RuleAuto:
				mode = store.RuleManual // a rule turned to the tenant is never automatic
			case cur.Mode == store.RuleFollow && cur.Source != src:
				mode = store.RuleManual // a follow's rule moved to another source is the administrator's now
			}
		}
		tx.SetWarningAct("source.set") // spec 0035 phase 2
		prior, err := tx.SetChannelRule(ctx, ch.ID, name, src, mode, a.String())
		if err != nil {
			return err
		}
		after := mode
		if after == store.RuleNone {
			after = prior.Mode
		}
		if prior.Source == src && prior.Mode == after {
			return nil
		}
		bl, err := tx.SourceLabel(ctx, prior.Source)
		if err != nil {
			return err
		}
		if prior.Source == "" {
			bl = ""
		}
		if err := tx.Event(ctx, ch.TenantID, a.String(), "source.set", "channel:"+ch.Name+"/ext:"+name,
			map[string]any{"name": name, "source": label, "before": bl, "arrival": false, "mode": after}); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	if err != nil {
		return SourceView{}, err
	}
	return s.Source(ctx, a, tenant, channel, name)
}

// ClearSource removes a name's source in a channel: nothing serves the name until the next
// release of it lands and sets it again.
func (s *Service) ClearSource(ctx context.Context, a authz.Actor, tenant, channel, name string) error {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		if _, err := tx.GetChannel(ctx, tenant, channel); err != nil { // a deleting tenant's refuses (spec 0019)
			return err
		}
		before, err := tx.ClearChannelSource(ctx, ch.ID, name)
		if err != nil {
			return err
		}
		if before == "" {
			return fmt.Errorf("%w: %s has no source in the channel", store.ErrNotFound, name)
		}
		bl, err := tx.SourceLabel(ctx, before)
		if err != nil {
			return err
		}
		if err := tx.Event(ctx, ch.TenantID, a.String(), "source.clear", "channel:"+ch.Name+"/ext:"+name,
			map[string]any{"name": name, "before": bl}); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
}
