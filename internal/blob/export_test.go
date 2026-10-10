package blob

import "time"

// SetBusyPoll sets how often a commit waiting on a delete tries again, until the test ends.
func SetBusyPoll(t interface{ Cleanup(func()) }, d time.Duration) {
	old := busyPoll
	busyPoll = d
	t.Cleanup(func() { busyPoll = old })
}

// SetClock sets the record cache's clock, until the test ends.
func SetClock(t interface{ Cleanup(func()) }, now func() time.Time) {
	old := clock
	clock = now
	t.Cleanup(func() { clock = old })
}
