package api_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Spec 0034: package types on the API. ?type= and package_type default to duckdb_extension and are
// left out of answers for it; a type this server does not know is refused everywhere (400); on a
// channel's routes the type is the channel's (another one 400); a promotion or a placement across
// types is refused (409).
func TestPackageTypeRoutes(t *testing.T) {
	m := newMgmt(t)
	adm, root := m.toks["tenant admin"], m.toks["server admin"]

	// the default: answers as before, and naming it changes nothing
	for _, path := range []string{"/api/v1/tenants/acme/grants", "/api/v1/tenants/acme/catalogue", "/api/v1/tenants/acme/channels",
		"/api/v1/tenants/acme/licenses", "/api/v1/tenants/acme/reserved-names", "/api/v1/tenants/acme/extension-groups"} {
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

	// a second type, as a later spec registers one
	defer pkgtype.RegisterForTest("test_package")()
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
}
