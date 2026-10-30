package release

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// RevocationHooks are an upstream's own steps in the transaction applying or lifting one of its
// revocations (spec 0039), under the tenant's upstream lock: Check runs first (false: nothing is
// done), Done last, with how many releases were yanked or restored. Applying and lifting a
// revocation are serialised so, and each is whole or nothing.
type RevocationHooks struct {
	Check func(tx *store.Tx) (bool, error)
	Done  func(tx *store.Tx, n int) error
}

// errChannelsMoved is a channel created between listing the tenant's channels and locking them:
// the transaction is begun again with it.
var errChannelsMoved = errors.New("the tenant's channels changed")

// allChannels runs fn under the tenant's upstream lock and every channel's lock of the tenant (a
// promotion or placement into any of them is ordered against what fn does), in one transaction.
func (s *Service) allChannels(ctx context.Context, tenantID string, fn func(tx *store.Tx, channels []string) error) error {
	for {
		ids, err := s.Store.TenantChannelIDs(ctx, tenantID)
		if err != nil {
			return err
		}
		locks := []string{store.TenantUpstreamLock(tenantID)}
		for _, id := range ids {
			locks = append(locks, lockKey(id))
		}
		err = s.Store.InTenantTx(ctx, tenantID, locks, func(tx *store.Tx) error {
			now, err := tx.TenantChannelIDs(ctx, tenantID)
			if err != nil {
				return err
			}
			if !slices.Equal(now, ids) {
				return errChannelsMoved
			}
			return fn(tx, ids)
		})
		if !errors.Is(err, errChannelsMoved) {
			return err
		}
	}
}

// UpstreamBlock applies an upstream's withdrawal or block of a body (spec 0039, decisions A and D),
// in one transaction under every channel's lock: the block is recorded (from then on the body is
// refused from that upstream, under each channel's lock, as blocks are), each live release of the
// upstream's builds with that body (in its home channel, and the copies promoted or placed
// elsewhere) is yanked and recorded so a lift restores it, and every channel showing a release of
// the body is bumped when the block is new or changed (its -/revocations rows changed). The same
// bytes from any other source stay. It answers how many releases it yanked, and whether Check let
// it apply.
func (s *Service) UpstreamBlock(ctx context.Context, a authz.Actor, tenantID, upstreamID, hash, state, reason string, h RevocationHooks) (int, bool, error) {
	if state != store.RevokeWithdrawn && state != store.RevokeBlocked {
		return 0, false, fmt.Errorf("%w: an upstream blocks for a withdrawal or a block, not %q", store.ErrInvalid, state)
	}
	n, done := 0, false
	err := s.allChannels(ctx, tenantID, func(tx *store.Tx, channels []string) error {
		n, done = 0, false
		if ok, err := h.Check(tx); err != nil || !ok {
			return err
		}
		changed, err := tx.PutUpstreamBlock(ctx, store.UpstreamBlock{UpstreamID: upstreamID, BodyHash: hash, TenantID: tenantID, State: state,
			Reason: reason})
		if err != nil {
			return err
		}
		for _, ch := range channels {
			yanked, err := upstreamYank(ctx, tx, a, ch, tenantID, upstreamID, hash, "", store.YankCauseBlock)
			if err != nil {
				return err
			}
			n += yanked
		}
		if changed {
			if err := tx.BumpUpstreamBodyChannels(ctx, upstreamID, hash); err != nil {
				return err
			}
		}
		done = true
		return h.Done(tx, n)
	})
	return n, done, err
}

