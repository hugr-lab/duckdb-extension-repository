package release

import (
	"crypto/sha256"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// NamesIndex is a snapshot's -/names answer before a caller's view (spec 0039): the names the
// channel serves (a release active or deprecated, its body not blocked), in order, and those it
// serves publicly. A view is every name it holds install on and the public others, so its ETag is
// derived from Digest and those names. Computed once and kept with the snapshot.
type NamesIndex struct {
	All    []string
	Public map[string]bool
	Digest []byte
}

// NamesIndex answers the snapshot's names, computing them on first use.
func (s *Snapshot) NamesIndex() *NamesIndex {
	s.namesOnce.Do(func() {
		x := &NamesIndex{Public: map[string]bool{}}
		seen := map[string]bool{}
		for i := range s.Releases {
			c := &s.Releases[i]
			if c.State != store.ReleaseActive && c.State != store.ReleaseDeprecated || s.Blocked[c.BodyHash] {
				continue
			}
			if !seen[c.Name] {
				seen[c.Name] = true
				x.All = append(x.All, c.Name)
			}
			if c.Visibility == store.Public {
				x.Public[c.Name] = true
			}
		}
		slices.Sort(x.All)
		sum := sha256.New()
		for _, n := range x.All {
			sum.Write([]byte(n + "\x00"))
			if x.Public[n] {
				sum.Write([]byte{1})
			}
		}
		x.Digest = sum.Sum(nil)
		s.names = x
	})
	return s.names
}
