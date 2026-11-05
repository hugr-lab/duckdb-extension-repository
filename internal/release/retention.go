package release

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: version retention in channels. A rule (the tenant's default, a channel's own) keeps the
// newest N versions of each name per DuckDB version the channel serves (a line), always its current
// ones and the versions pinned for it; the pass retires the others and purges them after a grace.

// retainer is the retention pass's actor (system:retention is the events pruner's, spec 0010).
var retainer = authz.Actor{Kind: authz.ActorSystem, ID: "release-retention"}

// Where a field of a rule in force comes from.
const (
	FromChannel = "channel"
	FromTenant  = "tenant"
)

// RuleInForce is a channel's rule as the pass applies it: each field's value and where it comes from
// ("" for no value anywhere: no rule in force). None is a channel's keep of none. A grace above the
// ceiling (limits.ceilings.retention_grace) is capped (Capped), the stored value kept.
type RuleInForce struct {
	Keep      int
	KeepFrom  string
	None      bool
	Grace     time.Duration
	GraceFrom string
	Capped    bool
	// Passthrough is a passthrough channel's: it takes no retention, whatever the tenant's default.
	Passthrough bool
}

// Active reports whether a rule is in force: a keep (not none) and a grace, each from somewhere.
func (r RuleInForce) Active() bool { return !r.None && r.KeepFrom != "" && r.GraceFrom != "" }

// Missing names the fields with no value anywhere.
func (r RuleInForce) Missing() []string {
	var out []string
	if r.KeepFrom == "" {
		out = append(out, "keep")
	}
	if r.GraceFrom == "" {
		out = append(out, "grace")
	}
	return out
}

// InForce is a channel's rule in force from its own and its tenant's, each field on its own.
func InForce(ch, tn store.RetentionRule) RuleInForce {
	var r RuleInForce
	switch {
	case ch.Keep != nil && *ch.Keep == 0:
		r.None, r.KeepFrom = true, FromChannel
	case ch.Keep != nil:
		r.Keep, r.KeepFrom = *ch.Keep, FromChannel
	case tn.Keep != nil:
		r.Keep, r.KeepFrom = *tn.Keep, FromTenant
	}
	switch {
	case ch.Grace != nil:
		r.Grace, r.GraceFrom = *ch.Grace, FromChannel
	case tn.Grace != nil:
		r.Grace, r.GraceFrom = *tn.Grace, FromTenant
	}
	if r.GraceFrom != "" && r.Grace > store.MaxRetentionGrace {
		r.Grace, r.Capped = store.MaxRetentionGrace, true
	}
	return r
}

// counted reports whether a release counts in a line: active or deprecated, its entry not withdrawn
// (yanked, retired and withdrawn entries' releases are neither counted nor touched).
func counted(c *store.Candidate) bool {
	return (c.State == store.ReleaseActive || c.State == store.ReleaseDeprecated) && !c.Withdrawn
}

// sortedLines are a channel's DuckDB versions in version order.
func sortedLines(capis map[string][]store.CAPI) []string {
	out := make([]string, 0, len(capis))
	for v := range capis {
		out = append(out, v)
	}
	slices.SortFunc(out, CompareVersions)
	return out
}

// linesOf are the lines of capis a release serves.
func linesOf(c *store.Candidate, capis map[string][]store.CAPI) []string {
	var out []string
	for _, l := range sortedLines(capis) {
		if Serves(c, l, capis) {
			out = append(out, l)
		}
	}
	return out
}

// pinned reports whether a pin keeps (name, version) in one of lines (a pin for every line keeps it
// in any).
func pinned(pins []store.RetentionPin, name, version string, lines []string) bool {
	return slices.ContainsFunc(pins, func(p store.RetentionPin) bool {
		return p.Name == name && p.PackageVersion == version && (p.DuckDBVersion == "" || slices.Contains(lines, p.DuckDBVersion))
	})
}

// pinLines are the channel's lines with those its pins name that it no longer serves, each by the
// DuckDB version's own C API maxima (a version the server no longer has: none, so only a build of its
// slot serves it): a pin keeps a release serving its line, and goes only once none is left there,
// whatever the channel serves now.
func pinLines(capis map[string][]store.CAPI, pins []store.RetentionPin, maxima func(string) ([]store.CAPI, error)) (map[string][]store.CAPI, error) {
	out := maps.Clone(capis)
	for _, p := range pins {
		if _, ok := out[p.DuckDBVersion]; ok || p.DuckDBVersion == "" {
			continue
		}
		m, err := maxima(p.DuckDBVersion)
		if errors.Is(err, store.ErrNotFound) {
			m, err = []store.CAPI{}, nil
		}
		if err != nil {
			return nil, err
		}
		out[p.DuckDBVersion] = m
	}
	return out, nil
}

