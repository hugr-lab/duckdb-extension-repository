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
// its count.
type SourceView struct {
	Name      string
	Source    string
	SetBy     string
	SetAt     time.Time
	Available map[string]int
}

// mayAdminChannel allows a channel's administrators (admin on the channel; tenant administrators).
func (s *Service) mayAdminChannel(ctx context.Context, a authz.Actor, tenant, channel string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel}); err != nil {
		return store.Channel{}, err
	}
	return s.Store.GetChannel(ctx, tenant, channel)
}

// Sources lists a channel's names with their sources: every name with a selected source or a
// release, by name.
func (s *Service) Sources(ctx context.Context, a authz.Actor, tenant, channel string) ([]SourceView, error) {
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
	names := map[string]bool{}
	for n := range sel {
		names[n] = true
	}
	for n := range avail {
		names[n] = true
	}
	out := make([]SourceView, 0, len(names))
	for n := range names {
		v := SourceView{Name: n, Available: map[string]int{}}
		if x, ok := sel[n]; ok {
			v.Source, v.SetBy, v.SetAt = store.Label(labels, x.Source), x.SetBy, x.SetAt
		}
		for src, k := range avail[n] {
			v.Available[store.Label(labels, src)] += k
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y SourceView) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// Source is one name's view (Sources); a name with neither a source nor a release is not found.
func (s *Service) Source(ctx context.Context, a authz.Actor, tenant, channel, name string) (SourceView, error) {
	vs, err := s.Sources(ctx, a, tenant, channel)
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
// or not: its stored releases are served at once, the others' stay stored and unserved.
func (s *Service) SetSource(ctx context.Context, a authz.Actor, tenant, channel, name, label string) (SourceView, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return SourceView{}, err
	}
	if err := ValidName(name); err != nil {
		return SourceView{}, err
	}
	src, err := s.Store.ResolveSource(ctx, ch.TenantID, label)
	if errors.Is(err, store.ErrNoSource) {
		return SourceView{}, fmt.Errorf("%w: %q is not a source of the tenant (tenant, upstream:<an upstream>, publisher:<a publisher>)",
			store.ErrInvalid, label)
	}
	if err != nil {
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
		before, err := tx.SetChannelSource(ctx, ch.ID, name, src, a.String())
		if err != nil || before == src {
			return err
		}
		bl, err := tx.SourceLabel(ctx, before)
		if err != nil {
			return err
		}
		if before == "" {
			bl = ""
		}
		if err := tx.Event(ctx, ch.TenantID, a.String(), "source.set", "channel:"+ch.Name+"/ext:"+name,
			map[string]any{"name": name, "source": label, "before": bl, "arrival": false}); err != nil {
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
