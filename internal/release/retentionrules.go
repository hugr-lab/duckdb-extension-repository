package release

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: the retention rules, pins, the line act and the plan, as administrators reach them.

// RetentionTag is a stored rule's ETag (a PUT's If-Match).
func RetentionTag(r store.RetentionRule) string {
	k, g := "-", "-"
	if r.Keep != nil {
		k = strconv.Itoa(*r.Keep)
	}
	if r.Grace != nil {
		g = strconv.FormatInt(int64(*r.Grace/time.Second), 10)
	}
	return `"r` + k + `.` + g + `"`
}

// RuleFields are a stored rule as events show it: keep (a number, "none") and grace, each only when set.
func RuleFields(r store.RetentionRule) map[string]any {
	out := map[string]any{}
	if r.Keep != nil {
		out["keep"] = *r.Keep
		if *r.Keep == 0 {
			out["keep"] = "none"
		}
	}
	if r.Grace != nil {
		out["grace"] = audit.Duration(*r.Grace)
	}
	return out
}

// InForceFields are a rule in force as answers and events show it: each field's value and from (a
// field with no value is null), the fields missing, and capped when the ceiling caps the grace.
func InForceFields(r RuleInForce) map[string]any {
	missing := r.Missing()
	if missing == nil {
		missing = []string{}
	}
	out := map[string]any{"keep": nil, "grace": nil, "missing": missing}
	switch {
	case r.None:
		out["keep"] = map[string]any{"value": "none", "from": r.KeepFrom}
	case r.KeepFrom != "":
		out["keep"] = map[string]any{"value": r.Keep, "from": r.KeepFrom}
	}
	if r.GraceFrom != "" {
		g := map[string]any{"value": audit.Duration(r.Grace), "from": r.GraceFrom}
		if r.Capped {
			g["capped"] = true
		}
		out["grace"] = g
	}
	return out
}

// checkGrace refuses a grace above the ceiling where it changes (spec 0038's rule: an unchanged value
// sent again passes).
func checkGrace(before, after store.RetentionRule) error {
	if after.Grace == nil || *after.Grace <= store.MaxRetentionGrace || before.Grace != nil && *before.Grace == *after.Grace {
		return nil
	}
	return fmt.Errorf("%w: grace is at most limits.ceilings.retention_grace (%s)", store.ErrInvalid, audit.Duration(store.MaxRetentionGrace))
}

// TenantRetention reads a tenant's default rule: its administrators and auditors.
func (s *Service) TenantRetention(ctx context.Context, a authz.Actor, tenant string) (store.RetentionRule, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant}); err != nil {
		return store.RetentionRule{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.RetentionRule{}, err
	}
	return s.Store.TenantRetention(ctx, t.ID)
}

// SetTenantRetention sets a tenant's default rule, the whole rule (a field left out is unset): its
// administrators. expected, when set, is the rule's ETag. Nothing changed writes nothing.
func (s *Service) SetTenantRetention(ctx context.Context, a authz.Actor, tenant string, r store.RetentionRule, expected string) (store.RetentionRule, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.RetentionRule{}, err
	}
	if err := store.ValidRetention(r, false); err != nil {
		return store.RetentionRule{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.RetentionRule{}, err
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, nil, func(tx *store.Tx) error {
		before, err := tx.TenantRetention(ctx, t.ID)
		if err != nil {
			return err
		}
		if expected != "" && expected != RetentionTag(before) {
			return store.ErrConflict
		}
		if before.Equal(r) {
			return nil
		}
		if err := checkGrace(before, r); err != nil {
			return err
		}
		if err := tx.SetTenantRetention(ctx, t.ID, r); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "retention.set", "tenant:"+t.Name,
			map[string]any{"before": RuleFields(before), "after": RuleFields(r)})
	})
	return r, err
}

// ChannelRetention is a channel's rule: its own, the rule in force, when the pass last looked and
// when it looks next (zero: never, or no scheduled pass).
type ChannelRetention struct {
	Rule               store.RetentionRule
	InForce            RuleInForce
	CheckedAt, NextRun time.Time
}

