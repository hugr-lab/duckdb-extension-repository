package serve

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0039: GET <prefix>/-/revocations answers the channel's revoked rows in the caller's view
// (anonymous: public releases; a token: the names it holds install on too), paged by path, its
// ETag changing with a grant; a query string is no route; a suspended tenant answers its install
// holders only, a deleting one nobody; a release of a source the channel no longer selects is still
// a row.
func TestRevocationsRoute(t *testing.T) {
	en, idp, adm := authEnv(t, "https")
	tok := idp.token(t, nil)
	pub := en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
	priv := en.add(t, ext(t, 2000, 2, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor", Private: true})
	en.add(t, ext(t, 2000, 3, cpp("2.0")), release.AddOptions{Unchecked: true, Name: "acl"})
	for _, r := range []store.Release{pub, priv} {
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", r.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
	}
	const path = "/acme/prod/-/revocations"
	read := func(p string, hdr map[string]string) (answer, RevocationsAnswer) {
		t.Helper()
		a := en.do(t, "GET", p, hdr)
		var ans RevocationsAnswer
		if a.status == 200 {
			if err := json.Unmarshal(a.body, &ans); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		}
		return a, ans
	}
	// anonymous: the public row only
	a, ans := read(path, nil)
	if a.status != 200 || ans.API != 1 || len(ans.Rows) != 1 || ans.Rows[0].Version != "1.0" || ans.Rows[0].State != store.RevokeYanked ||
		a.header.Get("ETag") == "" || a.header.Get("Vary") != "Authorization" {
		t.Fatalf("anonymous: %d %+v %v", a.status, ans, a.header)
	}
	etag := a.header.Get("ETag")
	if a := en.do(t, "GET", path, map[string]string{"If-None-Match": etag}); a.status != 304 {
		t.Fatalf("conditional: %d", a.status)
	}
	// a private row added moves no anonymous ETag: it hashes the public view only
	hidden := en.add(t, ext(t, 2000, 4, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "hidden", Private: true})
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "hidden", hidden.ID, release.Yank, 0); err != nil {
		t.Fatal(err)
	}
	if a := en.do(t, "GET", path, map[string]string{"If-None-Match": etag}); a.status != 304 {
		t.Fatalf("a private row moved the anonymous ETag: %d", a.status)
	}
	if a := en.do(t, "GET", path+"?cursor=x", nil); a.status != 404 {
		t.Fatalf("a query string: %d", a.status)
	}
	// a token without a grant sees what anonymous sees; with install on the name, the private row too,
	// under another ETag
	if _, ans := read(path, bearerHdr(tok)); len(ans.Rows) != 1 {
		t.Fatalf("a token without a grant: %+v", ans)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Package: "tresor"}); err != nil {
		t.Fatal(err)
	}
	a, ans = read(path, bearerHdr(tok))
	if len(ans.Rows) != 2 || a.header.Get("ETag") == etag {
		t.Fatalf("granted: %+v %v", ans, a.header)
	}
	if a := en.do(t, "GET", path, map[string]string{"Authorization": "Bearer " + tok, "If-None-Match": etag}); a.status != 200 {
		t.Fatalf("the grant's view under the anonymous ETag: %d", a.status)
	}
	// an administrator's block is stronger than a yank
	if _, _, err := en.rel.Block(ctx, admin, "acme", candidate(t, en, priv).BodyHash, "malware"); err != nil {
		t.Fatal(err)
	}
	if _, ans := read(path, bearerHdr(tok)); len(ans.Rows) != 2 || ans.Rows[1].State != store.RevokeBlocked {
		t.Fatalf("blocked: %+v", ans)
	}
	// a release of a source the channel no longer selects is still a row
	if err := en.rel.ClearSource(ctx, admin, "acme", "prod", "tresor"); err != nil {
		t.Fatal(err)
	}
	if _, ans := read(path, bearerHdr(tok)); len(ans.Rows) != 2 {
		t.Fatalf("a source no longer selected: %+v", ans)
	}
	// a suspended tenant: install holders only, the names they hold it on
	ten := &tenants.Service{Store: en.st, Authz: authz.ServerAdmin{}}
	if _, err := ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0); err != nil {
		t.Fatal(err)
	}
	if a, _ := read(path, nil); a.status != 404 {
		t.Fatalf("suspended, anonymous: %d", a.status)
	}
	if a, _ := read(path, bearerHdr(idp.token(t, map[string]any{"sub": "bob"}))); a.status != 404 {
		t.Fatalf("suspended, a token holding no install: %d", a.status)
	}
	if a, ans := read(path, bearerHdr(tok)); a.status != 200 || len(ans.Rows) != 2 {
		t.Fatalf("suspended, an install holder: %d %+v", a.status, ans)
	}
	if a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", bearerHdr(tok)); a.status != 404 {
		t.Fatalf("suspended, a binary: %d", a.status)
	}
	if _, err := ten.SetTenantState(ctx, admin, "acme", store.TenantActive, 0); err != nil {
		t.Fatal(err)
	}
	// a passthrough channel and a missing one: as a missing path
	for _, p := range []string{"/acme/mirror/-/revocations", "/acme/nope/-/revocations"} {
		if a := en.do(t, "GET", p, nil); a.status != 404 {
			t.Fatalf("%s: %d", p, a.status)
		}
	}
}

// Spec 0039: the route pages by path, 1,000 rows a page, each page conditional on its own ETag.
func TestRevocationsPaging(t *testing.T) {
	en := newEnv(t, Options{}, "https")
	snap := &release.Snapshot{}
	for i := range RevocationsPage + 1 {
		snap.Revoked = append(snap.Revoked, store.Candidate{Release: store.Release{ID: strconv.Itoa(i), Name: "n", PackageVersion: "1." + strconv.Itoa(i),
			Platform: "linux_amd64", State: store.ReleaseYanked, Visibility: store.Public}, BodyHash: "h"})
	}
	rows := snap.RevokedRows(release.Everything)
	if len(rows) != RevocationsPage+1 {
		t.Fatalf("rows: %d", len(rows))
	}
	cur := encodeCursor(rows[RevocationsPage-1].Key())
	if k, ok := decodeCursor(cur); !ok || k != rows[RevocationsPage-1].Key() || !keyAfter(rows[RevocationsPage].Key(), k) {
		t.Fatalf("the cursor: %v %v", k, ok)
	}
	if a := en.do(t, "GET", "/acme/prod/-/revocations/"+cur, nil); a.status != 200 {
		t.Fatalf("a page by path: %d", a.status)
	}
	if a := en.do(t, "GET", "/acme/prod/-/revocations/not%20one", nil); a.status != 404 {
		t.Fatalf("a malformed cursor: %d", a.status)
	}
}

// candidate reads a release with its build.
func candidate(t *testing.T, en *env, r store.Release) store.Candidate {
	t.Helper()
	rs, err := en.rel.List(ctx, admin, "acme", "prod", r.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rs {
		if c.ID == r.ID {
			return c
		}
	}
	t.Fatalf("no release %s", r.ID)
	return store.Candidate{}
}
