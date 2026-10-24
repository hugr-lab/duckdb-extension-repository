package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Spec 0033 phase B part 1: a distribution admin (or a server administrator) marks a distributor's
// publisher, sets its hold, manages its keys, purges and withdraws its versions, and reads its
// entries without channels; the publisher publishes into the catalogue only, without grants, any
// name not reserved server-wide nor in the tenant for another holder; a tenant administrator does
// none of the distributor's acts. A withdrawn version is served nowhere, shown withdrawn, never made
// current or placed again, never purged, and its body never arrives again.
func TestDistributorRoutes(t *testing.T) {
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
	m := newMgmt(t)
	da, ta, sa := m.toks["server dist admin"], m.toks["tenant admin"], m.toks["server admin"]
	const T = "/api/v1/tenants/acme"
	call := func(want int, method, path, tok, body string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}

	// the mark: a distribution admin's or a server administrator's, never a tenant administrator's
	call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci"}`)
	call(404, "PUT", T+"/publishers/enterest-ci/distributor", ta, `{"distributor":"enterest"}`)
	call(400, "PUT", T+"/publishers/enterest-ci/distributor", da, `{"distributor":""}`)
	if r := call(200, "PUT", T+"/publishers/enterest-ci/distributor", da, `{"distributor":"enterest"}`); r.json(t)["distributor"] != "enterest" {
		t.Fatalf("marked: %s", r.body)
	}
	if r := call(200, "GET", T+"/publishers/enterest-ci", ta, ""); r.json(t)["distributor"] != "enterest" {
		t.Fatalf("the tenant administrator reads the mark: %s", r.body)
	}
	// a publisher with grants is not marked; a distributor's publisher takes no grant
	call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`)
	call(201, "POST", T+"/grants", ta, `{"principal":"publisher:own-ci","verbs":["publish"]}`)
	call(400, "PUT", T+"/publishers/own-ci/distributor", sa, `{"distributor":"enterest"}`)
	call(404, "POST", T+"/grants", ta, `{"principal":"publisher:enterest-ci","verbs":["publish"]}`) // marked: not the tenant's
	call(400, "POST", T+"/grants", sa, `{"principal":"publisher:enterest-ci","verbs":["publish"],"extension":"geo_tools"}`)
	// its keys and its hold are the distributor's
	call(404, "POST", T+"/publishers/enterest-ci/keys", ta, `{"public_key":"`+newSigner(t).authKey+`"}`)
	key := m.addKey(t, da, "acme", "enterest-ci")
	call(200, "POST", T+"/publishers/enterest-ci/license-keys", da, "") // its keys are the distributor's already
	call(404, "PUT", T+"/publishers/enterest-ci/hold", ta, `{"hold":"1h"}`)
	call(404, "PUT", T+"/publishers/own-ci/hold", da, `{"hold":"1h"}`)
	if r := call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"1h"}`); r.json(t)["hold"] != "1h0m0s" {
		t.Fatalf("its hold: %s", r.body)
	}
	call(200, "PUT", T+"/publishers/enterest-ci/hold", sa, `{"hold":"0s"}`)

	// it publishes into the catalogue only: any name not reserved server-wide nor for another holder
	q := "?version=1.0&platform=linux_amd64"
	geo := built(t, "1.0", "geo_tools_duckdb_cpp_init")
	if r := m.upload(t, T+"/channels/prod/extensions/geo_tools/releases"+q, key, geo); r.status != 404 {
		t.Fatalf("a distributor's publisher on a channel's route: %d %s", r.status, r.body)
	}
	r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds"+q, key, geo)
	if r.status != 201 || r.json(t)["state"] != "available" || strings.Contains(string(r.body), `"placed"`) {
		t.Fatalf("into the catalogue: %d %s", r.status, r.body)
	}
	build := r.json(t)["id"].(string)
	call(200, "PUT", T+"/reserved-names/acme_other", da, `{"holder":"someone-else"}`)
	call(200, "PUT", T+"/reserved-names/acme_mine", da, `{"holder":"enterest"}`)
	if r := m.upload(t, T+"/catalogue/extensions/acme_other/builds"+q, key, built(t, "1.0", "acme_other_duckdb_cpp_init")); r.status != 404 {
		t.Fatalf("a name reserved for another holder: %d %s", r.status, r.body)
	}
	if r := m.upload(t, T+"/catalogue/extensions/acme_mine/builds"+q, key, built(t, "1.0", "acme_mine_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("a name reserved for its distributor: %d %s", r.status, r.body)
	}
	if r := m.upload(t, T+"/catalogue/extensions/json/builds"+q, key, built(t, "1.0", "json_duckdb_cpp_init")); r.status != 404 {
		t.Fatalf("a core name: %d %s", r.status, r.body)
	}

	// a distribution admin reads its distributors' entries, without channels; the tenant's own are not its
	if r := call(200, "GET", T+"/catalogue", da, ""); !strings.Contains(string(r.body), `"source":"publisher:enterest-ci"`) ||
		strings.Contains(string(r.body), `"source":"tenant"`) {
		t.Fatalf("the distribution admin's catalogue: %s", r.body)
	}
	call(404, "GET", T+"/catalogue/tenant/extensions/tresor", da, "")
	call(404, "GET", T+"/catalogue/publisher:own-ci/extensions/geo_tools", da, "")

	// placed into prod by a server administrator; served to a holder of install
	call(201, "POST", T+"/channels/prod/extensions/geo_tools/releases/place", sa, `{"source":"publisher:enterest-ci","version":"1.0"}`)
	const geoPath = "/acme/prod/geo_tools/1.0/v2.0.0/linux_amd64/geo_tools.duckdb_extension.gz"
	if r := m.call(t, "GET", geoPath, m.toks["install"], ""); r.status != 200 {
		t.Fatalf("served before the withdrawal: %d", r.status)
	}
	if r := call(200, "GET", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools", da, ""); strings.Contains(string(r.body), `"placed"`) ||
		strings.Contains(string(r.body), "prod") {
		t.Fatalf("the distribution admin learns nothing of the channels: %s", r.body)
	}
	if r := call(200, "GET", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools", ta, ""); !strings.Contains(string(r.body), `"channel":"prod"`) {
		t.Fatalf("the tenant administrator reads where it is placed: %s", r.body)
	}

	// the withdrawal: a distribution admin's or a server administrator's, with a reason
	const W = T + "/catalogue/publisher:enterest-ci/extensions/geo_tools/versions/1.0/withdraw"
	call(404, "POST", W, ta, `{"reason":"CVE-2026-0001"}`)
	call(400, "POST", W, da, `{"reason":""}`)
	call(404, "POST", T+"/catalogue/publisher:own-ci/extensions/geo_tools/versions/1.0/withdraw", da, `{"reason":"x"}`)
	call(404, "POST", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools/versions/9.9/withdraw", da, `{"reason":"x"}`)
	r = call(200, "POST", W, da, `{"reason":"CVE-2026-0001"}`)
	var wd struct {
		Tenant, Version string
		Already         bool
		Withdrawn       []struct{ Build string }
	}
	if json.Unmarshal(r.body, &wd) != nil || wd.Tenant != "acme" || wd.Version != "1.0" || wd.Already || len(wd.Withdrawn) != 1 ||
		wd.Withdrawn[0].Build != build || strings.Contains(string(r.body), "prod") || strings.Contains(string(r.body), "channel") {
		t.Fatalf("withdrawn: %s", r.body)
	}
	if r := call(200, "POST", W, sa, `{"reason":"again"}`); r.json(t)["already"] != true {
		t.Fatalf("withdrawn again: %s", r.body)
	}
	if r := m.call(t, "GET", geoPath, m.toks["install"], ""); r.status != 404 {
		t.Fatalf("served after the withdrawal: %d", r.status)
	}
	if r := call(200, "GET", T+"/events?kind=catalogue.withdrawn", ta, ""); !strings.Contains(string(r.body), "CVE-2026-0001") {
		t.Fatalf("the tenant is told: %s", r.body)
	}
	// shown withdrawn; never current, placed or purged again; its body never arrives again
	rels := call(200, "GET", T+"/channels/prod/extensions/geo_tools/releases", ta, "").json(t)["releases"].([]any)
	rel := rels[0].(map[string]any)
	if rel["withdrawn"] != true {
		t.Fatalf("the release shows it: %v", rel)
	}
	if r := m.call(t, "POST", T+"/channels/prod/extensions/geo_tools/releases/"+rel["id"].(string)+"/current", ta, "", "If-Match", "*"); r.status != 409 ||
		r.json(t)["type"] != "urn:kista:problem:withdrawn" {
		t.Fatalf("made current: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/channels/staging/extensions/geo_tools/releases/place", sa,
		`{"source":"publisher:enterest-ci","version":"1.0","early":true}`); r.status != 409 || r.json(t)["type"] != "urn:kista:problem:withdrawn" {
		t.Fatalf("placed again: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/catalogue/builds/"+build, ta, ""); r.status != 409 {
		t.Fatalf("purged: %d %s", r.status, r.body)
	}
	if r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds"+q, key, geo); r.status != 409 || r.json(t)["type"] != "urn:kista:problem:withdrawn" {
		t.Fatalf("the body again: %d %s", r.status, r.body)
	}
	if r := call(200, "GET", T+"/catalogue?name=geo_tools", ta, ""); !strings.Contains(string(r.body), `"withdrawn":["1.0"]`) {
		t.Fatalf("the catalogue shows it: %s", r.body)
	}
	if r := call(200, "GET", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools", da, ""); !strings.Contains(string(r.body), `"state":"withdrawn"`) ||
		!strings.Contains(string(r.body), `"withdrawn_reason":"CVE-2026-0001"`) {
		t.Fatalf("the entry shows it: %s", r.body)
	}

	// purges: a distributor's held entries with its key (purge_held), an entry placed nowhere
	call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"1h"}`)
	if r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds?version=1.1&platform=linux_amd64", key, built(t, "1.1", "geo_tools_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("1.1: %d %s", r.status, r.body)
	}
	key2 := m.addKey(t, da, "acme", "enterest-ci")
	r = m.upload(t, T+"/catalogue/extensions/geo_tools/builds?version=1.2&platform=linux_amd64", key2, built(t, "1.2", "geo_tools_duckdb_cpp_init"))
	if r.status != 201 {
		t.Fatalf("1.2: %d %s", r.status, r.body)
	}
	b12 := r.json(t)["id"].(string)
	r = call(200, "DELETE", T+"/publishers/enterest-ci/keys/"+m.signers[key].id+"?purge_held=true", da, "")
	if !strings.Contains(string(r.body), `"version":"1.1"`) || strings.Contains(string(r.body), `"version":"1.0"`) {
		t.Fatalf("its key's held entries purged (the withdrawn one kept): %s", r.body)
	}
	tenantBuild := call(200, "GET", T+"/catalogue/tenant/extensions/tresor", ta, "").json(t)["versions"].([]any)[0].(map[string]any)["builds"].([]any)[0].(map[string]any)["id"].(string)
	call(404, "DELETE", T+"/catalogue/builds/"+tenantBuild, da, "")
	call(204, "DELETE", T+"/catalogue/builds/"+b12, da, "")
	// the tenant's own publisher's keys are not a distribution admin's
	call(404, "POST", T+"/publishers/own-ci/keys", da, `{"public_key":"`+newSigner(t).authKey+`"}`)

	// removed by the tenant: its versions are still the distributor's to withdraw and read
	call(201, "POST", T+"/publishers", da, `{"name":"gone-ci"}`)
	call(200, "PUT", T+"/publishers/gone-ci/distributor", da, `{"distributor":"enterest"}`)
	gone := m.addKey(t, da, "acme", "gone-ci")
	if r := m.upload(t, T+"/catalogue/extensions/gone_ext/builds"+q, gone, built(t, "1.0", "gone_ext_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("gone_ext: %d %s", r.status, r.body)
	}
	call(204, "DELETE", T+"/publishers/gone-ci", ta, "")
	call(200, "GET", T+"/catalogue/publisher:gone-ci/extensions/gone_ext", da, "")
	call(200, "POST", T+"/catalogue/publisher:gone-ci/extensions/gone_ext/versions/1.0/withdraw", da, `{"reason":"retired"}`)
}
