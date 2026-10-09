package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// PromoteOptions name what to promote (spec 0008): one release of the source channel, or every
// active release of an extension version there.
type PromoteOptions struct {
	From       string // the source channel
	Release    string // a release id, or
	Version    string // an extension version
	Private    bool   // narrow the visibility (a promotion never widens it)
	NotCurrent bool
	// Provenance is a JSON object of the promoter's facts (a publisher's run), merged into each
	// release's provenance.
	Provenance string
}

// Promote releases Builds of name that are in channel o.From in channel, with channel's signature,
// all or none. The caller needs promote on (channel, name) and publish on (o.From, name); a source
// it may not publish from answers as missing. It returns the releases and whether all existed.
func (s *Service) Promote(ctx context.Context, a authz.Actor, tenant, channel, name string, o PromoteOptions) ([]store.Release, bool, error) {
	res := func(ch string) authz.Resource {
		return authz.Resource{Tenant: tenant, Channel: ch, Extension: name, Reserved: reserved.Kind(name) != ""}
	}
	if err := s.Authz.Allow(ctx, a, authz.VerbPromote, res(channel)); err != nil {
		return nil, false, err
	}
	if o.From == "" {
		return nil, false, fmt.Errorf("%w: from_channel is required", store.ErrInvalid)
	}
	if (o.Release == "") == (o.Version == "") {
		return nil, false, fmt.Errorf("%w: name either a release or a version", store.ErrInvalid)
	}
	if o.From == channel {
		return nil, false, fmt.Errorf("%w: a release is promoted to another channel", store.ErrInvalid)
	}
	if err := s.Authz.Allow(ctx, a, authz.VerbPublish, res(o.From)); err != nil {
		if errors.Is(err, authz.ErrDenied) {
			return nil, false, fmt.Errorf("%w: release", store.ErrNotFound) // as a missing source
		}
		return nil, false, err
	}
	target, err := s.Store.GetServeChannel(ctx, tenant, channel)
	if err != nil {
		return nil, false, err
	}
	source, err := s.Store.GetServeChannel(ctx, tenant, o.From)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, fmt.Errorf("%w: release", store.ErrNotFound)
	}
	if err != nil {
		return nil, false, err
	}
	if source.Channel.Kind != store.ChannelSigned {
		return nil, false, fmt.Errorf("%w: release", store.ErrNotFound) // a source's kind is not told
	}
	if target.Channel.Kind != store.ChannelSigned {
		return nil, false, fmt.Errorf("%w: promotion is into a signed channel", ErrState)
	}
	if _, err := activeKey(ctx, s.Store, target.Channel.ID); err != nil {
		return nil, false, err
	}
	rels, err := s.Store.ListReleases(ctx, source.Channel.ID, name)
	if err != nil {
		return nil, false, err
	}
	var picked []store.Candidate
	for _, r := range rels {
		if r.ID == o.Release || o.Version != "" && r.ExtVersion == o.Version {
			picked = append(picked, r)
		}
	}
	if len(picked) == 0 {
		return nil, false, fmt.Errorf("%w: release", store.ErrNotFound)
	}
	var items []item
	for _, r := range picked {
		if r.State != store.ReleaseActive {
			if o.Version != "" {
				continue // a version's deprecated and yanked releases stay behind
			}
			return nil, false, fmt.Errorf("%w: only an active release is promoted; this one is %s", ErrState, r.State)
		}
		var b store.Build
		if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			b, err = tx.GetBuild(ctx, r.BuildID)
			return err
		}); err != nil {
			return nil, false, err
		}
		if !b.Checked {
			return nil, false, fmt.Errorf("%w: %s %s %s was added unchecked; publish it to check it", ErrState, r.Name, r.ExtVersion, r.Platform)
		}
		vis := r.Visibility
		if o.Private {
			vis = store.Private
		}
		pm := map[string]any{}
		if o.Provenance != "" {
			_ = json.Unmarshal([]byte(o.Provenance), &pm)
		}
		pm["actor"], pm["from_channel"], pm["from_release"] = a.String(), o.From, r.ID
		prov, err := json.Marshal(pm)
		if err != nil || len(prov) > 4000 {
			prov, _ = json.Marshal(map[string]string{"actor": a.String(), "from_channel": o.From, "from_release": r.ID})
		}
		it := item{b: b, visibility: vis, notCurrent: o.NotCurrent, origin: store.OriginPromotion, provenance: string(prov)}
		if t := res(channel); !t.Reserved {
			it.recheck = s.reservedCheck(a, authz.VerbPromote, t)
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, false, fmt.Errorf("%w: version %s has no active release in %s", ErrState, o.Version, o.From)
	}
	return s.insert(ctx, a, target.Channel.ID, items)
}

var bodyHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Block bans a body hash in a tenant (spec 0008): the block is committed first, then every signed
// channel of the tenant is swept under its lock and the body's releases are yanked. Every release
// insert checks the block under the same lock, so none slips past. Blocking a banned hash sweeps
// again (a sweep that failed half-way is finished) and reports that it existed.
func (s *Service) Block(ctx context.Context, a authz.Actor, tenant, hash, reason string) (store.Block, bool, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Block{}, false, err
	}
	if !bodyHashRe.MatchString(hash) {
		return store.Block{}, false, fmt.Errorf("%w: a body hash is 64 lowercase hex digits", store.ErrInvalid)
	}
	if reason == "" {
		return store.Block{}, false, fmt.Errorf("%w: a block needs a reason", store.ErrInvalid)
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Block{}, false, err
	}
	b := store.Block{TenantID: t.ID, BodyHash: hash, Reason: reason, CreatedBy: a.String()}
	existed := false
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertBlock(ctx, &b) })
	if errors.Is(err, store.ErrExists) {
		existed = true
		if b, err = s.GetBlock(ctx, a, tenant, hash); err != nil {
			return store.Block{}, false, err
		}
	} else if err != nil {
		return store.Block{}, false, err
	}
	chs, err := s.Store.ListChannels(ctx, tenant)
	if err != nil {
		return b, existed, err
	}
	for _, ch := range chs { // signed and passthrough channels (spec 0009)
		err := s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
			rels, err := tx.LiveReleasesWithBody(ctx, ch.ID, hash)
			if err != nil {
				return err
			}
			for i := range rels {
				if err := tx.SetReleaseState(ctx, &rels[i], store.ReleaseYanked, a.String()); err != nil {
					return err
				}
			}
			return tx.BumpReleaseVersion(ctx, ch.ID)
		})
		if err != nil {
			return b, existed, fmt.Errorf("release: the block is recorded; yanking in channel %s failed (block it again to finish): %w", ch.Name, err)
		}
	}
	return b, existed, nil
}

// Unblock lifts a ban: the body may be released again; yanked releases stay yanked.
func (s *Service) Unblock(ctx context.Context, a authz.Actor, tenant, hash string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeleteBlock(ctx, t.ID, hash) }); err != nil {
		return err
	}
	// the index marks yanked rows of blocked bodies: every channel's snapshot is renewed
	chs, err := s.Store.ListChannels(ctx, tenant)
	if err != nil {
		return err
	}
	for _, ch := range chs {
		if err := s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error { return tx.BumpReleaseVersion(ctx, ch.ID) }); err != nil {
			return err
		}
	}
	return nil
}

// ListBlocks lists a tenant's blocks.
func (s *Service) ListBlocks(ctx context.Context, a authz.Actor, tenant string) ([]store.Block, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return nil, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return s.Store.ListBlocks(ctx, t.ID)
}

// GetBlock reads one block.
func (s *Service) GetBlock(ctx context.Context, a authz.Actor, tenant, hash string) (store.Block, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Block{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Block{}, err
	}
	return s.Store.GetBlock(ctx, t.ID, hash)
}