// groupResult is what retention computes for one (source, name)'s releases in a channel.
type groupResult struct {
	retire []*store.Candidate  // retirable in every line it serves, oldest first
	lines  map[string][]string // release id → the lines it serves
	kept   map[string][]string // line → its kept versions, newest first
	keptBy map[string][]string // release id → the lines keeping a release retirable in another
	order  map[string][]string // line → its versions, newest first by first release
}

// computeGroup computes one (source, name)'s retention in a channel's lines: per line, its versions
// (of its counted releases serving the line, a version's platforms once) ordered newest first by the
// version's first release in the line; kept are its current versions (the flat path's on each
// platform in the full and the public view, by the source's own releases), the newest keep, and those
// pinned for it; keepAll keeps every version (no rule in force: the line act's other lines). A
// release serving no line is left alone.
func computeGroup(rels []*store.Candidate, capis map[string][]store.CAPI, pins []store.RetentionPin, keep int, keepAll bool) groupResult {
	g := groupResult{lines: map[string][]string{}, kept: map[string][]string{}, keptBy: map[string][]string{}, order: map[string][]string{}}
	keptSet := map[string]map[string]bool{}
	for _, l := range sortedLines(capis) {
		first := map[string]time.Time{}
		var in []*store.Candidate
		for _, c := range rels {
			if !counted(c) || !Serves(c, l, capis) {
				continue
			}
			in = append(in, c)
			if f, ok := first[c.PackageVersion]; !ok || c.CreatedAt.Before(f) {
				first[c.PackageVersion] = c.CreatedAt
			}
		}
		versions := make([]string, 0, len(first))
		for v := range first {
			versions = append(versions, v)
		}
		slices.SortFunc(versions, func(a, b string) int {
			if c := first[b].Compare(first[a]); c != 0 {
				return c
			}
			return CompareVersions(b, a)
		})
		g.order[l] = versions
		k := map[string]bool{}
		for i, v := range versions {
			if keepAll || i < keep {
				k[v] = true
			}
		}
		// the current versions: per platform and view, the active release with a seq of the highest seq
		for _, vis := range []Visible{Everything, PublicOnly} {
			best := map[string]*store.Candidate{}
			for _, c := range in {
				if c.State != store.ReleaseActive || c.Seq == 0 || !vis(c) {
					continue
				}
				if b := best[c.Platform]; b == nil || c.Seq > b.Seq {
					best[c.Platform] = c
				}
			}
			for _, c := range best {
				k[c.PackageVersion] = true
			}
		}
		for _, v := range versions {
			if pinned(pins, nameOf(rels), v, []string{l}) {
				k[v] = true
			}
		}
		keptSet[l] = k
		for _, v := range versions {
			if k[v] {
				g.kept[l] = append(g.kept[l], v)
			}
		}
	}
	for _, c := range rels {
		if !counted(c) {
			continue
		}
		ls := linesOf(c, capis)
		if len(ls) == 0 {
			continue
		}
		g.lines[c.ID] = ls
		var keeping []string
		for _, l := range ls {
			if keptSet[l][c.PackageVersion] {
				keeping = append(keeping, l)
			}
		}
		switch {
		case len(keeping) == 0:
			g.retire = append(g.retire, c)
		case len(keeping) < len(ls):
			g.keptBy[c.ID] = keeping
		}
	}
	return g
}

func nameOf(rels []*store.Candidate) string {
	if len(rels) == 0 {
		return ""
	}
	return rels[0].Name
}

// groups splits releases by (source, name), every state, in their order.
func groups(rels []store.Candidate) ([][2]string, map[[2]string][]*store.Candidate) {
	by := map[[2]string][]*store.Candidate{}
	var keys [][2]string
	for i := range rels {
		k := [2]string{rels[i].Source, rels[i].Name}
		if by[k] == nil {
			keys = append(keys, k)
		}
		by[k] = append(by[k], &rels[i])
	}
	slices.SortFunc(keys, func(a, b [2]string) int { return cmp.Or(strings.Compare(a[1], b[1]), strings.Compare(a[0], b[0])) })
	return keys, by
}

// retireItem is one (source, name, version) the pass retires: the releases, the lines they serve,
// each line's kept versions (why), and the version's releases another line keeps.
type retireItem struct {
	source, name, version string
	releases              []store.Candidate
	lines                 []string
	kept                  map[string][]string
	keptBy                map[string][]string // release id → lines
}

