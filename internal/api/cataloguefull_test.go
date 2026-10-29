package api_test

import "testing"

// Spec 0037: a source's full catalogue answers 429 catalogue-full, without Retry-After: it clears
// only when the source's builds are placed, purged or trimmed, so a client stops.
func TestCatalogueFull(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	da := m.toks["server dist admin"]
	if r := m.call(t, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`); r.status != 201 {
		t.Fatalf("the publisher: %d %s", r.status, r.body)
	}
	key := m.addKey(t, da, "acme", "enterest-ci")
	m.apiRel.MaxEntries = 1
	if r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds?version=1.0&platform=linux_amd64", key,
		built(t, "1.0", "geo_tools_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("the first build: %d %s", r.status, r.body)
	}
	r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds?version=1.1&platform=linux_amd64", key, built(t, "1.1", "geo_tools_duckdb_cpp_init"))
	if r.status != 429 || r.json(t)["type"] != "urn:kista:problem:catalogue-full" || r.header.Get("Retry-After") != "" || r.json(t)["detail"] == nil {
		t.Fatalf("a full catalogue: %d %v %s", r.status, r.header, r.body)
	}
}
