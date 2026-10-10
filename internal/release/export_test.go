package release

import "github.com/hugr-lab/duckdb-extension-repository/internal/store"

// SetAfterBuild sets a hook run between finding an intake's Build and inserting its release.
func SetAfterBuild(s *Service, f func(store.Build)) { s.afterBuild = f }
