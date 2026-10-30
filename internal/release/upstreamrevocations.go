package release

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// UpstreamBlock applies an upstream's withdrawal or block of a body (spec 0039, decisions A and D):
// the block is recorded first (from then on the body is refused from that upstream under every
// channel's lock, as blocks are), then each channel holding a live release of the upstream's builds
// with that body is swept under its lock: those releases (the home channel's and their copies
// promoted or placed elsewhere) are yanked and recorded, so a lift restores them. The same bytes
// from any other source stay. It answers how many releases it yanked; applying it again sweeps
// again (a sweep that failed half-way is finished).
func (s *Service) UpstreamBlock(ctx context.Context, a authz.Actor, tenantID, upstreamID, hash, state, reason string) (int, error) {
	if state != store.RevokeWithdrawn && state != store.RevokeBlocked {
		return 0, fmt.Errorf("%w: an upstream blocks for a withdrawal or a block, not %q", store.ErrInvalid, state)
	}
	if err := s.Store.InTenantTx(ctx, tenantID, nil, func(tx *store.Tx) error {
		return tx.PutUpstreamBlock(ctx, store.UpstreamBlock{UpstreamID: upstreamID, BodyHash: hash, TenantID: tenantID, State: state,
			Reason: reason})
	}); err != nil {
		return 0, err
	}
	chs, err := s.Store.RevocationChannels(ctx, tenantID, upstreamID, hash)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ch := range chs {
		yanked, err := s.upstreamYank(ctx, a, ch.ID, tenantID, upstreamID, hash, "", store.YankCauseBlock)
		if err != nil {
			return n, err
		}
		n += yanked
	}
	return n, nil
}

// UpstreamYank applies an upstream's yank of a build (spec 0039, decision C): its releases in the
// upstream's home channel are yanked and recorded (a copy promoted or placed elsewhere is that
// channel's own choice). It answers how many releases it yanked.
func (s *Service) UpstreamYank(ctx context.Context, a authz.Actor, tenantID, upstreamID, channelID, buildID, hash string) (int, error) {
	return s.upstreamYank(ctx, a, channelID, tenantID, upstreamID, hash, buildID, store.YankCauseYank)
}

// upstreamYank yanks and records, under a channel's lock, its live releases of the upstream's builds
// with a body (of one build when buildID is set).
func (s *Service) upstreamYank(ctx context.Context, a authz.Actor, channelID, tenantID, upstreamID, hash, buildID, cause string) (int, error) {
	n := 0
	err := s.Store.InTenantTx(ctx, tenantID, []string{lockKey(channelID)}, func(tx *store.Tx) error {
		n = 0
		rels, err := tx.LiveUpstreamReleases(ctx, channelID, upstreamID, hash, buildID)
		if err != nil {
			return err
		}
		for i := range rels {
			prior := rels[i].State
			if err := tx.SetReleaseState(ctx, &rels[i], store.ReleaseYanked, a.String()); err != nil {
				return err
			}
			if err := tx.RecordUpstreamYank(ctx, store.UpstreamYank{ReleaseID: rels[i].ID, TenantID: tenantID, UpstreamID: upstreamID,
				BuildID: rels[i].BuildID, BodyHash: hash, PriorState: prior, Cause: cause}); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return nil
		}
		return tx.BumpReleaseVersion(ctx, channelID)
	})
	return n, err
}

// LiftUpstream lifts an upstream's revocation of a build (spec 0039, decisions A and C): its block of
// the build's body, if any, is removed, and the releases recorded for the build are restored under
// each channel's lock to their state before, each only while it is still yanked, its build not
// withdrawn (spec 0033), its body blocked neither by an administrator nor by the upstream; the
// records go either way. It answers how many releases it restored.
func (s *Service) LiftUpstream(ctx context.Context, a authz.Actor, tenantID, upstreamID, buildID, hash string) (int, error) {
	if err := s.Store.InTenantTx(ctx, tenantID, nil, func(tx *store.Tx) error {
		_, err := tx.DeleteUpstreamBlock(ctx, upstreamID, hash)
		return err
	}); err != nil {
		return 0, err
	}
	recs, err := s.Store.UpstreamYanksOf(ctx, upstreamID, buildID)
	if err != nil {
		return 0, err
	}
	byChannel := map[string][]store.UpstreamYank{}
	var gone []store.UpstreamYank
	for _, y := range recs {
		if y.ChannelID == "" {
			gone = append(gone, y)
			continue
		}
		byChannel[y.ChannelID] = append(byChannel[y.ChannelID], y)
	}
	if len(gone) > 0 {
		if err := s.Store.InTenantTx(ctx, tenantID, nil, func(tx *store.Tx) error {
			for _, y := range gone {
				if err := tx.DeleteUpstreamYank(ctx, y.ReleaseID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
	}
	n := 0
	for channelID, ys := range byChannel {
		restored := 0
		err := s.Store.InTenantTx(ctx, tenantID, []string{lockKey(channelID)}, func(tx *store.Tx) error {
			restored = 0
			for _, y := range ys {
				rel, err := tx.ReleaseByID(ctx, y.ReleaseID)
				if err == nil && rel.State == store.ReleaseYanked && !rel.Withdrawn {
					blocked, err := tx.Blocked(ctx, tenantID, y.BodyHash)
					if err != nil {
						return err
					}
					still, err := tx.UpstreamBlocked(ctx, upstreamID, y.BodyHash)
					if err != nil {
						return err
					}
					if !blocked && !still {
						if err := tx.SetReleaseState(ctx, &rel.Release, y.PriorState, a.String()); err != nil {
							return err
						}
						restored++
					}
				} else if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				if err := tx.DeleteUpstreamYank(ctx, y.ReleaseID); err != nil {
					return err
				}
			}
			if restored == 0 {
				return nil
			}
			return tx.BumpReleaseVersion(ctx, channelID)
		})
		if err != nil {
			return n, err
		}
		n += restored
	}
	return n, nil
}