// UpstreamYank applies an upstream's yank of a build (spec 0039, decision C): its releases in the
// upstream's home channel are yanked and recorded (a copy promoted or placed elsewhere is that
// channel's own choice), in one transaction under the tenant's upstream lock and the channel's.
func (s *Service) UpstreamYank(ctx context.Context, a authz.Actor, tenantID, upstreamID, channelID, buildID, hash string, h RevocationHooks) (int, bool, error) {
	n, done := 0, false
	err := s.Store.InTenantTx(ctx, tenantID, []string{store.TenantUpstreamLock(tenantID), lockKey(channelID)}, func(tx *store.Tx) error {
		n, done = 0, false
		if ok, err := h.Check(tx); err != nil || !ok {
			return err
		}
		var err error
		if n, err = upstreamYank(ctx, tx, a, channelID, tenantID, upstreamID, hash, buildID, store.YankCauseYank); err != nil {
			return err
		}
		done = true
		return h.Done(tx, n)
	})
	return n, done, err
}

// upstreamYank yanks and records, under the caller's lock of a channel, its live releases of the
// upstream's builds with a body (of one build when buildID is set).
func upstreamYank(ctx context.Context, tx *store.Tx, a authz.Actor, channelID, tenantID, upstreamID, hash, buildID, cause string) (int, error) {
	rels, err := tx.LiveUpstreamReleases(ctx, channelID, upstreamID, hash, buildID)
	if err != nil || len(rels) == 0 {
		return 0, err
	}
	for i := range rels {
		prior := rels[i].State
		if err := tx.SetReleaseState(ctx, &rels[i], store.ReleaseYanked, a.String()); err != nil {
			return 0, err
		}
		if err := tx.RecordUpstreamYank(ctx, store.UpstreamYank{ReleaseID: rels[i].ID, TenantID: tenantID, UpstreamID: upstreamID,
			BuildID: rels[i].BuildID, BodyHash: hash, PriorState: prior, Cause: cause}); err != nil {
			return 0, err
		}
	}
	return len(rels), tx.BumpReleaseVersion(ctx, channelID)
}

// LiftUpstream lifts an upstream's stored revocation of a build (spec 0039, decisions A and C), in
// one transaction under every channel's lock: its block of the build's body, if any, is removed (the
// channels showing the body bumped), and the releases recorded for the build are restored to their
// state before, each only while it is still yanked, its build not withdrawn (spec 0033), and its body
// not blocked by an administrator; the records go either way. It answers how many releases it
// restored, and whether Check let it lift.
func (s *Service) LiftUpstream(ctx context.Context, a authz.Actor, tenantID, upstreamID, buildID string, h RevocationHooks) (int, bool, error) {
	n, done := 0, false
	err := s.allChannels(ctx, tenantID, func(tx *store.Tx, _ []string) error {
		n, done = 0, false
		if ok, err := h.Check(tx); err != nil || !ok {
			return err
		}
		rev, err := tx.GetUpstreamRevocation(ctx, upstreamID, buildID)
		if err != nil {
			return err
		}
		hash := rev.BodyHash
		removed, err := tx.DeleteUpstreamBlock(ctx, upstreamID, hash)
		if err != nil {
			return err
		}
		if removed {
			if err := tx.BumpUpstreamBodyChannels(ctx, upstreamID, hash); err != nil {
				return err
			}
		}
		recs, err := tx.UpstreamYanksOf(ctx, upstreamID, buildID)
		if err != nil {
			return err
		}
		bumped := map[string]bool{}
		for _, y := range recs {
			if y.ChannelID != "" {
				rel, err := tx.ReleaseByID(ctx, y.ReleaseID)
				if err != nil {
					return err
				}
				blocked, err := tx.Blocked(ctx, tenantID, y.BodyHash)
				if err != nil {
					return err
				}
				if rel.State == store.ReleaseYanked && !rel.Withdrawn && !blocked {
					if err := tx.SetReleaseState(ctx, &rel.Release, y.PriorState, a.String()); err != nil {
						return err
					}
					n++
					if !bumped[y.ChannelID] {
						bumped[y.ChannelID] = true
						if err := tx.BumpReleaseVersion(ctx, y.ChannelID); err != nil {
							return err
						}
					}
				}
			}
			if err := tx.DeleteUpstreamYank(ctx, y.ReleaseID); err != nil {
				return err
			}
		}
		done = true
		return h.Done(tx, n)
	})
	return n, done, err
}
