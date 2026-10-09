package api_test

import (
	"strings"
	"testing"

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
		"the same name":       {core, 409},
		"an alias":            {`{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"extensions":[{"name":"postgres"}]}`, 400},
		"a name on two":       {`{"name":"x","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`, 409},
		"a replacement holds": {`{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"extensions":[{"name":"acl"}]}`, 409},
		"no channel":          {`{"name":"x","kind":"duckdb-core","channel":"nope","platforms":["linux_amd64"]}`, 400},
		"a kind":              {`{"name":"x","kind":"enterest","channel":"prod","platforms":["linux_amd64"]}`, 400},
		"an unknown field":    {`{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"bogus":1}`, 400},
	} {
		if r := m.call(t, "POST", U, ta, tc.body); r.status != tc.want {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// the collision names the names
	if r := m.call(t, "POST", U, ta, `{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"],"extensions":[{"name":"acl"}]}`); !strings.Contains(string(r.body), "acl") {
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
