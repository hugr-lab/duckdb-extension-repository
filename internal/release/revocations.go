package release

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// RevokedRow is one row of a channel's -/revocations route (spec 0039): a release the channel
// released, of any source, that is revoked, with its strongest state and that state's reason.
type RevokedRow struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	BodyHash string `json:"body_hash"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

// Key orders and pages the rows.
func (r RevokedRow) Key() [4]string { return [4]string{r.Name, r.Version, r.Platform, r.BodyHash} }

// RevokedIndex is a snapshot's -/revocations rows, computed once and kept with the snapshot (so it
// lives as long as the snapshot does): every row (the view of a caller holding install on its name)
// and the public view's, both in key order. A view's rows are a function of the two and of the names
// it holds install on, so its ETag is derived from Digest and those names; an anonymous view's, from
// PublicDigest alone (the public rows).
type RevokedIndex struct {
	All          []RevokedRow
	Public       map[[4]string]RevokedRow
	Names        []string // the names of All's rows, once each, in order
	Digest       []byte
	PublicDigest []byte
}

// RevokedIndex answers the snapshot's index, computing it on first use.
func (s *Snapshot) RevokedIndex() *RevokedIndex {
	s.revOnce.Do(func() {
		x := &RevokedIndex{All: s.RevokedRows(Everything), Public: map[[4]string]RevokedRow{}}
		sum := sha256.New()
		for _, r := range x.All {
			if len(x.Names) == 0 || x.Names[len(x.Names)-1] != r.Name {
				x.Names = append(x.Names, r.Name)
			}
			b, _ := json.Marshal(r)
			sum.Write(b)
		}
		sum.Write([]byte{0})
		pub := sha256.New()
		for _, r := range s.RevokedRows(func(c *store.Candidate) bool { return c.Visibility == store.Public }) {
			x.Public[r.Key()] = r
			b, _ := json.Marshal(r)
			sum.Write(b)
			pub.Write(b)
		}
		x.Digest, x.PublicDigest = sum.Sum(nil), pub.Sum(nil)
		s.rev = x
	})
	return s.rev
}

// RevokedRows lists the channel's revoked rows a view sees, ordered by (name, version, platform, body
// hash), one row each, at its strongest state:
//   - withdrawn: its build was withdrawn (spec 0033 phase B), the distributor's reason;
//   - blocked: its body is blocked by an administrator (spec 0008);
//   - yanked: the release is yanked;
//   - for a release this kista yanked applying an upstream's revocation, that revocation's state and
//     reason (an upstream block's), so a chain carries the strongest state on.
func (s *Snapshot) RevokedRows(vis Visible) []RevokedRow {
	best := map[[4]string]RevokedRow{}
	for i := range s.Revoked {
		c := &s.Revoked[i]
		if !vis(c) {
			continue
		}
		row := RevokedRow{Name: c.Name, Version: c.PackageVersion, Platform: c.Platform, BodyHash: c.BodyHash}
		take := func(state, reason string) {
			if store.RevocationRank(state) > store.RevocationRank(row.State) {
				row.State, row.Reason = state, reason
			}
		}
		if c.Withdrawn {
			take(store.RevokeWithdrawn, c.WithdrawnReason)
		}
		if s.Blocked[c.BodyHash] {
			take(store.RevokeBlocked, "")
		}
		// a release of an upstream's build whose body that upstream blocked shows the block, whatever
		// yanked it (the upstream's yank first, or an administrator's: the block's sweep finds it
		// yanked already)
		if id, ok := strings.CutPrefix(c.Source, store.SourceUpstreamPrefix); ok {
			if b, ok := s.UpstreamBlocks[[2]string{id, c.BodyHash}]; ok {
				take(b.State, b.Reason)
			}
		}
		if c.State == store.ReleaseYanked {
			take(store.RevokeYanked, "")
		}
		if row.State == "" {
			continue
		}
		if cur, ok := best[row.Key()]; !ok || store.RevocationRank(row.State) > store.RevocationRank(cur.State) {
			best[row.Key()] = row
		}
	}
	out := make([]RevokedRow, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RevokedRow) int {
		ka, kb := a.Key(), b.Key()
		for i := range ka {
			if c := cmp.Compare(ka[i], kb[i]); c != 0 {
				return c
			}
		}
		return 0
	})
	return out
}
