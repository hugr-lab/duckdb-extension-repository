package attachment

import (
	"context"
	"slices"

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

// AfterRelayRead runs fn between a relayed answer's reads and its check that nothing changed.
func (s *Service) AfterRelayRead(fn func()) { s.afterRelayRead = fn }

// ForgetKind makes this binary one that does not know a kind yet, as the release before the kind
// was added; the returned function brings it back.
func ForgetKind(kind string) func() {
	kinds, types, floor := slices.Clone(Kinds), mediaTypes[kind], floors[kind]
	Kinds = slices.DeleteFunc(Kinds, func(k string) bool { return k == kind })
	delete(mediaTypes, kind)
	delete(floors, kind)
	return func() {
		Kinds, mediaTypes[kind] = kinds, types
		if floor != nil {
			floors[kind] = floor
		}
	}
}

// FailWaitingRead makes the read of what a feed passed over, after a change, fail with err.
func (f *Feeds) FailWaitingRead(err error) {
	f.readWaiting = func(context.Context, *store.Feed) error { return err }
}