// mayReadChannel admits a channel's administrators and the tenant's auditors.
func (s *Service) mayReadChannel(ctx context.Context, a authz.Actor, tenant, channel string) (store.Channel, error) {
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel})
	if errors.Is(err, authz.ErrDenied) {
		err = s.Authz.Allow(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant})
	}
	if err != nil {
		return store.Channel{}, err
	}
	return s.Store.GetChannel(ctx, tenant, channel)
}

func (s *Service) channelRetention(ctx context.Context, ch store.Channel) (ChannelRetention, error) {
	own, checked, err := s.Store.ChannelRetention(ctx, ch.ID)
	if err != nil {
		return ChannelRetention{}, err
	}
	tn, err := s.Store.TenantRetention(ctx, ch.TenantID)
	if err != nil {
		return ChannelRetention{}, err
	}
	out := ChannelRetention{Rule: own, InForce: InForce(own, tn), CheckedAt: checked}
	if s.RetentionInterval > 0 && !checked.IsZero() {
		out.NextRun = checked.Add(s.RetentionInterval)
	}
	return out, nil
}

// ChannelRetention reads a channel's rule: its administrators and the tenant's auditors.
func (s *Service) ChannelRetention(ctx context.Context, a authz.Actor, tenant, channel string) (ChannelRetention, error) {
	ch, err := s.mayReadChannel(ctx, a, tenant, channel)
	if err != nil {
		return ChannelRetention{}, err
	}
	return s.channelRetention(ctx, ch)
}

// SetChannelRetention sets a signed channel's own rule, the whole rule (a field left out is the
// tenant's; keep 0 is none there): its administrators. expected, when set, is the rule's ETag.
// Nothing changed writes nothing.
func (s *Service) SetChannelRetention(ctx context.Context, a authz.Actor, tenant, channel string, r store.RetentionRule, expected string) (ChannelRetention, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return ChannelRetention{}, err
	}
	if ch.Kind != store.ChannelSigned {
		return ChannelRetention{}, errNoRetention
	}
	if err := store.ValidRetention(r, true); err != nil {
		return ChannelRetention{}, err
	}
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		before, _, err := tx.ChannelRetention(ctx, ch.ID)
		if err != nil {
			return err
		}
		if expected != "" && expected != RetentionTag(before) {
			return store.ErrConflict
		}
		if before.Equal(r) {
			return nil
		}
		if err := checkGrace(before, r); err != nil {
			return err
		}
		if err := tx.SetChannelRetention(ctx, ch.ID, r); err != nil {
			return err
		}
		tn, err := tx.TenantRetention(ctx, ch.TenantID)
		if err != nil {
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "retention.set", "channel:"+ch.Name,
			map[string]any{"before": RuleFields(before), "after": RuleFields(r), "in_force": InForceFields(InForce(r, tn))})
	})
	if err != nil {
		return ChannelRetention{}, err
	}
	return s.channelRetention(ctx, ch)
}

// --- pins ---

// Pins lists a channel's pins (of one name, "" every one) after a cursor (store.PinKey), up to
// limit: its administrators and the tenant's auditors.
func (s *Service) Pins(ctx context.Context, a authz.Actor, tenant, channel, name, after string, limit int) ([]store.RetentionPin, error) {
	ch, err := s.mayReadChannel(ctx, a, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ListPins(ctx, ch.ID, name, after, limit)
}

// Pin keeps (name, version) in one line of a signed channel, or (DuckDBVersion "") in every one,
// whatever the rule: its administrators. The version must have a release in the channel, and the
// line be one a release of it serves. It answers the pin and whether it existed already.
func (s *Service) Pin(ctx context.Context, a authz.Actor, tenant, channel string, p store.RetentionPin) (store.RetentionPin, bool, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return store.RetentionPin{}, false, err
	}
	if ch.Kind != store.ChannelSigned {
		return store.RetentionPin{}, false, errNoRetention
	}
	if p.Name == "" || p.PackageVersion == "" {
		return store.RetentionPin{}, false, fmt.Errorf("%w: a pin names a name and a version", store.ErrInvalid)
	}
	p.ChannelID, p.PinnedBy = ch.ID, a.String()
	existed := false
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		rels, err := tx.VersionReleases(ctx, ch.ID, p.Name, p.PackageVersion)
		if err != nil {
			return err
		}
		if len(rels) == 0 {
			return fmt.Errorf("%w: %s %s has no release in the channel", store.ErrInvalid, p.Name, p.PackageVersion)
		}
		if p.DuckDBVersion != "" {
			capis, err := tx.ChannelCAPIs(ctx, ch.ID)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(rels, func(c store.Candidate) bool { return Serves(&c, p.DuckDBVersion, capis) }) {
				return fmt.Errorf("%w: no release of %s %s serves DuckDB %s in the channel", store.ErrInvalid, p.Name, p.PackageVersion, p.DuckDBVersion)
			}
		}
		if existed, err = tx.InsertPin(ctx, &p); err != nil || existed {
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "retention.pin", "channel:"+ch.Name+"/ext:"+p.Name, pinFields(p))
	})
	return p, existed, err
}

