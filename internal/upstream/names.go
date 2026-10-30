package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0039 PR 2: "*" on a mirror of a kista channel.
const (
	// maxRemoteNames is the most names a run reads of a remote's -/names (kista's 500 a page); a
	// remote listing more is a problem, and the read is cut (it deletes nothing).
	maxRemoteNames = 10000
	maxNamesPages  = maxRemoteNames/500 + 1
	maxNamesPage   = 1 << 20
	// a run's count of the names its "*" keeps
	countDiscovered = "discovered"
	outcomeNames    = "names" // a problem of the discovery
)

// remoteNames is what a read of a remote's -/names got.
type remoteNames struct {
	names    []string
	complete bool // every page was read
}

// readNames reads a repository upstream's -/names, every page, with its credential.
func (s *Service) readNames(ctx context.Context, tenant string, u store.Upstream) (remoteNames, error) {
	var out remoteNames
	cred, tok, err := s.token(ctx, tenant, u)
	if err != nil {
		return out, err
	}
	base := s.prefix(u) + "/-/names"
	cursor, seen := "", map[string]bool{"": true}
	for page := 0; ; page++ {
		if page == maxNamesPages {
			return out, fmt.Errorf("the source's names run over %d pages: the rest is not read", maxNamesPages)
		}
		url := base
		if cursor != "" {
			url += "/" + cursor
		}
		var buf bytes.Buffer
		_, tok, err = s.getPage(ctx, cred, tok, url, "", &buf)
		switch {
		case errors.Is(err, egress.ErrNotFound):
			return out, errNotKista
		case err != nil:
			return out, err
		}
		var ans struct {
			API   *int      `json:"api"`
			Names *[]string `json:"names"`
			Next  string    `json:"next"`
		}
		if json.Unmarshal(buf.Bytes(), &ans) != nil || ans.API == nil || ans.Names == nil {
			return out, errNotKista
		}
		out.names = append(out.names, *ans.Names...)
		if len(out.names) > maxRemoteNames {
			out.names = out.names[:maxRemoteNames]
			return out, fmt.Errorf("the source lists over %d names: the rest is not read", maxRemoteNames)
		}
		if ans.Next == "" {
			out.complete = true
			return out, nil
		}
		if len(*ans.Names) == 0 {
			return out, errors.New("the source pages its names on past an empty page")
		}
		if seen[ans.Next] || !cursorRe.MatchString(ans.Next) {
			return out, errors.New("the source's names page to a cursor that is not one")
		}
		seen[ans.Next] = true
		cursor = ans.Next
	}
}

// discover reads a "*" mirror's names from its kista remote and keeps those its "*" takes (spec
// 0039): a name that passes a pull-through "*"'s checks (the grammar, not reserved server-wide, not
// an alias), that no upstream of the channel lists or discovered first, and that no source of the
// tenant other than an upstream holds in a channel; at most MaxDiscoveredNames, by name. Only a
// complete listing within the cap removes names absent from it. A change renews the tenant's
// snapshots. It answers the upstream as it is then.
func (s *Service) discover(ctx context.Context, sc store.ServeChannel, u store.Upstream, res *Result) (store.Upstream, error) {
	problem := func(detail string) {
		if len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, CellNote{Outcome: outcomeNames, Detail: detail})
		}
	}
	got, rerr := s.readNames(ctx, sc.Tenant.Name, u)
	if rerr != nil {
		problem(rerr.Error())
		if len(got.names) == 0 {
			return u, nil
		}
	}
	var names []string
	var over int
	err := s.Store.InTenantTx(ctx, u.TenantID, []string{store.TenantUpstreamLock(u.TenantID)}, func(tx *store.Tx) error {
		cur, err := tx.GetUpstreamByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if !cur.AnyMirror() { // "*" removed meanwhile
			u = cur
			return nil
		}
		listed, found, err := tx.ChannelNames(ctx, cur.ChannelID, cur.ID)
		if err != nil {
			return err
		}
		held, err := tx.TenantNamesOfOtherSources(ctx, cur.TenantID)
		if err != nil {
			return err
		}
		names, over = nil, 0
		taken := map[string]bool{}
		for _, n := range got.names {
			if taken[n] || release.ValidName(n) != nil || !release.AnyTakes(pkgtype.DuckDBExtension, cur.Kind, n) ||
				listed[n] || found[n] || held[n] || slices.ContainsFunc(cur.Entries, func(e store.UpstreamEntry) bool { return e.Name == n }) {
				continue
			}
			taken[n] = true
			names = append(names, n)
		}
		slices.Sort(names)
		if len(names) > store.MaxDiscoveredNames {
			over = len(names) - store.MaxDiscoveredNames
			names = names[:store.MaxDiscoveredNames]
		}
		if res.DryRun {
			u = cur
			return nil
		}
		changed, err := tx.SetUpstreamNames(ctx, cur, names, rerr == nil && got.complete && over == 0)
		if err != nil || !changed {
			u = cur
			return err
		}
		if err := bumpTenant(ctx, tx, cur.TenantID); err != nil {
			return err
		}
		u, err = tx.GetUpstreamByID(ctx, cur.ID)
		return err
	})
	if err != nil {
		return u, err
	}
	if over > 0 {
		problem(fmt.Sprintf("the source lists %d names more than \"*\" keeps (%d)", over, store.MaxDiscoveredNames))
	}
	res.Counts[countDiscovered] = len(names)
	return u, nil
}
