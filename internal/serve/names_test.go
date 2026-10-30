package serve

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0039 PR 2: GET <prefix>/-/names answers the names the channel serves in the caller's view
// (anonymous: those with a public release; a token: the names it holds install on too), paged by
// path, its ETag changing with a grant; a name only yanked or blocked is not served; a suspended
// tenant, a passthrough or a missing channel answer as a missing path.
func TestNamesRoute(t *testing.T) {
	en, idp, adm := authEnv(t, "https")
	tok := idp.token(t, nil)
	en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "acl"})
	en.add(t, ext(t, 2000, 2, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor", Private: true})
	yanked := en.add(t, ext(t, 2000, 3, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "gone"})
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "gone", yanked.ID, release.Yank, 0); err != nil {
		t.Fatal(err)
	}
	const path = "/acme/prod/-/names"
	read := func(p string, hdr map[string]string) (answer, NamesAnswer) {
		t.Helper()
		a := en.do(t, "GET", p, hdr)
		var ans NamesAnswer
		if a.status == 200 {
			if err := json.Unmarshal(a.body, &ans); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		}
		return a, ans
	}
	a, ans := read(path, nil)
	if a.status != 200 || ans.API != 1 || len(ans.Names) != 1 || ans.Names[0] != "acl" || a.header.Get("Vary") != "Authorization" {
		t.Fatalf("anonymous: %d %+v", a.status, ans)
	}
	etag := a.header.Get("ETag")
	if a := en.do(t, "GET", path, map[string]string{"If-None-Match": etag}); a.status != 304 {
		t.Fatalf("conditional: %d", a.status)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Package: "tresor"}); err != nil {
		t.Fatal(err)
	}
	a, ans = read(path, bearerHdr(tok))
	if len(ans.Names) != 2 || ans.Names[1] != "tresor" || a.header.Get("ETag") == etag {
		t.Fatalf("granted: %+v %v", ans, a.header)
	}
	// paging by path: after acl, the rest
	if _, ans := read(path+"/"+base64.RawURLEncoding.EncodeToString([]byte("acl")), bearerHdr(tok)); len(ans.Names) != 1 || ans.Names[0] != "tresor" {
		t.Fatalf("a page after acl: %+v", ans)
	}
	if a := en.do(t, "GET", path+"?cursor=x", nil); a.status != 404 {
		t.Fatalf("a query string: %d", a.status)
	}
	// suspended: nobody's
	ten := &tenants.Service{Store: en.st, Authz: authz.ServerAdmin{}}
	if _, err := ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0); err != nil {
		t.Fatal(err)
	}
	if a, _ := read(path, bearerHdr(tok)); a.status != 404 {
		t.Fatalf("suspended: %d", a.status)
	}
	if _, err := ten.SetTenantState(ctx, admin, "acme", store.TenantActive, 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/acme/mirror/-/names", "/acme/nope/-/names"} {
		if a := en.do(t, "GET", p, nil); a.status != 404 {
			t.Fatalf("%s: %d", p, a.status)
		}
	}
}

// Spec 0039 PR 2: a name is served while it has an active or deprecated release whose body is not
// blocked; public when one of those is public.
func TestNamesIndex(t *testing.T) {
	snap := &release.Snapshot{Blocked: map[string]bool{"b": true}}
	for _, c := range []struct{ name, state, vis, hash string }{{"a", store.ReleaseActive, store.Public, "h"}, {"b", store.ReleaseDeprecated, store.Private, "h"},
		{"c", store.ReleaseYanked, store.Public, "h"}, {"d", store.ReleaseActive, store.Public, "b"}} {
		snap.Releases = append(snap.Releases, store.Candidate{Release: store.Release{Name: c.name, State: c.state, Visibility: c.vis}, BodyHash: c.hash})
	}
	x := snap.NamesIndex()
	if len(x.All) != 2 || x.All[0] != "a" || x.All[1] != "b" || !x.Public["a"] || x.Public["b"] {
		t.Fatalf("index: %+v", x)
	}
}
