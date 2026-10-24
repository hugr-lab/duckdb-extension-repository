package release

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
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

// PlaceFollow takes, for a follow, every name of its source the channel has no rule for (spec 0033):
// it writes the name's rule (mode follow) and places as an auto rule would; it skips a name the
// tenant uses from another source, one reserved (server-wide, unless the source is DuckDB's own
// repository of that kind; or in the tenant), and one another follow of the channel offers too. It
// returns how many builds it placed, the names it skipped with the reason, and the first failure's
// text.
func (s *Service) PlaceFollow(ctx context.Context, f store.DueFollow, renew func(context.Context) error) (int, map[string]string, string, error) {
	names, err := s.Store.SourceNames(ctx, f.TenantID, f.Source)
	if err != nil || len(names) == 0 {
		return 0, nil, "", err
	}
	rules, err := s.Store.ChannelSources(ctx, f.ChannelID)
	if err != nil {
		return 0, nil, "", err
	}
	follows, err := s.Store.Follows(ctx, f.ChannelID)
	if err != nil {
		return 0, nil, "", err
	}
	offered := map[string]bool{} // what the channel's other follows offer
	for _, o := range follows {
		if o.Source == f.Source {
			continue
		}
		ns, err := s.Store.SourceNames(ctx, f.TenantID, o.Source)
		if err != nil {
			return 0, nil, "", err
		}
		for _, n := range ns {
			offered[n] = true
		}
	}
	kind, err := s.Store.UpstreamKindOf(ctx, f.Source)
	if err != nil {
		return 0, nil, "", err
	}
	label, err := s.Store.SourceLabel(ctx, f.Source)
	if err != nil {
		return 0, nil, "", err
	}
	ch, err := s.channelByID(ctx, f.ChannelID)
	if err != nil {
		return 0, nil, "", err
	}
	skips := map[string]string{}
	placed, failure := 0, ""
	for _, name := range names {
		if r, ok := rules[name]; ok {
			if r.Source != f.Source {
				skips[name] = "the channel serves it from another source"
			}
			continue // the name's rule places it (a follow's rule, mode follow)
		}
		if offered[name] {
			skips[name] = "contested: another follow of the channel offers it too"
			continue
		}
		reason, err := s.followLimit(ctx, f, name, kind)
		if err != nil {
			return placed, skips, failure, err
		}
		if reason != "" {
			skips[name] = reason
			continue
		}
		wrote := false
		if err := s.Store.InTx(ctx, lockKey(f.ChannelID), func(tx *store.Tx) error {
			if _, had, err := tx.ChannelSourceOf(ctx, f.ChannelID, name); err != nil || had {
				return err
			}
			if _, err := tx.SetChannelRule(ctx, f.ChannelID, name, f.Source, store.RuleFollow, placer.String()); err != nil {
				return err
			}
			wrote = true
			return tx.Event(ctx, f.TenantID, placer.String(), "source.set", "channel:"+ch.Name+"/ext:"+name,
				map[string]any{"name": name, "source": label, "before": "", "arrival": true, "mode": store.RuleFollow})
		}); err != nil {
			return placed, skips, failure, err
		}
		if !wrote {
			continue
		}
		n, fail, err := s.PlaceRule(ctx, store.DueRule{TenantID: f.TenantID, ChannelID: f.ChannelID, Name: name, Source: f.Source,
			Mode: store.RuleFollow}, renew)
		if err != nil {
			return placed, skips, failure, err
		}
		placed += n
		if failure == "" && fail != "" {
			failure = name + ": " + fail
		}
	}
	return placed, skips, failure, nil
}

// followLimit says why a follow does not take a name ("" when it does).
func (s *Service) followLimit(ctx context.Context, f store.DueFollow, name, kind string) (string, error) {
	reserved, err := s.Store.NameReservedFor(ctx, f.TenantID, name)
	if err != nil {
		return "", err
	}
	switch {
	case reserved == store.ReservedCore && kind != store.UpstreamCore, reserved == store.ReservedCommunity && kind != store.UpstreamCommunity:
		return "a name DuckDB's " + reserved + " repository has: a rule naming it takes it", nil
	case reserved == "tenant":
		return "a name reserved in the tenant: a rule naming it takes it", nil
	}
	return s.Store.NameUsedElsewhere(ctx, f.TenantID, name, f.Source)
}