// purgeItem is a retired release: the lines it serves, when it will be purged (zero: no grace in
// force) and whether a pin keeps it.
type purgeItem struct {
	release store.Candidate
	lines   []string
	purgeAt time.Time
	pinned  bool
}

// retentionLook is what the pass reads and computes of a channel at once (the plan shows it).
type retentionLook struct {
	ch      store.Channel
	rule    RuleInForce
	checked time.Time
	retire  []retireItem
	purge   []purgeItem // past the grace: those pinned are kept
	inGrace []purgeItem
}

// look reads a channel's retention: its rule in force, releases, lines and pins, at now.
func (s *Service) look(ctx context.Context, ch store.Channel, now time.Time) (retentionLook, error) {
	lk := retentionLook{ch: ch}
	tn, err := s.Store.TenantRetention(ctx, ch.TenantID)
	if err != nil {
		return lk, err
	}
	own, checked, err := s.Store.ChannelRetention(ctx, ch.ID)
	if err != nil {
		return lk, err
	}
	lk.rule, lk.checked = InForce(own, tn), checked
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return lk, err
	}
	rels, err := s.Store.ListReleases(ctx, ch.ID, "")
	if err != nil {
		return lk, err
	}
	slices.SortStableFunc(rels, func(a, b store.Candidate) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	pins, err := s.Store.ChannelPins(ctx, ch.ID)
	if err != nil {
		return lk, err
	}
	pl, err := pinLines(capis, pins, func(v string) ([]store.CAPI, error) { return s.Store.DuckDBVersionCAPIs(ctx, v) })
	if err != nil {
		return lk, err
	}
	keys, by := groups(rels)
	if lk.rule.Active() {
		for _, k := range keys {
			g := computeGroup(by[k], capis, pins, lk.rule.Keep, false)
			items := map[string]*retireItem{}
			var order []string
			for _, c := range g.retire {
				it := items[c.PackageVersion]
				if it == nil {
					it = &retireItem{source: k[0], name: k[1], version: c.PackageVersion, kept: map[string][]string{}, keptBy: map[string][]string{}}
					items[c.PackageVersion] = it
					order = append(order, c.PackageVersion)
				}
				it.releases = append(it.releases, *c)
				for _, l := range g.lines[c.ID] {
					if !slices.Contains(it.lines, l) {
						it.lines = append(it.lines, l)
					}
					it.kept[l] = g.kept[l]
				}
			}
			for _, c := range by[k] {
				if ls, ok := g.keptBy[c.ID]; ok && items[c.PackageVersion] != nil {
					items[c.PackageVersion].keptBy[c.ID] = ls
				}
			}
			for _, v := range order {
				slices.SortFunc(items[v].lines, CompareVersions)
				lk.retire = append(lk.retire, *items[v])
			}
		}
	}
	for i := range rels {
		c := &rels[i]
		if c.State != store.ReleaseRetired || c.Withdrawn {
			continue
		}
		it := purgeItem{release: *c, lines: linesOf(c, capis)}
		it.pinned = pinned(pins, c.Name, c.PackageVersion, linesOf(c, pl))
		if lk.rule.Active() {
			it.purgeAt = c.RetiredAt.Add(lk.rule.Grace)
		}
		if lk.rule.Active() && !now.Before(it.purgeAt) {
			lk.purge = append(lk.purge, it)
		} else {
			lk.inGrace = append(lk.inGrace, it)
		}
	}
	return lk, nil
}

// RetentionResult is a pass's outcome in a channel: how many releases it retired and purged, how
// many retirements or purges failed and the first failure's text; NoRule: no rule in force (nothing
// done); PurgeOff: purges are off (gc.interval is 0).
type RetentionResult struct {
	Retired, Purged, Failed int
	Failure                 string
	NoRule, PurgeOff        bool
}

func (r *RetentionResult) fail(what string, err error) {
	r.Failed++
	if r.Failure == "" {
		r.Failure = what + ": " + err.Error()
	}
}

