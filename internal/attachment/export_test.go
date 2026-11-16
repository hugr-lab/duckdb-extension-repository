package attachment

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