func pinFields(p store.RetentionPin) map[string]any {
	return map[string]any{"id": p.ID, "version": p.PackageVersion, "duckdb_version": p.DuckDBVersion, "reason": p.Reason}
}

// Unpin removes a channel's pin: its administrators (another channel's id is not found).
func (s *Service) Unpin(ctx context.Context, a authz.Actor, tenant, channel, id string) error {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		p, err := tx.DeletePin(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "retention.unpin", "channel:"+ch.Name+"/ext:"+p.Name, pinFields(p))
	})
}

// --- the line act ---

// LineRelease is a release the line act retired or kept.
type LineRelease struct {
	ID, Name, Version, Platform string
	Lines                       []string // kept: the other lines keeping it
}

// LineResult is what the line act did.
type LineResult struct {
	Retired, Kept          []LineRelease
	Withdrawn, PinsRemoved int
}

// RetireLine retires a DuckDB version's line in a signed channel (spec 0042), of one name or every
// one: every active or deprecated release that serves it (by the version's own C API maxima for a
// C API build, its slot for another: the channel need not serve the version any more) and that no
// other line of the channel keeps (with no rule in force, another line keeps every version), current
// and pinned versions included; the line's pins go (a pin for every line stays). Withdrawn entries'
// releases are skipped and counted. Its administrators; one transaction under the channel lock.
func (s *Service) RetireLine(ctx context.Context, a authz.Actor, tenant, channel, duckdbVersion, name string) (LineResult, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return LineResult{}, err
	}
	if ch.Kind != store.ChannelSigned {
		return LineResult{}, errNoRetention
	}
	maxima, err := s.Store.DuckDBVersionCAPIs(ctx, duckdbVersion)
	if errors.Is(err, store.ErrNotFound) {
		return LineResult{}, fmt.Errorf("%w: %q is not a DuckDB version of this server", store.ErrInvalid, duckdbVersion)
	}
	if err != nil {
		return LineResult{}, err
	}
	var res LineResult
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		res = LineResult{}
		rule, err := ruleTx(ctx, tx, ch)
		if err != nil {
			return err
		}
		capis, err := tx.ChannelCAPIs(ctx, ch.ID)
		if err != nil {
			return err
		}
		rels, err := tx.ChannelCandidates(ctx, ch.ID, name)
		if err != nil {
			return err
		}
		pins, err := tx.ChannelPins(ctx, ch.ID)
		if err != nil {
			return err
		}
		line := map[string][]store.CAPI{duckdbVersion: maxima}
		others := map[string][]store.CAPI{}
		for v, m := range capis {
			if v != duckdbVersion {
				others[v] = m
			}
		}
		keys, by := groups(rels)
		type version struct{ name, version string }
		retired := map[version][]string{}
		var order []version
		for _, k := range keys {
			g := computeGroup(by[k], others, pins, rule.Keep, !rule.Active())
			for _, c := range by[k] {
				if c.State != store.ReleaseActive && c.State != store.ReleaseDeprecated || !Serves(c, duckdbVersion, line) {
					continue
				}
				if c.Withdrawn {
					res.Withdrawn++
					continue
				}
				var keeping []string
				for _, l := range linesOf(c, others) {
					if slices.Contains(g.kept[l], c.PackageVersion) {
						keeping = append(keeping, l)
					}
				}
				lr := LineRelease{ID: c.ID, Name: c.Name, Version: c.PackageVersion, Platform: c.Platform, Lines: keeping}
				if len(keeping) > 0 {
					res.Kept = append(res.Kept, lr)
					continue
				}
				if err := tx.RetireRelease(ctx, &c.Release, a.String()); err != nil {
					return err
				}
				lr.Lines = nil
				res.Retired = append(res.Retired, lr)
				v := version{c.Name, c.PackageVersion}
				if retired[v] == nil {
					order = append(order, v)
				}
				retired[v] = append(retired[v], c.ID)
			}
		}
		var gone []string
		for _, p := range pins {
			if p.DuckDBVersion == duckdbVersion && (name == "" || p.Name == name) {
				gone = append(gone, p.ID)
			}
		}
		if err := tx.DeletePins(ctx, ch.ID, gone); err != nil {
			return err
		}
		res.PinsRemoved = len(gone)
		for _, v := range order {
			if err := retireEvent(ctx, tx, ch, a.String(), v.name, v.version, retired[v], []string{duckdbVersion}, "line"); err != nil {
				return err
			}
		}
		f := map[string]any{"duckdb_version": duckdbVersion, "retired": len(res.Retired), "kept": len(res.Kept),
			"withdrawn": res.Withdrawn, "pins_removed": res.PinsRemoved}
		if name != "" {
			f["name"] = name
		}
		if err := tx.Event(ctx, ch.TenantID, a.String(), "retention.line", "channel:"+ch.Name, f); err != nil {
			return err
		}
		if len(res.Retired) == 0 {
			return nil
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return res, err
}

