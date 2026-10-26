package release

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a channel following a whole source.

// FollowView is a follow as answers show it: its source, who set it and when, and the names it
// skips, each with the reason.
type FollowView struct {
	Source string
	SetBy  string
	SetAt  time.Time
	Skips  map[string]string
}

// Follows lists a channel's follows (its administrators').
func (s *Service) Follows(ctx context.Context, a authz.Actor, tenant, channel string) ([]FollowView, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return nil, err
	}
	fs, err := s.Store.Follows(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	labels, err := s.Store.SourceLabels(ctx, ch.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]FollowView, 0, len(fs))
	for _, f := range fs {
		skips, err := s.Store.FollowSkips(ctx, ch.ID, f.Source)
		if err != nil {
			return nil, err
		}
		out = append(out, FollowView{Source: store.Label(labels, f.Source), SetBy: f.SetBy, SetAt: f.SetAt, Skips: skips})
	}
	return out, nil
}

// Follow is one follow's view; a source the channel does not follow is not found.
func (s *Service) Follow(ctx context.Context, a authz.Actor, tenant, channel, label string) (FollowView, error) {
	vs, err := s.Follows(ctx, a, tenant, channel)
	if err != nil {
		return FollowView{}, err
	}
	for _, v := range vs {
		if v.Source == label {
			return v, nil
		}
	}
	return FollowView{}, fmt.Errorf("%w: the channel does not follow %s", store.ErrNotFound, label)
}

// SetFollow makes a signed channel follow a source of the tenant (an upstream or a publisher; never
// the tenant's own, which is everyone who may publish in it). The placement pass takes its names.
func (s *Service) SetFollow(ctx context.Context, a authz.Actor, tenant, channel, label string) (FollowView, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return FollowView{}, err
	}
	if ch.Kind != store.ChannelSigned {
		return FollowView{}, fmt.Errorf("%w: a %s channel follows nothing: its mirrors fill it", store.ErrInvalid, ch.Kind)
	}
	if label == store.SourceTenant {
		return FollowView{}, fmt.Errorf("%w: the tenant's own source is everyone who may publish in it: it is not followed", store.ErrInvalid)
	}
	src, err := s.Store.ResolveSource(ctx, ch.TenantID, label)
	if errors.Is(err, store.ErrNoSource) {
		return FollowView{}, fmt.Errorf("%w: %q is not a source of the tenant (upstream:<an upstream>, publisher:<a publisher>)", store.ErrInvalid, label)
	}
	if err != nil {
		return FollowView{}, err
	}
	if err := s.sourceOfType(ctx, ch, src, label); err != nil {
		return FollowView{}, err
	}
	if err := s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		if _, err := tx.GetChannel(ctx, tenant, channel); err != nil { // a deleting tenant's refuses (spec 0019)
			return err
		}
		created, err := tx.SetFollow(ctx, ch.ID, src, a.String())
		if err != nil || !created {
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "follow.set", "channel:"+ch.Name, map[string]any{"source": label})
	}); err != nil {
		return FollowView{}, err
	}
	return s.Follow(ctx, a, tenant, channel, label)
}

// RemoveFollow stops a channel following a source: nothing more is placed by it; what it placed
// stays, and the rules it wrote complete the versions they hold (mode follow acts as manual).
func (s *Service) RemoveFollow(ctx context.Context, a authz.Actor, tenant, channel, label string) error {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return err
	}
	src, err := s.Store.ResolveSource(ctx, ch.TenantID, label)
	if errors.Is(err, store.ErrNoSource) {
		return fmt.Errorf("%w: the channel does not follow %s", store.ErrNotFound, label)
	}
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		if err := tx.RemoveFollow(ctx, ch.ID, src); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: the channel does not follow %s", store.ErrNotFound, label)
			}
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "follow.clear", "channel:"+ch.Name, map[string]any{"source": label})
	})
}

// followLook is what a follow's look reads once: the tenant's catalogue names by source, its rules
// by name (with their channels), the names the channel's other follows offer, and how to ask a
// publisher's grants.
type followLook struct {
	tenant  store.Tenant
	channel store.Channel
	label   string
	kind    string              // the followed upstream's kind ("" for a publisher)
	byName  map[string][]string // name → the live sources with builds of it
	rules   []store.Rule
	offered map[string]bool
	consent *authz.Actor // a followed publisher, whose grants a name must reach (spec 0033)
	// distributor is a followed distributor's publisher's distributor (spec 0033 phase B): it has no
	// grants, so a name it may publish into the catalogue (not reserved server-wide, nor in the tenant
	// for another holder) is consented to instead
	distributor string
}

