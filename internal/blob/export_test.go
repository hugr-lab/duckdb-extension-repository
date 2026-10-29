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

// SetVerifyPage sets the records a verification pass reads at once (spec 0018), for a test.
func SetVerifyPage(n int) (restore func()) {
	old := verifyPage
	verifyPage = n
	return func() { verifyPage = old }
}

// SetImportPause sets the wait before an import object is re-opened, until the test ends.
func SetImportPause(t interface{ Cleanup(func()) }, d time.Duration) {
	old := importPause
	importPause = d
	t.Cleanup(func() { importPause = old })
}

// SetIconClaimWait sets how long an icon's write waits on the collector (spec 0036), until the test
// ends.
func SetIconClaimWait(t interface{ Cleanup(func()) }, d time.Duration) {
	old := iconClaimWait
	iconClaimWait = d
	t.Cleanup(func() { iconClaimWait = old })
}