// --- the plan ---

// PlanRelease is a release in the plan, with its body: its length, and whether the purge would not
// free it in the tenant (shared: another release of the tenant's channels or a landed catalogue build
// holds it), naming the holders among the channels the caller administers and the catalogue
// (SharedBy) and counting the others (Others).
type PlanRelease struct {
	ID, Platform, Slot, BodyHash string
	Bytes                        int64
	Shared                       bool
	SharedBy                     []string
	Others                       int
	// Lines are, for a release another line keeps (PlanRetire.KeptBy), the lines keeping it.
	Lines []string
}

// PlanRetire is a (name, version) the next pass retires: its source (as answers name it), the lines
// and platforms its releases serve, the releases with their bytes, why (each line's kept versions),
// and the version's releases another line keeps.
type PlanRetire struct {
	Source, Name, Version string
	Lines, Platforms      []string
	Releases              []PlanRelease
	Bytes                 int64
	Kept                  map[string][]string
	KeptBy                []PlanRelease
}

// PlanRetired is a retired release: past its grace (purged by the next pass unless Pinned), or in it
// (PurgeAt, zero without a rule in force).
type PlanRetired struct {
	Source, Name, Version string
	Release               PlanRelease
	RetiredAt, PurgeAt    time.Time
	Pinned                bool
}

// RetentionPlan is what the next pass would do in a channel, paged by (name, version).
type RetentionPlan struct {
	InForce        RuleInForce
	NextRun        time.Time
	PurgeOff       bool
	Retire         []PlanRetire
	Purge, Retired []PlanRetired
	Next           string // the cursor of the next page, "" for none
}

// planKey is a (name, version)'s place in the plan's order.
func planKey(name, version string) string { return name + "\x00" + version }

func comparePlanKeys(a, b string) int {
	an, av, _ := strings.Cut(a, "\x00")
	bn, bv, _ := strings.Cut(b, "\x00")
	return cmp.Or(strings.Compare(an, bn), CompareVersions(av, bv))
}

