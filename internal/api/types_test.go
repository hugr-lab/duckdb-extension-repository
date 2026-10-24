package api_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0034: package types on the API. ?type= and package_type default to duckdb_extension and are
// left out of answers for it; a type this server does not know is refused everywhere (400), and so is
// ?type= on a route that takes none; on a channel's routes the type is the channel's (another one
// 400; one this server does not know: no channel, 404); a promotion or a placement across types is
// refused (409).
func TestPackageTypeRoutes(t *testing.T) {
	m := newMgmt(t)
	adm, root := m.toks["tenant admin"], m.toks["server admin"]

	// the default: answers as before, and naming it changes nothing
	for _, path := range []string{"/api/v1/tenants/acme/grants", "/api/v1/tenants/acme/catalogue", "/api/v1/tenants/acme/licenses",
		"/api/v1/tenants/acme/reserved-names", "/api/v1/tenants/acme/extension-groups", "/api/v1/tenants/acme/stats/downloads"} {
		plain := m.call(t, "GET", path, adm, "")
		if plain.status != 200 || bytes.Contains(plain.body, []byte("package_type")) {
			t.Fatalf("%s: %d %s", path, plain.status, plain.body)
		}
		if named := m.call(t, "GET", path+"?type="+pkgtype.DuckDBExtension, adm, ""); named.status != 200 || !bytes.Equal(named.body, plain.body) {
			t.Fatalf("%s?type=%s: %d %s", path, pkgtype.DuckDBExtension, named.status, named.body)
		}
		// an unknown type, or "*", is refused before anything else
		for _, bad := range []string{"nope", "*", "duckdb_extension&type=duckdb_extension"} {
			if r := m.call(t, "GET", path+"?type="+bad, "", ""); r.status != 400 {
				t.Fatalf("%s?type=%s: %d %s", path, bad, r.status, r.body)
			}
		}
	}

	// a route that takes no ?type= refuses one (a body names its package_type), known or not
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tenants/acme/channels", ""},
		{"POST", "/api/v1/tenants/acme/channels", `{"name":"x","kind":"signed"}`},
		{"POST", "/api/v1/tenants/acme/grants", `{"principal":"subject:corp|dan","verbs":["install"]}`},
		{"POST", "/api/v1/tenants/acme/extension-groups/platform/members", `{"add":["snowflake"]}`},
		{"GET", "/api/v1/tenants/acme/issuers", ""},
		{"GET", "/api/v1/tenants/acme/whoami", ""},
	} {
		if r := m.call(t, c.method, c.path+"?type="+pkgtype.DuckDBExtension, adm, c.body); r.status != 400 || !strings.Contains(string(r.body), "no type") {
			t.Fatalf("%s %s?type=: %d %s", c.method, c.path, r.status, r.body)
		}
	}

	// a second type, as a later spec registers one
	unregister := pkgtype.RegisterForTest("test_package")
	defer func() { unregister() }()
	r := m.call(t, "POST", "/api/v1/tenants/acme/channels", adm, `{"name":"drivers","kind":"signed","package_type":"test_package"}`)
	if r.status != 201 || r.json(t)["package_type"] != "test_package" {
		t.Fatalf("a channel of another type: %d %s", r.status, r.body)
	}
	for body, want := range map[string]int{
		`{"name":"x","kind":"signed","package_type":"nope"}`:              400,
		`{"name":"x","kind":"passthrough","package_type":"test_package"}`: 400,
	} {
		if r := m.call(t, "POST", "/api/v1/tenants/acme/channels", adm, body); r.status != want {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/channels", "", ""); r.status != 200 ||
		!strings.Contains(string(r.body), `"name":"drivers","kind":"signed","package_type":"test_package"`) ||
		strings.Count(string(r.body), "package_type") != 1 {
		t.Fatalf("the channels: %d %s", r.status, r.body)
	}
	// on a channel's routes the type is the channel's
	for path, want := range map[string]int{
		"/api/v1/tenants/acme/channels/drivers/sources?type=test_package":     200,
		"/api/v1/tenants/acme/channels/drivers/sources":                       200,
		"/api/v1/tenants/acme/channels/drivers/sources?type=duckdb_extension": 400,
		"/api/v1/tenants/acme/channels/prod/sources?type=test_package":        400,
	} {
		if r := m.call(t, "GET", path, adm, ""); r.status != want {
			t.Errorf("%s: %d %s", path, r.status, r.body)
		}
	}

	// grants: a name names its type, never "*"; a typed grant without a name shows its type
	for body, want := range map[string]int{
		`{"principal":"subject:corp|dan","verbs":["install"],"extension":"snowflake","package_type":"*"}`:     400,
		`{"principal":"subject:corp|dan","verbs":["install"],"package_type":"nope"}`:                          400,
		`{"principal":"subject:corp|dan","verbs":["install"],"channel":"prod","package_type":"test_package"}`: 400,
		`{"principal":"subject:corp|dan","verbs":["admin"],"package_type":"test_package"}`:                    201,
		`{"principal":"subject:corp|dan","verbs":["install"],"extension":"snowflake"}`:                        201,
		// audit is the tenant's log: of every type only
		`{"principal":"subject:corp|dan","verbs":["audit"],"package_type":"test_package"}`:     400,
		`{"principal":"subject:corp|dan","verbs":["audit"],"package_type":"duckdb_extension"}`: 400,
	} {
		r := m.call(t, "POST", "/api/v1/tenants/acme/grants", adm, body)
		if r.status != want {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
		if want == 201 && strings.Contains(body, "package_type") != strings.Contains(string(r.body), `"package_type":"test_package"`) {
			t.Fatalf("%s: %s", body, r.body)
		}
	}
	dan := m.keys.sign(t, idpURL, map[string]any{"sub": "dan", "aud": publicURL + "/acme"})
	// dan administers the other type's channel, not DuckDB's, and nothing tenant-level
	for path, want := range map[string]int{
		"/api/v1/tenants/acme/channels/drivers/sources":   200,
		"/api/v1/tenants/acme/channels/prod/sources":      404,
		"/api/v1/tenants/acme/grants":                     404,
		"/api/v1/tenants/acme/licenses?type=test_package": 404,
	} {
		if r := m.call(t, "GET", path, dan, ""); r.status != want {
			t.Errorf("dan on %s: %d %s", path, r.status, r.body)
		}
	}

	// a grant on a channel of the channel's type is "*" there: answered so, and the same grant
	r = m.call(t, "POST", "/api/v1/tenants/acme/grants", adm, `{"principal":"subject:corp|eve","verbs":["install"],"channel":"drivers","package_type":"test_package"}`)
	if r.status != 201 || strings.Contains(string(r.body), "package_type") {
		t.Fatalf("a channel's grant of its type: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/grants", adm, `{"principal":"subject:corp|eve","verbs":["install"],"channel":"drivers"}`); r.status != 409 {
		t.Fatalf("the same grant of every type: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/grants", adm, `{"principal":"subject:corp|eve","verbs":["install"],"channel":"prod"}`); r.status != 201 {
		t.Fatalf("a grant on prod: %d %s", r.status, r.body)
	}
	// a channel's grant reaches its channel's type only
	r = m.call(t, "GET", "/api/v1/tenants/acme/grants?principal=subject:corp|eve&extension=snowflake&type=test_package", adm, "")
	if gs := r.json(t)["grants"].([]any); r.status != 200 || len(gs) != 1 || gs[0].(map[string]any)["channel"] != "drivers" {
		t.Fatalf("eve's grants reaching the other type's snowflake: %d %s", r.status, r.body)
	}
	r = m.call(t, "GET", "/api/v1/tenants/acme/grants?principal=subject:corp|eve&extension=snowflake", adm, "")
	if gs := r.json(t)["grants"].([]any); r.status != 200 || len(gs) != 1 || gs[0].(map[string]any)["channel"] != "prod" {
		t.Fatalf("eve's grants reaching DuckDB's snowflake: %d %s", r.status, r.body)
	}

	// statistics are of one type: a channel of another is refused
	for path, want := range map[string]int{
		"/api/v1/tenants/acme/stats/downloads?channel=drivers&type=test_package": 200,
		"/api/v1/tenants/acme/stats/downloads?channel=drivers":                   400,
		"/api/v1/tenants/acme/stats/downloads?channel=prod&type=test_package":    400,
		"/api/v1/tenants/acme/stats/downloads?channel=prod":                      200,
	} {
		if r := m.call(t, "GET", path, adm, ""); r.status != want {
			t.Errorf("%s: %d %s", path, r.status, r.body)
		}
	}

	// a group's removal lists its members one type per event
	if r := m.call(t, "POST", "/api/v1/tenants/acme/extension-groups", adm, `{"name":"mixed"}`); r.status != 201 {
		t.Fatalf("a group: %d %s", r.status, r.body)
	}
	for _, body := range []string{`{"add":["snowflake","bigquery"]}`, `{"add":["snowflake"],"package_type":"test_package"}`} {
		if r := m.call(t, "POST", "/api/v1/tenants/acme/extension-groups/mixed/members", adm, body); r.status != 200 {
			t.Fatalf("members %s: %d %s", body, r.status, r.body)
		}
	}
	if r := m.call(t, "DELETE", "/api/v1/tenants/acme/extension-groups/mixed", adm, "", "If-Match", "*"); r.status != 204 {
		t.Fatalf("the group's removal: %d %s", r.status, r.body)
	}
	acme, _ := m.st.GetTenant(ctx, "acme")
	evs, err := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "extension_group.members", Subject: "extension-group:mixed", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, e := range evs {
		if strings.Contains(e.Data, `"added":[]`) {
			removed = append(removed, e.Data)
		}
	}
	slices.Sort(removed)
	if want := []string{`{"added":[],"package_type":"test_package","removed":["snowflake"]}`, `{"added":[],"removed":["bigquery","snowflake"]}`}; !slices.Equal(removed, want) {
		t.Fatalf("the removal's member events: %q", removed)
	}

	// a promotion and a placement across types; an intake of a type this server reads nothing of
	if r := m.call(t, "POST", "/api/v1/tenants/acme/channels/drivers/extensions/tresor/releases/promote", root,
		`{"from_channel":"prod","version":"1.0"}`); r.status != 409 {
		t.Fatalf("a promotion across types: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/channels/prod/extensions/tresor/releases/place", root,
		`{"source":"tenant","version":"1.0","package_type":"test_package"}`); r.status != 409 {
		t.Fatalf("a placement across types: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/channels/prod/extensions/tresor/releases/place", root,
		`{"source":"tenant","version":"1.0","package_type":"nope"}`); r.status != 400 {
		t.Fatalf("a placement of an unknown type: %d %s", r.status, r.body)
	}
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	if r := m.upload(t, "/api/v1/tenants/acme/catalogue/extensions/newext/builds?version=1.0&platform=linux_amd64&type=test_package",
		root, file); r.status != 409 {
		t.Fatalf("a publication of another type: %d %s", r.status, r.body)
	}
	// the other type's catalogue and licences are its own
	if r := m.call(t, "GET", "/api/v1/tenants/acme/catalogue?type=test_package", adm, ""); r.status != 200 ||
		string(r.body) != `{"entries":[]}`+"\n" && string(r.body) != `{"entries":[]}` {
		t.Fatalf("the other type's catalogue: %d %s", r.status, r.body)
	}

	// a downgrade: a channel of a type this server does not know answers as a missing one, its type
	// unsaid, even to a server administrator
	unregister()
	unregister = func() {}
	for _, path := range []string{"/api/v1/tenants/acme/channels/drivers/sources?type=duckdb_extension", "/api/v1/tenants/acme/channels/nope/sources?type=duckdb_extension"} {
		if r := m.call(t, "GET", path, root, ""); r.status != 404 || strings.Contains(string(r.body), "test_package") {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/stats/downloads?channel=drivers", root, ""); r.status != 404 {
		t.Fatalf("the statistics of an unknown type's channel: %d %s", r.status, r.body)
	}
}
