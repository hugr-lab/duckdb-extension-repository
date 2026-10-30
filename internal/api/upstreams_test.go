package api_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0009 phase 1a: upstream management, tenant administrators only.
func TestUpstreams(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const U = "/api/v1/tenants/acme/upstreams"
	core := `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`
	// who: tenant administrators; others get 404, anonymous 401
	for who, want := range map[string]int{"anonymous": 401, "no grants": 404, "install": 404, "ext admin": 404, "channel adm": 404,
		"other admin": 401} {
		if r := m.call(t, "POST", U, m.toks[who], core); r.status != want {
			t.Errorf("%s adds an upstream: %d", who, r.status)
		}
	}
	r := m.call(t, "POST", U, ta, core)
	if r.status != 201 || r.header.Get("Location") != U+"/core" || r.header.Get("ETag") != `"v1"` {
		t.Fatalf("add: %d %s %v", r.status, r.body, r.header)
	}
	if u := r.json(t); u["channel"] != "staging" || u["visibility"] != "private" || u["run"] != "idle" || u["prefix"] != nil {
		t.Errorf("the upstream: %v", u)
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"the same name":    {core, 409},
		"an alias":         {`{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"extensions":[{"name":"postgres"}]}`, 400},
		"a name on two":    {`{"name":"x","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`, 409},
		"no channel":       {`{"name":"x","kind":"duckdb-core","channel":"nope","platforms":["linux_amd64"]}`, 400},
		"a kind":           {`{"name":"x","kind":"enterest","channel":"prod","platforms":["linux_amd64"]}`, 400},
		"an unknown field": {`{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"bogus":1}`, 400},
	} {
		if r := m.call(t, "POST", U, ta, tc.body); r.status != tc.want {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// the collision names the names
	if r := m.call(t, "POST", U, ta, `{"name":"x","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`); !strings.Contains(string(r.body), "tresor") {
		t.Errorf("a collision's message: %s", r.body)
	}
	// the reservation shows in the index: tresor's own releases in prod shadow the upstream's name
	ix := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/extensions/tresor", ta, "")
	if !strings.Contains(string(ix.body), `"shadows":"upstream"`) {
		t.Errorf("the index's shadows: %s", ix.body)
	}
	// the extension's administrator does not manage upstream entries
	if r := m.call(t, "DELETE", U+"/core/extensions/tresor", m.toks["ext admin"], ""); r.status != 404 {
		t.Errorf("an extension admin removes an entry: %d", r.status)
	}
	// state changes need If-Match
	if r := m.call(t, "POST", U+"/core/pause", ta, ""); r.status != 428 {
		t.Errorf("pause without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/core/pause", ta, "", "If-Match", `"v9"`); r.status != 412 {
		t.Errorf("pause, stale: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/core/pause", ta, "", "If-Match", `"v1"`); r.status != 200 || r.json(t)["state"] != "paused" {
		t.Fatalf("pause: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/sync", ta, ""); r.status != 409 {
		t.Errorf("sync a paused upstream: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/core/resume", ta, "", "If-Match", "*"); r.status != 200 {
		t.Fatalf("resume: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/public", ta, "", "If-Match", "*"); r.status != 200 || r.json(t)["visibility"] != "public" {
		t.Fatalf("public: %d %s", r.status, r.body)
	}
	etag := m.call(t, "GET", U+"/core", ta, "").header.Get("ETag")
	if r := m.call(t, "POST", U+"/core/private", ta, "", "If-Match", etag); r.status != 200 || r.json(t)["visibility"] != "private" {
		t.Fatalf("private: %d %s", r.status, r.body)
	}
	// a change to the allowlist is a new version of the record
	before := m.call(t, "GET", U+"/core", ta, "").header.Get("ETag")
	m.call(t, "POST", U+"/core/platforms", ta, `{"platform":"linux_arm64"}`)
	if after := m.call(t, "GET", U+"/core", ta, "").header.Get("ETag"); after == before {
		t.Errorf("the ETag did not move: %s", after)
	}
	if r := m.call(t, "POST", U+"/core/sync", ta, ""); r.status != 202 || r.json(t)["run"] != "requested" {
		t.Errorf("sync: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/sync?dry_run=true", ta, ""); r.status != 202 || r.json(t)["run"] != "requested" {
		t.Errorf("a dry sync after a real one: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/sync?dry_run=maybe", ta, ""); r.status != 400 {
		t.Errorf("dry_run=maybe: %d", r.status)
	}
	// entries, platforms, keys
	if r := m.call(t, "POST", U+"/core/extensions", ta, `{"name":"json"}`); r.status != 201 {
		t.Errorf("add an entry: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/extensions", ta, `{"name":"json","versions":["abc"]}`); r.status != 200 {
		t.Errorf("replace an entry: %d %s", r.status, r.body)
	}
	// the allowlist by name, paged, with a prefix (spec 0015 phase 2b)
	for _, n := range []string{"json_a", "json_b", "parquet"} {
		if r := m.call(t, "POST", U+"/core/extensions", ta, `{"name":"`+n+`"}`); r.status != 201 {
			t.Fatalf("add %s: %d %s", n, r.status, r.body)
		}
	}
	pg := m.call(t, "GET", U+"/core/extensions?prefix=json&limit=2", ta, "").json(t)
	nx, _ := pg["next"].(string)
	if got := names(pg, "extensions", "name"); !slices.Equal(got, []string{"json", "json_a"}) || nx == "" {
		t.Fatalf("first page: %v", pg)
	}
	if got := names(m.call(t, "GET", U+"/core/extensions?prefix=json&limit=2&cursor="+nx, ta, "").json(t), "extensions", "name"); !slices.Equal(got, []string{"json_b"}) {
		t.Errorf("second page: %v", got)
	}
	if r := m.call(t, "GET", U+"/core/extensions?prefix=json!", ta, ""); r.status != 400 {
		t.Errorf("an invalid prefix: %d", r.status)
	}
	if all := m.call(t, "GET", U+"/core/extensions", ta, "").json(t); all["next"] != nil || len(all["extensions"].([]any)) != 5 {
		t.Errorf("without a limit, the whole allowlist: %v", all)
	}
	for _, n := range []string{"json_a", "json_b", "parquet"} {
		if r := m.call(t, "DELETE", U+"/core/extensions/"+n, ta, ""); r.status != 204 {
			t.Errorf("remove %s: %d", n, r.status)
		}
	}
	if r := m.call(t, "DELETE", U+"/core/extensions/json", ta, ""); r.status != 204 {
		t.Errorf("remove an entry: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/core/platforms", ta, `{"platform":"osx_arm64"}`); r.status != 201 {
		t.Errorf("add a platform: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", U+"/core/platforms/osx_arm64", ta, ""); r.status != 204 {
		t.Errorf("remove a platform: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/core/keys", ta, `{"fingerprint":"sha256:`+strings.Repeat("a", 64)+`"}`); r.status != 400 {
		t.Errorf("a key on a core upstream: %d", r.status)
	}
	if r := m.call(t, "GET", U+"/core/cells", ta, ""); r.status != 200 || !strings.Contains(string(r.body), `"cells":[]`) {
		t.Errorf("cells: %d %s", r.status, r.body)
	}
	if l := m.call(t, "GET", U, ta, "").json(t)["upstreams"].([]any); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	// shadows (phase 1b): tenant administrators only
	if r := m.call(t, "GET", "/api/v1/tenants/acme/shadows", ta, ""); r.status != 200 || !strings.Contains(string(r.body), `"shadows":[]`) {
		t.Errorf("shadows: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/shadows", m.toks["channel adm"], ""); r.status != 404 {
		t.Errorf("shadows for a channel admin: %d", r.status)
	}
	if r := m.call(t, "DELETE", "/api/v1/tenants/acme/shadows/httpfs", ta, ""); r.status != 404 {
		t.Errorf("remove a missing shadow: %d", r.status)
	}
	// removal needs If-Match
	if r := m.call(t, "DELETE", U+"/core", ta, ""); r.status != 428 {
		t.Errorf("remove without If-Match: %d", r.status)
	}
	if r := m.call(t, "DELETE", U+"/core", ta, "", "If-Match", "*"); r.status != 204 {
		t.Errorf("remove: %d", r.status)
	}
	if r := m.call(t, "GET", U+"/core", ta, ""); r.status != 404 {
		t.Errorf("a removed upstream: %d", r.status)
	}
}

// Spec 0039: an upstream's revocations switch and stored revocations, tenant administrators only;
// the switch needs If-Match, is refused on a mirror of no kista, and an unchanged switch is no
// change; a lift of a build the upstream stores no revocation of is a missing path.
func TestUpstreamRevocations(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const U = "/api/v1/tenants/acme/upstreams"
	if _, err := m.ups.Add(ctx, admin, "acme", upstream.Spec{Name: "air", Kind: store.UpstreamCommunity, Channel: "prod",
		Mode: store.ModeBundle, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "POST", U, ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"json"}]}`); r.status != 201 {
		t.Fatalf("core: %d %s", r.status, r.body)
	}
	r := m.call(t, "GET", U+"/air/revocations", ta, "")
	if g := r.json(t); r.status != 200 || g["remote_kista"] != false || g["revocations"] != false || len(g["stored"].([]any)) != 0 ||
		r.header.Get("ETag") == "" {
		t.Fatalf("read: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", U+"/air/revocations", m.toks["channel adm"], ""); r.status != 404 {
		t.Errorf("a channel administrator reads them: %d", r.status)
	}
	for name, tc := range map[string]struct {
		path, body, match string
		want              int
	}{
		"no If-Match":        {"/air/revocations", `{"revocations":true}`, "", 428},
		"stale":              {"/air/revocations", `{"revocations":true}`, `"v9"`, 412},
		"no value":           {"/air/revocations", `{}`, "*", 400},
		"a mirror of DuckDB": {"/core/revocations", `{"revocations":true}`, "*", 400},
		"a missing one":      {"/nope/revocations", `{"revocations":true}`, "*", 404},
	} {
		hdr := []string{}
		if tc.match != "" {
			hdr = []string{"If-Match", tc.match}
		}
		if r := m.call(t, "POST", U+tc.path, ta, tc.body, hdr...); r.status != tc.want {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if r := m.call(t, "POST", U+"/air/revocations", ta, `{"revocations":true}`, "If-Match", `"v1"`); r.status != 200 ||
		r.json(t)["revocations"] != true || r.header.Get("ETag") != `"v2"` {
		t.Fatalf("on: %d %s %v", r.status, r.body, r.header)
	}
	if r := m.call(t, "POST", U+"/air/revocations", ta, `{"revocations":true}`, "If-Match", "*"); r.header.Get("ETag") != `"v2"` {
		t.Errorf("on again moved the version: %v", r.header)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/events?kind=upstream.revocations", ta, ""); !strings.Contains(string(r.body), `"pending":0`) {
		t.Errorf("the event: %s", r.body)
	}
	if r := m.call(t, "DELETE", U+"/air/revocations/b1", ta, ""); r.status != 428 {
		t.Errorf("a lift without If-Match: %d", r.status)
	}
	if r := m.call(t, "DELETE", U+"/air/revocations/b1", ta, "", "If-Match", "*"); r.status != 404 {
		t.Errorf("a lift of nothing stored: %d %s", r.status, r.body)
	}

	// stored and applied (as a bundle's import stores and applies one): listed, lifted once (200),
	// then nothing is left to lift (409); one never applied is not lifted either
	air, err := m.ups.Get(ctx, admin, "acme", "air")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"b1", "b2"} {
		if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.PutUpstreamRevocation(ctx, store.UpstreamRevocation{UpstreamID: air.ID, BuildID: b, TenantID: air.TenantID,
				BodyHash: strings.Repeat("c", 64), Name: "x", PackageVersion: "1.0", Platform: "linux_amd64", State: store.RevokeYanked}); err != nil {
				return err
			}
			if b == "b2" {
				return nil
			}
			return tx.MarkRevocationApplied(ctx, air.ID, b, store.RevokeYanked)
		}); err != nil {
			t.Fatal(err)
		}
	}
	r = m.call(t, "GET", U+"/air/revocations", ta, "")
	if body := string(r.body); !strings.Contains(body, `"status":"applied"`) || !strings.Contains(body, `"status":"waiting"`) {
		t.Fatalf("stored: %s", body)
	}
	listed := r.header.Get("ETag")
	if r := m.call(t, "DELETE", U+"/air/revocations/b1", ta, "", "If-Match", "*"); r.status != 200 || r.json(t)["restored"] != float64(0) {
		t.Fatalf("a lift: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", U+"/air/revocations", ta, ""); !strings.Contains(string(r.body), `"status":"lifted"`) || r.header.Get("ETag") == listed {
		t.Fatalf("lifted: %s %v", r.body, r.header)
	}
	for _, b := range []string{"b1", "b2"} {
		if r := m.call(t, "DELETE", U+"/air/revocations/"+b, ta, "", "If-Match", "*"); r.status != 409 {
			t.Errorf("%s: nothing to lift: %d %s", b, r.status, r.body)
		}
	}
}

// Spec 0039 PR 2: a mirror with "*" answers the number of names it discovered, and GET …/names lists
// them (tenant administrators, a name prefix); an upstream without "*" answers neither; "*" on a
// mirror of DuckDB's repositories is refused.
func TestUpstreamNames(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const U = "/api/v1/tenants/acme/upstreams"
	if r := m.call(t, "POST", U, ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"json"}]}`); r.status != 201 {
		t.Fatalf("core: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", U+"/core", ta, ""); r.status != 200 || r.json(t)["discovered"] != nil {
		t.Fatalf("no *: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", U+"/core/names", ta, ""); r.status != 200 || len(r.json(t)["names"].([]any)) != 0 {
		t.Fatalf("no * names: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U+"/core/extensions", ta, `{"name":"*"}`); r.status != 400 {
		t.Errorf("* on a mirror of DuckDB: %d %s", r.status, r.body)
	}
	// as a run records them: "*" and the names it discovered
	u, err := m.ups.Get(ctx, admin, "acme", "core")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.PutUpstreamEntry(ctx, u.ID, store.UpstreamEntry{Name: "*"}); err != nil {
			return err
		}
		cur, err := tx.GetUpstreamByID(ctx, u.ID)
		if err != nil {
			return err
		}
		_, err = tx.SetUpstreamNames(ctx, cur, []string{"geo", "tresor"}, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "GET", U+"/core", ta, ""); r.json(t)["discovered"] != float64(2) {
		t.Fatalf("discovered: %s", r.body)
	}
	if r := m.call(t, "GET", U+"/core/names?prefix=t", ta, ""); r.status != 200 || !strings.Contains(string(r.body), `"names":["tresor"]`) {
		t.Fatalf("names: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", U+"/core/names", m.toks["channel adm"], ""); r.status != 404 {
		t.Errorf("a channel administrator reads them: %d", r.status)
	}
}

// Spec 0009 phase 2: a miss on a DuckDB route offers the cell to a pull-through upstream only for a
// caller holding install, on the flat path; the answer is the same.
func TestPullThroughOffers(t *testing.T) {
	m := newMgmt(t)
	if _, err := m.ups.Add(ctx, admin, "acme", upstream.Spec{Name: "pull", Kind: store.UpstreamCommunity, Channel: "prod",
		Mode: store.ModePullThrough, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "*"}}}); err != nil {
		t.Fatal(err)
	}
	const flat = "/acme/prod/v2.0.0/linux_amd64/newext.duckdb_extension.gz"
	for who, want := range map[string]int{"anonymous": 401, "no grants": 404, "install": 404, "channel adm": 404,
		"server admin": 401} {
		if r := m.call(t, "GET", flat, m.toks[who], ""); r.status != want {
			t.Errorf("%s: %d", who, r.status)
		}
		offers := m.pulls.take()
		if (who == "install" || who == "channel adm") != (len(offers) == 1) { // admin holds install
			t.Errorf("%s offered %v", who, offers)
		}
		if who == "install" && (offers[0].Name != "newext" || offers[0].DuckDBVersion != "v2.0.0" || offers[0].Platform != "linux_amd64") {
			t.Errorf("the offer: %+v", offers[0])
		}
	}
	// a pull-through upstream is not run
	if r := m.call(t, "POST", "/api/v1/tenants/acme/upstreams/pull/sync", m.toks["tenant admin"], ""); r.status != 409 {
		t.Errorf("sync a pull-through upstream: %d", r.status)
	}
	// nor is a bundle-mode one (spec 0014)
	if _, err := m.ups.Add(ctx, admin, "acme", upstream.Spec{Name: "air", Kind: store.UpstreamCommunity, Channel: "prod",
		Mode: store.ModeBundle, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "x"}}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?dry_run=true"} {
		if r := m.call(t, "POST", "/api/v1/tenants/acme/upstreams/air/sync"+q, m.toks["tenant admin"], ""); r.status != 409 {
			t.Errorf("sync%s a bundle-mode upstream: %d", q, r.status)
		}
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/upstreams/air/credential", m.toks["tenant admin"], `{"credential":"any"}`,
		"If-Match", "*"); r.status != 400 {
		t.Errorf("a credential on a bundle-mode upstream: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/upstreams", m.toks["tenant admin"], `{"name":"air2","kind":"repository",`+
		`"prefix":"https://air.example.org/a/b","keys":["sha256:`+strings.Repeat("1", 64)+`"],"channel":"prod","mode":"bundle",`+
		`"platforms":["linux_amd64"],"extensions":[{"name":"x"}],"credential":"any"}`); r.status != 400 {
		t.Errorf("a bundle-mode upstream created with a credential: %d %s", r.status, r.body)
	}
	// a core name, an alias, the versioned path, an existing release, a platform it does not fetch: no offer
	for _, path := range []string{"/acme/prod/v2.0.0/linux_amd64/json.duckdb_extension.gz",
		"/acme/prod/v2.0.0/linux_amd64/postgres.duckdb_extension.gz",
		"/acme/prod/newext/1.0/v2.0.0/linux_amd64/newext.duckdb_extension.gz",
		"/acme/prod/v2.0.0/linux_amd64/tresor.duckdb_extension.gz",
		"/acme/prod/v2.0.0/osx_arm64/newext.duckdb_extension.gz"} {
		m.call(t, "GET", path, m.toks["install"], "")
		if offers := m.pulls.take(); len(offers) != 0 {
			t.Errorf("%s offered %v", path, offers)
		}
	}
}

// Spec 0009 phase 3: credentials are listed to server administrators by name, never their
// settings; an upstream takes one by name.
func TestUpstreamCredentials(t *testing.T) {
	m := newMgmt(t)
	creds, err := credential.New([]config.Credential{{Name: "sub", Tenants: []string{"acme"}, Prefixes: []string{"https://repo.example/acme/"},
		Kind: "token_file", TokenFile: "/run/secrets/token"}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	m.ups.Credentials = creds
	r := m.call(t, "GET", "/api/v1/credentials", m.toks["server admin"], "")
	if r.status != 200 || !strings.Contains(string(r.body), `"name":"sub"`) || strings.Contains(string(r.body), "/run/secrets") {
		t.Fatalf("credentials: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/credentials", m.toks["tenant admin"], ""); r.status != 404 {
		t.Fatalf("a tenant administrator lists credentials: %d", r.status)
	}
	const U = "/api/v1/tenants/acme/upstreams"
	ta := m.toks["tenant admin"]
	if r := m.call(t, "POST", U, ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"credential":"sub"}`); r.status != 400 {
		t.Fatalf("a credential on DuckDB's core: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", U, ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"]}`); r.status != 201 {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	for body, want := range map[string]int{`{"credential":"sub"}`: 400, `{}`: 400, `{"credential":""}`: 200} {
		if r := m.call(t, "POST", U+"/core/credential", ta, body, "If-Match", "*"); r.status != want {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}
}

// A pinned key is removed by its fingerprint in the path, the colon as is (spec 0015 phase 2b: the
// console sends sha256:… unescaped, since the API refuses escaped paths).
func TestUpstreamKeyPath(t *testing.T) {
	m := newMgmt(t)
	acme, _ := m.st.GetTenant(ctx, "acme")
	ch, err := m.st.GetChannel(ctx, "acme", "staging")
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		u := store.Upstream{TenantID: acme.ID, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://repo.example/acme/", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Private, State: store.UpstreamActive, CreatedBy: "os:1:t"}
		if err := tx.InsertUpstream(ctx, &u); err != nil {
			return err
		}
		for _, k := range []string{k1, k2} {
			if err := tx.AddUpstreamKey(ctx, u.ID, k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const U = "/api/v1/tenants/acme/upstreams/repo/keys/"
	ta := m.toks["tenant admin"]
	if r := m.call(t, "DELETE", U+k2, ta, ""); r.status != 204 {
		t.Fatalf("remove a pinned key: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", U+k1, ta, ""); r.status != 400 {
		t.Errorf("the last pinned key: %d %s", r.status, r.body)
	}
}

// Spec 0012 phase 1c: an upstream's usage switch, its last forwarding run, and the credentials that
// carry usage.
func TestUpstreamUsage(t *testing.T) {
	m := newMgmt(t)
	creds, err := credential.New([]config.Credential{{Name: "sub", Tenants: []string{"acme"}, Prefixes: []string{"https://repo.example/acme/"},
		Kind: "token_file", TokenFile: "/run/secrets/token"}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	m.ups.Credentials = creds
	acme, _ := m.st.GetTenant(ctx, "acme")
	ch, _ := m.st.GetChannel(ctx, "acme", "staging")
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		u := store.Upstream{TenantID: acme.ID, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://repo.example/acme/", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Private, State: store.UpstreamActive, CreatedBy: "os:1:t", Credential: "sub"}
		return tx.InsertUpstream(ctx, &u)
	}); err != nil {
		t.Fatal(err)
	}
	const U = "/api/v1/tenants/acme/upstreams/repo"
	ta := m.toks["tenant admin"]
	if r := m.call(t, "GET", U, ta, "").json(t); r["usage"] != "forward" {
		t.Fatalf("usage defaults to forward: %v", r)
	}
	forwards := func() string {
		r := m.call(t, "GET", "/api/v1/credentials", m.toks["server admin"], "")
		var out struct {
			Credentials []struct {
				Forwards []map[string]string `json:"forwards_usage"`
			}
		}
		if err := json.Unmarshal(r.body, &out); err != nil || len(out.Credentials) != 1 {
			t.Fatalf("credentials: %s %v", r.body, err)
		}
		b, _ := json.Marshal(out.Credentials[0].Forwards)
		return string(b)
	}
	if f := forwards(); f != `[{"tenant":"acme","upstream":"repo"}]` {
		t.Fatalf("forwards_usage: %s", f)
	}
	if r := m.call(t, "GET", U+"/usage", ta, "").json(t); r["usage"] != "forward" || r["result"] != nil {
		t.Fatalf("no run yet: %v", r)
	}
	if r := m.call(t, "POST", U+"/usage", ta, `{"usage":"off"}`); r.status != 428 {
		t.Fatalf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/usage", ta, `{"usage":"sometimes"}`, "If-Match", "*"); r.status != 400 {
		t.Fatalf("a bad value: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/usage", ta, `{"usage":"off"}`, "If-Match", "*"); r.status != 200 || r.json(t)["usage"] != "off" {
		t.Fatalf("off: %d %s", r.status, r.body)
	}
	if f := forwards(); f != `[]` {
		t.Fatalf("forwards_usage while off: %s", f)
	}
	evs, _ := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "upstream.change", Limit: 1})
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"change":"usage"`) || !strings.Contains(evs[0].Data, `"value":"off"`) {
		t.Fatalf("upstream.change: %+v", evs)
	}
	if r := m.call(t, "POST", U+"/usage", m.toks["install"], `{"usage":"forward"}`, "If-Match", "*"); r.status != 404 {
		t.Fatalf("an installer: %d", r.status)
	}
}

// Spec 0032: a mirror's fetch mode, an eager entry, and a lazy upstream's not_asked cells.
func TestUpstreamFetch(t *testing.T) {
	m := newMgmt(t)
	acme, _ := m.st.GetTenant(ctx, "acme")
	ch, _ := m.st.GetChannel(ctx, "acme", "staging")
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		u := store.Upstream{TenantID: acme.ID, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://repo.example/acme/", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Private, State: store.UpstreamActive, CreatedBy: "os:1:t",
			Platforms: []string{"linux_amd64", "osx_arm64"}, Entries: []store.UpstreamEntry{{Name: "acl"}}}
		return tx.InsertUpstream(ctx, &u)
	}); err != nil {
		t.Fatal(err)
	}
	const U = "/api/v1/tenants/acme/upstreams/repo"
	ta := m.toks["tenant admin"]
	if r := m.call(t, "GET", U, ta, "").json(t); r["fetch"] != "eager" {
		t.Fatalf("fetch defaults to eager: %v", r)
	}
	if r := m.call(t, "POST", U+"/fetch", ta, `{"fetch":"lazy"}`); r.status != 428 {
		t.Fatalf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/fetch", ta, `{"fetch":"sometimes"}`, "If-Match", "*"); r.status != 400 {
		t.Fatalf("a bad value: %d", r.status)
	}
	if r := m.call(t, "POST", U+"/fetch", ta, `{"fetch":"lazy"}`, "If-Match", "*"); r.status != 200 || r.json(t)["fetch"] != "lazy" {
		t.Fatalf("lazy: %d %s", r.status, r.body)
	}
	evs, _ := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "upstream.change", Limit: 1})
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"change":"fetch"`) || !strings.Contains(evs[0].Data, `"value":"lazy"`) {
		t.Fatalf("upstream.change: %+v", evs)
	}
	if r := m.call(t, "POST", U+"/extensions", ta, `{"name":"tresor","versions":[],"eager":true}`); r.status != 201 || r.json(t)["eager"] != true {
		t.Fatalf("an eager entry: %d %s", r.status, r.body)
	}
	var got struct {
		Extensions []struct {
			Name  string `json:"name"`
			Eager bool   `json:"eager"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(m.call(t, "GET", U, ta, "").body, &got); err != nil || len(got.Extensions) != 2 || got.Extensions[0].Eager ||
		!got.Extensions[1].Eager {
		t.Fatalf("entries: %+v %v", got, err)
	}
	// not_asked: acl's cells (tresor is eager), on both platforms and every DuckDB version
	versions, _ := m.st.ChannelVersions(ctx, ch.ID)
	if len(versions) == 0 {
		t.Fatal("staging serves no DuckDB version")
	}
	var cells struct {
		Cells []map[string]any `json:"cells"`
	}
	if err := json.Unmarshal(m.call(t, "GET", U+"/cells?outcome=not_asked", ta, "").body, &cells); err != nil || len(cells.Cells) != 2*len(versions) {
		t.Fatalf("not_asked: %+v %v", cells, err)
	}
	for _, c := range cells.Cells {
		if c["name"] != "acl" || c["outcome"] != "not_asked" {
			t.Fatalf("a not_asked cell: %v", c)
		}
	}
	if r := m.call(t, "POST", U+"/fetch", m.toks["install"], `{"fetch":"eager"}`, "If-Match", "*"); r.status != 404 {
		t.Fatalf("an installer: %d", r.status)
	}
	// added lazy; a pull-through takes no fetch mode and shows none
	const L = "/api/v1/tenants/acme/upstreams"
	if r := m.call(t, "POST", L, ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],
		"extensions":[{"name":"json","eager":true}],"fetch":"lazy"}`); r.status != 201 || r.json(t)["fetch"] != "lazy" {
		t.Fatalf("a lazy mirror: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", L, ta, `{"name":"pt","kind":"duckdb-community","channel":"staging","platforms":["linux_amd64"],
		"extensions":[{"name":"h3"}],"mode":"pull-through","fetch":"lazy"}`); r.status != 400 {
		t.Fatalf("a lazy pull-through: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", L, ta, `{"name":"pt","kind":"duckdb-community","channel":"staging","platforms":["linux_amd64"],
		"extensions":[{"name":"h3"}],"mode":"pull-through"}`); r.status != 201 || r.json(t)["fetch"] != nil {
		t.Fatalf("a pull-through: %d %s", r.status, r.body)
	}
}

// Spec 0032: a granted caller's miss of a lazy mirror's cell asks for it: on the plain path it is
// answered 503 with Retry-After while the build is coming, and offered; the versioned path asks only;
// callers without the grant ask nothing and get today's answers.
func TestLazyFirstRequest(t *testing.T) {
	m := newMgmt(t)
	if _, err := m.ups.Add(ctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "prod", FetchMode: store.FetchLazy,
		Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "spatial"}}}); err != nil {
		t.Fatal(err)
	}
	const flat = "/acme/prod/v2.0.0/linux_amd64/spatial.duckdb_extension.gz"
	m.pulls.coming = true
	r := m.call(t, "GET", flat, m.toks["install"], "")
	if r.status != 503 || r.header.Get("Retry-After") != "5" {
		t.Fatalf("a first request: %d %v", r.status, r.header)
	}
	offers := m.pulls.take()
	if len(offers) != 1 || !offers[0].Lazy || offers[0].Name != "spatial" || offers[0].Principal == "" || len(m.pulls.asks) != 1 || m.pulls.n503 != 1 {
		t.Fatalf("offers %+v, asks %+v", offers, m.pulls.asks)
	}
	m.pulls.coming = false
	if r := m.call(t, "GET", flat, m.toks["install"], ""); r.status != 404 {
		t.Fatalf("a cell fetched and not found: %d", r.status)
	}
	m.pulls.take()
	m.pulls.coming = true
	if r := m.call(t, "GET", "/acme/prod/spatial/1.0/v2.0.0/linux_amd64/spatial.duckdb_extension.gz", m.toks["install"], ""); r.status != 404 {
		t.Fatalf("the versioned path: %d", r.status)
	}
	if offers := m.pulls.take(); len(offers) != 0 || len(m.pulls.asks) != 3 {
		t.Fatalf("the versioned path offered %+v, asks %d", offers, len(m.pulls.asks))
	}
	for who, want := range map[string]int{"anonymous": 401, "no grants": 404} {
		if r := m.call(t, "GET", flat, m.toks[who], ""); r.status != want {
			t.Errorf("%s: %d", who, r.status)
		}
	}
	if offers := m.pulls.take(); len(offers) != 0 || len(m.pulls.asks) != 3 {
		t.Fatalf("callers without the grant offered %+v, asks %d", offers, len(m.pulls.asks))
	}
	// a name the lazy mirror does not list: nothing asked
	m.call(t, "GET", "/acme/prod/v2.0.0/linux_amd64/h3.duckdb_extension.gz", m.toks["install"], "")
	if len(m.pulls.asks) != 3 {
		t.Fatal("an unlisted name was asked")
	}
}
