package release

import (
	"context"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

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

// SetBeforeTell sets a hook run before a tell's transaction; an error it returns fails the tell
// (spec 0041).
func SetBeforeTell(s *Service, f func() error) { s.beforeTell = f }

// LookAndTell is the placement pass's look at a rule and its tell (spec 0041), as Placer makes them
// for a rule it read once.
func LookAndTell(ctx context.Context, s *Service, r store.DueRule) error {
	start := s.Store.Now()
	res, err := s.placeRule(ctx, r, nil)
	if err != nil {
		return err
	}
	return s.tellWaiting(ctx, r, start, res)
}

// UsageErr is a room check's error as an intake answers it (spec 0038).
var UsageErr = usageErr

// SetBeforeRetire sets a hook run between the retention pass's look and each retirement's
// transaction (spec 0042).
func SetBeforeRetire(s *Service, f func()) { s.beforeRetire = f }

// Retire is the retention computation over a channel's releases (spec 0042): the ids it retires,
// each (source, name, line)'s kept versions (key source/name@line), and the releases another line
// keeps (by id).
func Retire(rels []store.Candidate, capis map[string][]store.CAPI, pins []store.RetentionPin, keep int) ([]string, map[string][]string, map[string][]string) {
	keys, by := groups(rels)
	var retired []string
	kept, keptBy := map[string][]string{}, map[string][]string{}
	for _, k := range keys {
		g := computeGroup(by[k], capis, pins, keep, false)
		for _, c := range g.retire {
			retired = append(retired, c.ID)
		}
		for l, vs := range g.kept {
			kept[k[0]+"/"+k[1]+"@"+l] = vs
		}
		for id, ls := range g.keptBy {
			keptBy[id] = ls
		}
	}
	slices.Sort(retired)
	return retired, kept, keptBy
}
