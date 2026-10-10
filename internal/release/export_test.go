package release

import "github.com/hugr-lab/duckdb-extension-repository/internal/store"

// SetAfterBuild sets a hook run between finding an intake's Build and inserting its release.
func SetAfterBuild(s *Service, f func(store.Build)) { s.afterBuild = f }

// SetBeforeResign sets a hook run between reading a re-sign batch and inserting its signatures.
func SetBeforeResign(s *Service, f func([]store.Release)) { s.beforeResign = f }

// CheckUpstreamSlot is an upstream item's slot check.
var CheckUpstreamSlot = checkUpstreamSlot
