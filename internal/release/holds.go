package release

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033 phase A2: a channel's hold.

// ChannelHold reads a channel's hold (its administrators').
func (s *Service) ChannelHold(ctx context.Context, a authz.Actor, tenant, channel string) (time.Duration, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return 0, err
	}
	return ch.Hold, nil
}

// SetChannelHold sets a signed channel's hold (its administrators'): an automatic placement into it
// (a rule's, a follow's, a mirror's run) waits until an entry's arrival plus the hold, if that is
// later than the entry's own; placing by hand and promoting do not wait for it. A channel with a hold
// asks no lazy or pull-through upstream. It returns the hold it had.
func (s *Service) SetChannelHold(ctx context.Context, a authz.Actor, tenant, channel string, d time.Duration) (time.Duration, error) {
	ch, err := s.mayAdminChannel(ctx, a, tenant, channel)
	if err != nil {
		return 0, err
	}
	if err := store.ValidHold(d); err != nil {
		return 0, err
	}
	if ch.Kind != store.ChannelSigned {
		return 0, fmt.Errorf("%w: a passthrough channel serves DuckDB's core builds as they come, without a hold", store.ErrInvalid)
	}
	var before time.Duration
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		var err error
		if before, err = tx.SetChannelHold(ctx, ch.ID, d); err != nil {
			return err
		}
		return tx.Event(ctx, ch.TenantID, a.String(), "catalogue.hold", "channel:"+ch.Name,
			map[string]any{"channel": ch.Name, "hold": d.String(), "before": before.String()})
	})
	return before, err
}