// RetentionPlan answers what the next pass would do in a channel, computed as the pass computes it:
// its administrators and the tenant's auditors. Its bodies' holders are read in the tenant only, and
// named only among the channels the caller administers.
func (s *Service) RetentionPlan(ctx context.Context, a authz.Actor, tenant, channel, after string, limit int) (RetentionPlan, error) {
	ch, err := s.mayReadChannel(ctx, a, tenant, channel)
	if err != nil {
		return RetentionPlan{}, err
	}
	if ch.Kind != store.ChannelSigned {
		return RetentionPlan{}, errNoRetention
	}
	lk, err := s.look(ctx, ch, s.Store.Now())
	if err != nil {
		return RetentionPlan{}, err
	}
	plan := RetentionPlan{InForce: lk.rule, PurgeOff: s.PurgeOff}
	if s.RetentionInterval > 0 && !lk.checked.IsZero() {
		plan.NextRun = lk.checked.Add(s.RetentionInterval)
	}
	// the page's (name, version) keys
	var keys []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, it := range lk.retire {
		add(planKey(it.name, it.version))
	}
	for _, it := range append(slices.Clone(lk.purge), lk.inGrace...) {
		add(planKey(it.release.Name, it.release.PackageVersion))
	}
	slices.SortFunc(keys, comparePlanKeys)
	if after != "" {
		keys = slices.DeleteFunc(keys, func(k string) bool { return comparePlanKeys(k, after) <= 0 })
	}
	if limit > 0 && len(keys) > limit {
		keys, plan.Next = keys[:limit], keys[limit-1]
	}
	page := map[string]bool{}
	for _, k := range keys {
		page[k] = true
	}
	// the bodies of what is on the page
	var hashes []string
	var rels []store.Candidate
	for _, it := range lk.retire {
		if page[planKey(it.name, it.version)] {
			rels = append(rels, it.releases...)
		}
	}
	for _, it := range append(slices.Clone(lk.purge), lk.inGrace...) {
		if page[planKey(it.release.Name, it.release.PackageVersion)] {
			rels = append(rels, it.release)
		}
	}
	for _, c := range rels {
		hashes = append(hashes, c.BodyHash)
	}
	uses, err := s.Store.BodyUses(ctx, ch.TenantID, hashes)
	if err != nil {
		return RetentionPlan{}, err
	}
	labels, err := s.Store.SourceLabels(ctx, ch.TenantID)
	if err != nil {
		return RetentionPlan{}, err
	}
	admins := map[string]bool{}
	mayName := func(channel string) bool {
		ok, seen := admins[channel]
		if !seen {
			ok = s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}) == nil
			admins[channel] = ok
		}
		return ok
	}
	view := func(c store.Candidate) PlanRelease {
		u := uses[c.BodyHash]
		pr := PlanRelease{ID: c.ID, Platform: c.Platform, Slot: c.Slot, BodyHash: c.BodyHash, Bytes: u.Len}
		names := map[string]bool{}
		for _, h := range u.Holders {
			if h.ReleaseID == c.ID {
				continue
			}
			pr.Shared = true
			if mayName(h.Channel) {
				names[h.Channel] = true
			} else {
				pr.Others++
			}
		}
		for n := range names {
			pr.SharedBy = append(pr.SharedBy, n)
		}
		slices.Sort(pr.SharedBy)
		if u.Landed {
			pr.Shared = true
			pr.SharedBy = append(pr.SharedBy, "catalogue")
		}
		return pr
	}
	for _, it := range lk.retire {
		if !page[planKey(it.name, it.version)] {
			continue
		}
		p := PlanRetire{Source: store.Label(labels, it.source), Name: it.name, Version: it.version, Lines: it.lines, Kept: it.kept}
		for _, c := range it.releases {
			pr := view(c)
			p.Releases, p.Bytes = append(p.Releases, pr), p.Bytes+pr.Bytes
			if !slices.Contains(p.Platforms, c.Platform) {
				p.Platforms = append(p.Platforms, c.Platform)
			}
		}
		slices.Sort(p.Platforms)
		ids := make([]string, 0, len(it.keptBy))
		for id := range it.keptBy {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			c, err := s.releaseOf(ctx, ch.ID, id)
			if err != nil {
				return RetentionPlan{}, err
			}
			pr := PlanRelease{ID: c.ID, Platform: c.Platform, Slot: c.Slot, BodyHash: c.BodyHash, Lines: it.keptBy[id]}
			p.KeptBy = append(p.KeptBy, pr)
		}
		plan.Retire = append(plan.Retire, p)
	}
	retired := func(it purgeItem) PlanRetired {
		return PlanRetired{Source: store.Label(labels, it.release.Source), Name: it.release.Name, Version: it.release.PackageVersion,
			Release: view(it.release), RetiredAt: it.release.RetiredAt, PurgeAt: it.purgeAt, Pinned: it.pinned}
	}
	for _, it := range lk.purge {
		if page[planKey(it.release.Name, it.release.PackageVersion)] {
			plan.Purge = append(plan.Purge, retired(it))
		}
	}
	for _, it := range lk.inGrace {
		if page[planKey(it.release.Name, it.release.PackageVersion)] {
			plan.Retired = append(plan.Retired, retired(it))
		}
	}
	return plan, nil
}