// RetainChannel runs the retention pass in a signed channel (spec 0042): each (name, version) it
// retires and each release it purges in a short transaction of its own under the channel lock,
// re-checking there what its look found (a release changed, made current or pinned meanwhile is
// left), renewing the caller's lease first (renew may be nil). A failure is counted (the first's
// text kept for the caller's log), and the pass goes on; retention.run sums up a pass that changed
// something or failed. The look is recorded (retention_checked_at).
func (s *Service) RetainChannel(ctx context.Context, channelID string, renew func(context.Context) error) (RetentionResult, error) {
	var res RetentionResult
	ch, err := s.channelByID(ctx, channelID)
	if err != nil {
		return res, err
	}
	start := s.Store.Now()
	if ch.Kind != store.ChannelSigned {
		return res, nil
	}
	lk, err := s.look(ctx, ch, start)
	if err != nil {
		return res, err // fail closed: nothing retired in the channel
	}
	res.NoRule, res.PurgeOff = !lk.rule.Active(), s.PurgeOff
	var stopped error // the lease lost or the context ended: what was done is still summed up, the look not recorded
	if lk.rule.Active() {
	retire:
		for _, its := range byVersion(lk.retire) {
			if renew != nil {
				if stopped = renew(ctx); stopped != nil {
					break retire
				}
			}
			if s.beforeRetire != nil {
				s.beforeRetire()
			}
			n, err := s.retireVersion(ctx, ch, its)
			if err != nil {
				if stopped = ctx.Err(); stopped != nil {
					break retire // stopped, not failed
				}
				res.fail(its[0].name+" "+its[0].version, err)
				continue
			}
			res.Retired += n
		}
		for _, it := range lk.purge {
			if stopped != nil {
				break
			}
			if it.pinned || s.PurgeOff {
				continue
			}
			if renew != nil {
				if stopped = renew(ctx); stopped != nil {
					break
				}
			}
			done, err := s.purgeRetired(ctx, ch, it.release)
			if err != nil {
				if stopped = ctx.Err(); stopped != nil {
					break // stopped, not failed
				}
				res.fail("release "+it.release.ID, err)
				continue
			}
			if done {
				res.Purged++
			}
		}
	}
	wctx := context.WithoutCancel(ctx) // a pass stopped by its context still sums up what it did
	err = s.Store.InTenantTx(wctx, ch.TenantID, nil, func(tx *store.Tx) error {
		if res.Retired+res.Purged+res.Failed > 0 {
			if err := tx.Event(wctx, ch.TenantID, retainer.String(), "retention.run", "channel:"+ch.Name,
				map[string]any{"retired": res.Retired, "purged": res.Purged, "failed": res.Failed}); err != nil {
				return err
			}
		}
		if stopped != nil {
			return nil
		}
		return tx.SetRetentionChecked(wctx, ch.ID, start)
	})
	return res, cmp.Or(stopped, err)
}

// ruleTx reads a channel's rule in force under the caller's lock.
func ruleTx(ctx context.Context, tx *store.Tx, ch store.Channel) (RuleInForce, error) {
	own, _, err := tx.ChannelRetention(ctx, ch.ID)
	if err != nil {
		return RuleInForce{}, err
	}
	tn, err := tx.TenantRetention(ctx, ch.TenantID)
	return InForce(own, tn), err
}

// byVersion groups a look's retirements by (name, version), every source's together, in the look's
// order: one transaction and one release.retire each.
func byVersion(items []retireItem) [][]retireItem {
	var out [][]retireItem
	at := map[[2]string]int{}
	for _, it := range items {
		k := [2]string{it.name, it.version}
		if i, ok := at[k]; ok {
			out[i] = append(out[i], it)
			continue
		}
		at[k] = len(out)
		out = append(out, []retireItem{it})
	}
	return out
}

// retireVersion retires a look's (name, version), of each source the look found it retirable in,
// under the channel lock: only its releases that are as the look read them and still retirable
// there, with the rule, releases, lines and pins as they are now; one release.retire for them all. It
// answers how many it retired.
func (s *Service) retireVersion(ctx context.Context, ch store.Channel, its []retireItem) (int, error) {
	n := 0
	name, version := its[0].name, its[0].version
	err := s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		n = 0
		rule, err := ruleTx(ctx, tx, ch)
		if err != nil || !rule.Active() {
			return err
		}
		capis, err := tx.ChannelCAPIs(ctx, ch.ID)
		if err != nil {
			return err
		}
		pins, err := tx.ChannelPins(ctx, ch.ID)
		if err != nil {
			return err
		}
		var ids, lines []string
		for _, it := range its {
			rels, err := tx.SourceReleases(ctx, ch.ID, it.source, it.name)
			if err != nil {
				return err
			}
			var group []*store.Candidate
			for i := range rels {
				if rels[i].State != store.ReleasePurged {
					group = append(group, &rels[i])
				}
			}
			g := computeGroup(group, capis, pins, rule.Keep, false)
			looked := map[string]int64{}
			for _, r := range it.releases {
				looked[r.ID] = r.Version
			}
			for _, c := range g.retire {
				if v, ok := looked[c.ID]; !ok || v != c.Version || c.PackageVersion != version {
					continue // not in the look, or changed since (restored, made current, deprecated)
				}
				if err := tx.RetireRelease(ctx, &c.Release, retainer.String()); err != nil {
					return err
				}
				ids = append(ids, c.ID)
				for _, l := range g.lines[c.ID] {
					if !slices.Contains(lines, l) {
						lines = append(lines, l)
					}
				}
			}
		}
		if len(ids) == 0 {
			return nil
		}
		slices.SortFunc(lines, CompareVersions)
		n = len(ids)
		if err := retireEvent(ctx, tx, ch, retainer.String(), name, version, ids, lines, "retention"); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return n, err
}

