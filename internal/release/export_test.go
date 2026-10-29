package release

import "github.com/hugr-lab/duckdb-extension-repository/internal/store"

// SetAfterBuild sets a hook run between finding an intake's Build and inserting its release.
func SetAfterBuild(s *Service, f func(store.Build)) { s.afterBuild = f }

// SetBeforeResign sets a hook run between reading a re-sign batch and inserting its signatures.
func SetBeforeResign(s *Service, f func([]store.Release)) { s.beforeResign = f }

// CheckUpstreamSlot is an upstream item's slot check.
var CheckUpstreamSlot = checkUpstreamSlot

// ServedHeld is what a source's releases of a name hold in a channel, by version (newServed's held).
func ServedHeld(cands []store.Candidate, capis map[string][]store.CAPI) map[string]bool {
	return newServed(cands, capis).held
}

// DistributorMayNow is a distributor's name check in a landing transaction.
var DistributorMayNow = distributorMayNow

// SetBeforeMetaCommit sets a hook run between a metadata write's checks (and its upload) and its
// transaction (spec 0036).
func SetBeforeMetaCommit(s *Service, f func()) { s.beforeMetaCommit = f }