// releaseOf reads a release of a channel by id.
func (s *Service) releaseOf(ctx context.Context, channelID, id string) (store.Candidate, error) {
	var c store.Candidate
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		c, err = tx.GetCandidate(ctx, channelID, id)
		return err
	})
	return c, err
}

// --- the pass ---

// Retainer runs the retention pass (spec 0042): every Interval, each signed channel of an active
// tenant whose rule may be in force and whose last look is an Interval old, under a lease per channel
// renewed before each write.
type Retainer struct {
	Service  *Service
	Holder   string // this replica
	Interval time.Duration
	Log      *slog.Logger
}

// retentionLease is a channel's retention lease.
func retentionLease(channelID string) string { return "kista/retention/" + channelID }

// Run loops until ctx is done; an Interval of 0 runs nothing.
func (r *Retainer) Run(ctx context.Context) {
	if r.Interval <= 0 {
		return
	}
	t := time.NewTicker(min(r.Interval, time.Hour))
	defer t.Stop()
	for {
		r.Once(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once looks at the channels whose last look is an Interval old (all: every one).
func (r *Retainer) Once(ctx context.Context, all bool) {
	chs, err := r.Service.Store.RetentionChannels(ctx)
	if err != nil {
		r.Log.Error("release: listing the channels to retain", "error", err)
		return
	}
	now := r.Service.Store.Now()
	for _, c := range chs {
		if !all && !c.CheckedAt.IsZero() && now.Before(c.CheckedAt.Add(r.Interval)) {
			continue
		}
		res, held, err := r.channel(ctx, c.ChannelID, !all)
		switch {
		case err != nil:
			r.Log.Error("release: the retention pass", "tenant", c.Tenant, "channel", c.Channel, "error", err)
		case held && (res.Retired > 0 || res.Purged > 0 || res.Failed > 0):
			r.Log.Info("release: the retention pass", "tenant", c.Tenant, "channel", c.Channel, "retired", res.Retired,
				"purged", res.Purged, "failed", res.Failed, "failure", res.Failure)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Channel runs the pass in one channel under its lease; held is false when another replica holds it.
func (r *Retainer) Channel(ctx context.Context, channelID string) (RetentionResult, bool, error) {
	return r.channel(ctx, channelID, false)
}

// channel is Channel; due skips a channel another replica looked at since the list was read (its
// look is not an Interval old any more once the lease is held).
func (r *Retainer) channel(ctx context.Context, channelID string, due bool) (RetentionResult, bool, error) {
	lease := retentionLease(channelID)
	held, err := r.Service.Store.AcquireLease(ctx, lease, r.Holder, 2*time.Minute)
	if err != nil || !held {
		return RetentionResult{}, false, err
	}
	defer func() { _ = r.Service.Store.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder) }()
	if due {
		_, checked, err := r.Service.Store.ChannelRetention(ctx, channelID)
		if err != nil {
			return RetentionResult{}, true, err
		}
		if !checked.IsZero() && r.Service.Store.Now().Before(checked.Add(r.Interval)) {
			return RetentionResult{}, false, nil // looked at by another replica meanwhile
		}
	}
	renew := func(ctx context.Context) error {
		if ok, err := r.Service.Store.RenewLease(ctx, lease, r.Holder, 2*time.Minute); err != nil || !ok {
			return errRetentionLease
		}
		return nil
	}
	res, err := r.Service.RetainChannel(ctx, channelID, renew)
	return res, true, err
}

var errRetentionLease = errors.New("release: the retention lease was lost")
