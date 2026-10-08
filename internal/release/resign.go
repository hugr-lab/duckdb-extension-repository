package release

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// resignBatch is how many releases are signed between two transactions.
const resignBatch = 32

// Resign gives every non-yanked release of a channel a signature by its active key, then moves the
// serving key to the active key (spec 0006). It is idempotent; signing happens outside
// transactions, and each batch is inserted under the channel lock after re-reading the keys. It
// returns how many signatures were added and whether the serving key moved. renew, if set, is
// called before each batch; an error from it stops the work (the lease was lost).
func (s *Service) Resign(ctx context.Context, channelID string, renew func(context.Context) error) (int, bool, error) {
	signed := 0
	for {
		if renew != nil {
			if err := renew(ctx); err != nil {
				return signed, false, err
			}
		}
		active, err := activeKey(ctx, s.Store, channelID)
		if err != nil {
			return signed, false, err
		}
		rels, hashes, err := s.Store.UnsignedReleases(ctx, channelID, active.ID, resignBatch)
		if err != nil {
			return signed, false, err
		}
		if len(rels) == 0 {
			moved, err := s.moveServing(ctx, channelID, active.ID)
			if errors.Is(err, errResign) {
				continue // a release arrived or the key changed; look again
			}
			return signed, moved, err
		}
		sigs := make([][]byte, len(rels))
		for i, h := range hashes {
			bh, err := parseHash(h)
			if err != nil {
				return signed, false, err
			}
			if sigs[i], err = s.sign(ctx, active, bh); err != nil {
				return signed, false, err
			}
		}
		err = s.Store.InTx(ctx, lockKey(channelID), func(tx *store.Tx) error {
			keys, err := tx.ChannelKeys(ctx, channelID)
			if err != nil {
				return err
			}
			for _, k := range keys {
				if k.ID == active.ID && k.State == store.KeyRetired {
					return errResign
				}
			}
			for i, r := range rels {
				if err := tx.InsertSignature(ctx, r.ID, channelID, active.ID, sigs[i]); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, errResign) {
			return signed, false, err
		}
		if err == nil {
			signed += len(rels)
		}
	}
}

// moveServing moves the serving key to key when no non-yanked release lacks its signature.
func (s *Service) moveServing(ctx context.Context, channelID, key string) (bool, error) {
	moved := false
	err := s.Store.InTx(ctx, lockKey(channelID), func(tx *store.Tx) error {
		c, err := tx.ChannelByID(ctx, channelID)
		if err != nil {
			return err
		}
		keys, err := tx.ChannelKeys(ctx, channelID)
		if err != nil {
			return err
		}
		stillActive := false
		for _, k := range keys {
			stillActive = stillActive || k.ID == key && k.State == store.KeyActive
		}
		if !stillActive {
			return errResign
		}
		if c.ServingKeyID == key || c.ServingKeyID == "" {
			return nil // nothing to move; an empty channel gets its serving key with its first release
		}
		missing, _, err := tx.UnsignedReleases(ctx, channelID, key, 1)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return errResign
		}
		if err := tx.SetServingKey(ctx, channelID, c.ServingKeyID, key); err != nil {
			return err
		}
		moved = true
		return tx.BumpReleaseVersion(ctx, channelID)
	})
	return moved, err
}

// Resigner runs Resign for every channel that needs it, one replica per channel at a time.
type Resigner struct {
	Service  *Service
	Holder   string // this replica
	Interval time.Duration
	Log      *slog.Logger
}

// Run loops until ctx is done.
func (r *Resigner) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = time.Minute
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes one pass over the channels that need re-signing.
func (r *Resigner) Once(ctx context.Context) {
	ids, err := r.Service.Store.ChannelsToResign(ctx)
	if err != nil {
		r.Log.Error("release: listing channels to re-sign", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		lease := "resign/" + id
		held, err := r.Service.Store.AcquireLease(ctx, lease, r.Holder, 2*time.Minute)
		if err != nil {
			r.Log.Error("release: taking the re-sign lease", "channel", id, "error", err)
			continue
		}
		if !held {
			continue
		}
		renew := func(ctx context.Context) error {
			if ok, err := r.Service.Store.AcquireLease(ctx, lease, r.Holder, 2*time.Minute); err != nil || !ok {
				return errors.New("release: the re-sign lease was lost")
			}
			return nil
		}
		signed, moved, err := r.Service.Resign(ctx, id, renew)
		if err != nil {
			r.Log.Error("release: re-signing a channel", "channel", id, "error", err)
		} else if signed > 0 || moved {
			r.Log.Info("release: re-signed a channel", "channel", id, "signatures", signed, "serving_key_moved", moved)
		}
		_ = r.Service.Store.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder)
	}
}
