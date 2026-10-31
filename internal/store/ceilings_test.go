package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0038, amended 2026-10-31: the ceilings in force are the configuration's (SetCeilings), not
// constants. A lower channels ceiling refuses the next channel at it (a limit at its ceiling); a
// tenant value above it is invalid; the defaults' unlimited is that ceiling; the longest hold
// follows too. Not parallel: the ceilings are the process's.
func TestConfiguredCeilings(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s) // one channel: prod
		c := store.DefaultCeilings()
		c.Channels, c.MaxHold = 2, time.Hour
		store.SetCeilings(c)
		t.Cleanup(func() { store.SetCeilings(store.DefaultCeilings()) })
		if store.CurrentCeilings() != c {
			t.Fatalf("in force: %+v", store.CurrentCeilings())
		}
		channel := func(name string) error {
			ch := store.Channel{TenantID: tn.ID, Name: name, Kind: store.ChannelSigned}
			return s.InTenantTxForUpdate(ctx, tn.ID, nil, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &ch) })
		}
		if err := channel("dev"); err != nil {
			t.Fatal(err)
		}
		var le *store.LimitError
		if err := channel("qa"); !errors.As(err, &le) || le.Limit != store.LimitChannels || !le.AtCeiling {
			t.Fatalf("a channel past the configured ceiling: %v", err)
		}
		if err := store.ValidLimit(store.LimitChannels, 3); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a limit above the ceiling: %v", err)
		}
		if in := s.Effective(store.DefaultLimits()); in.Channels != 2 {
			t.Fatalf("unlimited is the ceiling: %+v", in)
		}
		if err := store.ValidHold(2 * time.Hour); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a hold past the configured longest: %v", err)
		}
		if err := (store.Ceilings{}).Check(); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("zero ceilings: %v", err)
		}
	})
}
