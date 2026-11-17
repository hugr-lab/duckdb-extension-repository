package attachment

import (
	"context"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Test hooks (spec 0046): a later binary's reader level, and a kind's declared floor.

// SetReaderLevel makes f read at another reader level, as a later (or an older) binary would.
func (f *Feeds) SetReaderLevel(n int) { f.level = n }

// DeclareFloor declares a kind's floor, as a kind of a later release does; the returned function
// withdraws it.
func DeclareFloor(kind string, floor Floor) func() {
	prev, had := floors[kind]
	floors[kind] = floor
	return func() {
		if had {
			floors[kind] = prev
		} else {
			delete(floors, kind)
		}
	}
}

// FailWaitingRead makes the read of what a feed passed over, after a change, fail with err.
func (f *Feeds) FailWaitingRead(err error) {
	f.readWaiting = func(context.Context, *store.Feed) error { return err }
}