// PlaceFollow takes, for a follow, every name of its source the channel has no rule for (spec 0033):
// it writes the name's rule (mode follow) and places as an auto rule would; a rule an arrival wrote
// for the same source becomes the follow's. It skips a name the tenant uses from another source, one
// reserved (server-wide, unless the source is DuckDB's own repository of that kind; or in the
// tenant, unless for a followed distributor's publisher's distributor), one another follow of the
// channel offers too, and, for a publisher, one it may not publish in the channel (a follow is consent
// to a source, not to every grant anyone gives it; a distributor's publisher has no grants, and the
// names it may publish into the catalogue are its consent, spec 0033 phase B). It returns how
// many builds it placed, the names it skipped with the reason, and the first failure's text; a
// failure on one name does not stop the others.
func (s *Service) PlaceFollow(ctx context.Context, f store.DueFollow, renew func(context.Context) error) (int, map[string]string, string, error) {
	ctx = store.WithLicenseWarnings(ctx) // spec 0035 phase 2: the look's licence warnings, once per source, name and channel
	lk, ok, err := s.followLook(ctx, f)
	if err != nil || !ok {
		return 0, nil, "", err
	}
	names := []string{}
	for n, srcs := range lk.byName {
		if slices.Contains(srcs, f.Source) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	sel, err := s.Store.ChannelSources(ctx, f.ChannelID)
	if err != nil {
		return 0, nil, "", err
	}
	skips := map[string]string{}
	placed, failure := 0, ""
	fail := func(name string, err error) {
		if failure == "" {
			failure = name + ": " + err.Error()
		}
	}
	for i, name := range names {
		if renew != nil && i%50 == 0 {
			if err := renew(ctx); err != nil {
				return placed, skips, failure, err
			}
		}
		r, ruled := sel[name]
		switch {
		case ruled && r.Source != f.Source:
			skips[name] = "the channel serves it from another source"
			continue
		case ruled && r.Mode != store.RuleNone:
			continue // its rule places it (a follow's, or one an administrator set)
		}
		if lk.offered[name] {
			skips[name] = "contested: another follow of the channel offers it too"
			continue
		}
		reason, err := s.followLimit(ctx, lk, f, name)
		if err != nil {
			fail(name, err)
			continue
		}
		if reason != "" {
			skips[name] = reason
			continue
		}
		start := s.Store.Now()
		wrote, err := s.takeName(ctx, lk, f, name)
		if err != nil {
			fail(name, err)
			continue
		}
		if !wrote {
			continue
		}
		rule := store.DueRule{TenantID: f.TenantID, ChannelID: f.ChannelID, Name: name, Source: f.Source, Mode: store.RuleFollow,
			PackageType: lk.channel.PackageType}
		n, why, due, err := s.placeRule(ctx, rule, renew)
		if errors.Is(err, errLeaseLost) {
			return placed, skips, failure, err
		}
		if err != nil {
			fail(name, err)
			continue
		}
		placed += n
		if why != "" {
			fail(name, errors.New(why))
		}
		if err := s.Store.SetRuleChecked(ctx, rule, start, start.Add(-placeSkew), why, due); err != nil {
			fail(name, err)
		}
	}
	return placed, skips, failure, nil
}

// followLook reads what a follow's look needs; ok is false when the source is gone or the channel
// does not follow it any more.
func (s *Service) followLook(ctx context.Context, f store.DueFollow) (followLook, bool, error) {
	var lk followLook
	if following, err := s.Store.Following(ctx, f.ChannelID, f.Source); err != nil || !following {
		return lk, false, err
	}
	labels, err := s.Store.SourceLabels(ctx, f.TenantID)
	if err != nil {
		return lk, false, err
	}
	if _, ok := labels[f.Source]; !ok {
		return lk, false, nil // a removed source's (its follows go with it)
	}
	lk.label = labels[f.Source]
	if lk.tenant, err = s.Store.GetTenantAnyByID(ctx, f.TenantID); err != nil {
		return lk, false, err
	}
	if lk.channel, err = s.channelByID(ctx, f.ChannelID); err != nil {
		return lk, false, err
	}
	if !pkgtype.Known(lk.channel.PackageType) {
		return lk, false, nil // spec 0034: a channel of a type this binary does not know places nothing
	}
	if lk.kind, err = s.Store.UpstreamKindOf(ctx, f.Source); err != nil {
		return lk, false, err
	}
	// spec 0034: a follow takes only its source's names of the channel's type
	vers, err := s.Store.SourceVersions(ctx, f.TenantID, lk.channel.PackageType)
	if err != nil {
		return lk, false, err
	}
	lk.byName = map[string][]string{}
	for k := range vers {
		if _, live := labels[k[0]]; live { // a removed source's builds are no use of the name
			lk.byName[k[1]] = append(lk.byName[k[1]], k[0])
		}
	}
	if lk.rules, err = s.Store.TenantRules(ctx, f.TenantID, lk.channel.PackageType); err != nil {
		return lk, false, err
	}
	follows, err := s.Store.Follows(ctx, f.ChannelID)
	if err != nil {
		return lk, false, err
	}
	lk.offered = map[string]bool{}
	for _, o := range follows {
		if o.Source == f.Source {
			continue
		}
		for n, srcs := range lk.byName {
			if slices.Contains(srcs, o.Source) {
				lk.offered[n] = true
			}
		}
	}
	if lk.distributor, err = s.Store.SourceDistributor(ctx, f.TenantID, f.Source); err != nil {
		return lk, false, err
	}
	if id, ok := strings.CutPrefix(f.Source, store.SourcePublisherPrefix); ok && lk.distributor == "" {
		a := authz.Actor{Kind: authz.ActorPublisher, ID: lk.tenant.Name + "/" + strings.TrimPrefix(lk.label, store.SourcePublisherPrefix),
			Tenant: lk.tenant.Name, Principals: auth.Principals{auth.PublisherKey(id): true}}
		lk.consent = &a
	}
	return lk, true, nil
}

// followLimit says why a follow does not take a name ("" when it does).
func (s *Service) followLimit(ctx context.Context, lk followLook, f store.DueFollow, name string) (string, error) {
	reserved, err := s.Store.NameReservedFor(ctx, f.TenantID, lk.channel.PackageType, name)
	if err != nil {
		return "", err
	}
	switch {
	case reserved == store.ReservedCore && lk.kind != store.UpstreamCore, reserved == store.ReservedCommunity && lk.kind != store.UpstreamCommunity:
		return "a name DuckDB's " + reserved + " repository has: a rule naming it takes it", nil
	case reserved == "tenant":
		// spec 0033 phase B: a distributor's publisher takes a name reserved for its distributor
		holder, ok, err := s.Store.TenantReservedHolder(ctx, f.TenantID, lk.channel.PackageType, name)
		if err != nil {
			return "", err
		}
		if lk.distributor == "" || !ok || holder != lk.distributor {
			return "a name reserved in the tenant: a rule naming it takes it", nil
		}
	}
	for _, src := range lk.byName[name] {
		if src != f.Source {
			return "the tenant uses it from another source", nil
		}
	}
	for _, r := range lk.rules {
		if r.Name == name && r.Source != f.Source {
			return "the tenant uses it from another source", nil
		}
	}
	if lk.consent != nil {
		err := s.Authz.Allow(ctx, *lk.consent, authz.VerbPublish, authz.Resource{Tenant: lk.tenant.Name, Channel: lk.channel.Name, Package: name})
		switch {
		case errors.Is(err, authz.ErrDenied):
			return "the publisher may not publish it in the channel: a rule naming it takes it", nil
		case err != nil:
			return "", err
		}
	}
	return "", nil
}

// takeName writes a name's follow rule under the channel lock, checking again that the channel
// still follows the source and the name is still free (no rule, or one an arrival wrote for the
// same source, which becomes the follow's) and not reserved in the tenant meanwhile.
func (s *Service) takeName(ctx context.Context, lk followLook, f store.DueFollow, name string) (bool, error) {
	wrote := false
	err := s.Store.InTx(ctx, lockKey(f.ChannelID), func(tx *store.Tx) error {
		if following, err := tx.FollowingTx(ctx, f.ChannelID, f.Source); err != nil || !following {
			return err
		}
		cur, had, err := tx.ChannelSourceOf(ctx, f.ChannelID, name)
		if err != nil || had && (cur.Source != f.Source || cur.Mode != store.RuleNone) {
			return err
		}
		// reserved in the tenant meanwhile, unless for the followed distributor's publisher's distributor
		holder, held, err := tx.TenantReservedHolder(ctx, f.TenantID, lk.channel.PackageType, name)
		if err != nil || held && (lk.distributor == "" || holder != lk.distributor) {
			return err
		}
		if _, err := tx.SetChannelRule(ctx, f.ChannelID, name, f.Source, store.RuleFollow, placer.String()); err != nil {
			return err
		}
		wrote = true
		return tx.Event(ctx, f.TenantID, placer.String(), "source.set", "channel:"+lk.channel.Name+"/ext:"+name,
			map[string]any{"name": name, "source": lk.label, "before": "", "arrival": !had, "mode": store.RuleFollow})
	})
	return wrote, err
}