// retireEvent writes release.retire: one per (name, version) per channel, by the pass (retention)
// or the line act (line).
func retireEvent(ctx context.Context, tx *store.Tx, ch store.Channel, actor, name, version string, ids, lines []string, by string) error {
	return tx.Event(ctx, ch.TenantID, actor, "release.retire", "channel:"+ch.Name+"/ext:"+name,
		map[string]any{"version": version, "releases": ids, "lines": lines, "by": by})
}

// purgeRetired purges a look's retired release under the channel lock, as an administrator's purge
// does (spec 0016), when it is as the look read it, still retired past the grace in force, and no pin
// keeps it; the pins its version leaves without a release in their line go. It reports whether it
// purged.
func (s *Service) purgeRetired(ctx context.Context, ch store.Channel, looked store.Candidate) (bool, error) {
	done := false
	err := s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		done = false
		rule, err := ruleTx(ctx, tx, ch)
		if err != nil || !rule.Active() {
			return err
		}
		c, err := tx.GetCandidate(ctx, ch.ID, looked.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if c.State != store.ReleaseRetired || c.Version != looked.Version || c.Withdrawn || tx.Now().Before(c.RetiredAt.Add(rule.Grace)) {
			return nil
		}
		capis, err := tx.ChannelCAPIs(ctx, ch.ID)
		if err != nil {
			return err
		}
		pins, err := tx.ChannelPins(ctx, ch.ID)
		if err != nil {
			return err
		}
		pl, err := pinLines(capis, pins, func(v string) ([]store.CAPI, error) { return tx.DuckDBVersionCAPIs(ctx, v) })
		if err != nil {
			return err
		}
		if pinned(pins, c.Name, c.PackageVersion, linesOf(&c, pl)) {
			return nil
		}
		if err := tx.PurgeRelease(ctx, c); err != nil {
			return err
		}
		dropped, err := dropPins(ctx, tx, ch.ID, c.Name, c.PackageVersion, pl, pins)
		if err != nil {
			return err
		}
		f := purgeFields(c, dropped)
		f["reason"] = "retention"
		if err := tx.Event(ctx, ch.TenantID, retainer.String(), "release.purge", releaseSubject(ch, c.Release), f); err != nil {
			return err
		}
		done = true
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return done, err
}

// dropPins removes, after a purge of a release of (name, version), the version's pins left without a
// release in their line (a pin for every line: in the channel), so pins stay bounded by what the
// channel holds; its lines are pinLines', so a line the channel no longer serves counts by its maxima.
// It answers the pins it removed (release.purge's pins_removed).
func dropPins(ctx context.Context, tx *store.Tx, channelID, name, version string, capis map[string][]store.CAPI, pins []store.RetentionPin) ([]string, error) {
	var mine []store.RetentionPin
	for _, p := range pins {
		if p.Name == name && p.PackageVersion == version {
			mine = append(mine, p)
		}
	}
	if len(mine) == 0 {
		return nil, nil
	}
	left, err := tx.VersionReleases(ctx, channelID, name, version)
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, p := range mine {
		held := slices.ContainsFunc(left, func(c store.Candidate) bool {
			return p.DuckDBVersion == "" || Serves(&c, p.DuckDBVersion, capis)
		})
		if !held {
			gone = append(gone, p.ID)
		}
	}
	return gone, tx.DeletePins(ctx, channelID, gone)
}

// purgeFields are release.purge's data: the release, and the pins the purge removed, when any.
func purgeFields(c store.Candidate, dropped []string) map[string]any {
	f := map[string]any{"release": c.ID, "name": c.Name, "version": c.PackageVersion, "platform": c.Platform, "slot": c.Slot,
		"body_hash": c.BodyHash}
	if len(dropped) > 0 {
		f["pins_removed"] = dropped
	}
	return f
}

var errNoRetention = fmt.Errorf("%w: a passthrough channel serves DuckDB's core builds as its upstream has them: it takes no retention", store.ErrInvalid)
