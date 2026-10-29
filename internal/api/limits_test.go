package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0038: a tenant's limits. Its administrators and server administrators read them (what is set,
// in force and used); only a server administrator sets them, a partial PUT leaving the others, a
// no-op writing nothing, If-Match honoured when sent; each change is a tenant.limits event;
// POST /tenants takes them; growth past a count is 429 tenant-limit naming the limit, with no
// figures; a distributor's publisher is not counted; a suspended tenant's limits are read and set.
func TestTenantLimitsAPI(t *testing.T) {
	m := newMgmt(t)
	ta, sa, da := m.toks["tenant admin"], m.toks["server admin"], m.toks["server dist admin"]
	const T = "/api/v1/tenants/acme"
	call := func(want int, method, path, tok, body string, hdr ...string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body, hdr...)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	limit := func(r resp, name string) map[string]any {
		t.Helper()
		return r.json(t)["limits"].(map[string]any)[name].(map[string]any)
	}
	r := call(200, "GET", T+"/limits", ta, "")
	if b := limit(r, "bytes"); b["set"] != "default" || b["limit"] != nil || b["used"] == nil {
		t.Fatalf("bytes: %v", b)
	}
	if c := limit(r, "channels"); c["set"] != "default" || c["limit"] != 1000.0 || c["used"] != 2.0 {
		t.Fatalf("channels: %v", c)
	}
	tag := r.header.Get("ETag")
	if tag == "" || call(200, "GET", T+"/limits", ta, "", "If-None-Match", tag).status != 200 {
		t.Fatal("the ETag, and never a 304: what is used changes without it")
	}
	call(200, "GET", T+"/limits", sa, "")
	call(404, "GET", T+"/limits", da, "")
	call(404, "GET", T+"/limits", m.toks["no grants"], "")
	call(404, "PUT", T+"/limits", ta, `{"channels": 3}`)
	call(404, "PUT", T+"/limits", da, `{"channels": 3}`)

	// set: a partial PUT, sizes, unlimited, 0
	call(412, "PUT", T+"/limits", sa, `{"channels": 3}`, "If-Match", `"l0.0.0.0"`)
	r = call(200, "PUT", T+"/limits", sa, `{"bytes": "1GiB", "channels": 3, "upstreams": 0}`, "If-Match", tag)
	if b := limit(r, "bytes"); b["set"] != 1073741824.0 || b["limit"] != 1073741824.0 {
		t.Fatalf("bytes set: %v", b)
	}
	if u := limit(r, "upstreams"); u["set"] != 0.0 || u["limit"] != 0.0 {
		t.Fatalf("upstreams set: %v", u)
	}
	r = call(200, "PUT", T+"/limits", sa, `{"publishers": "unlimited"}`)
	if p, c := limit(r, "publishers"), limit(r, "channels"); p["set"] != "unlimited" || p["limit"] != 200.0 || c["set"] != 3.0 {
		t.Fatalf("a partial PUT: %v %v", p, c)
	}
	// set round-trips; a no-op writes nothing
	var body map[string]any
	_ = json.Unmarshal(r.body, &body)
	again := map[string]any{}
	for n, v := range body["limits"].(map[string]any) {
		again[n] = v.(map[string]any)["set"]
	}
	b, _ := json.Marshal(again)
	call(200, "PUT", T+"/limits", sa, string(b))
	evs := call(200, "GET", T+"/events?kind=tenant.limits", sa, "").json(t)["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("the events (a no-op writes none): %v", evs)
	}
	data := evs[len(evs)-1].(map[string]any)["data"].(map[string]any)
	if ch, ok := data["channels"].(map[string]any); !ok || ch["old"] != "default" || ch["new"] != 3.0 || ch["limit"] != 3.0 ||
		data["bytes"].(map[string]any)["new"] != 1073741824.0 || data["publishers"] != nil {
		t.Fatalf("the first change's event: %v", data)
	}
	for _, bad := range []string{`{"channels": 1001}`, `{"channels": -1}`, `{"egress": 1}`, `{"bytes": "lots"}`, `{"channels": "1GiB"}`,
		`{"channels": 1.5}`, `{"channels": true}`, `{"channels": null}`, `{"bytes": "1.5GiB"}`} {
		call(400, "PUT", T+"/limits", sa, bad)
	}
	// default again: set round-trips, and the event says so
	if r := call(200, "PUT", T+"/limits", sa, `{"bytes": "default"}`); limit(r, "bytes")["set"] != "default" || limit(r, "bytes")["limit"] != nil {
		t.Fatalf("bytes back to the default: %s", r.body)
	}
	evs = call(200, "GET", T+"/events?kind=tenant.limits", sa, "").json(t)["events"].([]any)
	if len(evs) != 3 {
		t.Fatalf("the events: %v", evs)
	}
	last := evs[0].(map[string]any)
	if evs[len(evs)-1].(map[string]any)["at"].(string) > last["at"].(string) {
		last = evs[len(evs)-1].(map[string]any)
	}
	if b := last["data"].(map[string]any)["bytes"].(map[string]any); b["old"] != 1073741824.0 || b["new"] != "default" || b["limit"] != nil {
		t.Fatalf("back to the default's event: %v", last)
	}

	// growth past a count: 429 tenant-limit, its limit named, no figures
	call(201, "POST", T+"/channels", ta, `{"name":"qa","kind":"signed"}`)
	r = call(429, "POST", T+"/channels", ta, `{"name":"dev2","kind":"signed"}`)
	if p := r.json(t); p["type"] != "urn:kista:problem:tenant-limit" || p["limit"] != "channels" || r.header.Get("Retry-After") != "" ||
		strings.ContainsAny(p["detail"].(string), "0123456789") {
		t.Fatalf("a channel past the limit: %v %v", p, r.header)
	}
	call(200, "PUT", T+"/limits", sa, `{"publishers": 0}`)
	if r := call(429, "POST", T+"/publishers", ta, `{"name":"ci"}`); r.json(t)["limit"] != "publishers" {
		t.Fatalf("a publisher past 0: %s", r.body)
	}
	call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`) // not counted
	if r := call(200, "GET", T+"/limits", ta, ""); limit(r, "publishers")["used"] != 0.0 {
		t.Fatalf("a distributor's publisher counted: %s", r.body)
	}
	if r := call(429, "POST", T+"/upstreams", ta,
		`{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`); r.json(t)["limit"] != "upstreams" {
		t.Fatalf("an upstream past 0: %s", r.body)
	}

	// a suspended tenant's limits: a server administrator's
	if r := m.call(t, "POST", T+"/suspend", sa, "", "If-Match", "*"); r.status >= 300 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	call(200, "GET", T+"/limits", sa, "")
	call(404, "GET", T+"/limits", ta, "") // to its administrators a suspended tenant is unknown
	call(200, "PUT", T+"/limits", sa, `{"publishers": 5}`)

	// created with its limits, recorded beside its creation
	r = call(201, "POST", "/api/v1/tenants", sa, `{"name":"beta","limits":{"bytes":"10GiB","channels":0}}`)
	if r = call(200, "GET", "/api/v1/tenants/beta/limits", sa, ""); limit(r, "channels")["set"] != 0.0 || limit(r, "bytes")["limit"] != 10737418240.0 {
		t.Fatalf("beta's limits: %s", r.body)
	}
	if r := call(200, "GET", "/api/v1/events?kind=tenant.limits", sa, ""); !strings.Contains(string(r.body), `"subject":"tenant:beta"`) {
		t.Fatalf("the creation's limits event: %s", r.body)
	}
	call(400, "POST", "/api/v1/tenants", sa, `{"name":"gamma","limits":{"channels":"many"}}`)
	if _, err := m.st.GetTenant(ctx, "gamma"); err == nil {
		t.Fatal("a tenant created with invalid limits")
	}
}

// Spec 0038: a publisher's publication past its tenant's bytes limit is 429 tenant-limit, without
// figures; the same body again passes; a distributor's publisher's publication passes past it.
func TestTenantLimitPublication(t *testing.T) {
	m := newMgmt(t)
	ta, sa, da := m.toks["tenant admin"], m.toks["server admin"], m.toks["server dist admin"]
	const T = "/api/v1/tenants/acme"
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"ci"}`); r.status != 201 {
		t.Fatalf("the publisher: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ci","verbs":["publish"],"extension":"geo_tools"}`); r.status != 201 {
		t.Fatalf("the grant: %d %s", r.status, r.body)
	}
	key := m.addKey(t, ta, "acme", "ci")
	if r := m.call(t, "PUT", T+"/limits", sa, `{"bytes": 0}`); r.status != 200 {
		t.Fatalf("the limit: %d %s", r.status, r.body)
	}
	const B = T + "/catalogue/extensions/geo_tools/builds?platform=linux_amd64&version="
	r := m.upload(t, B+"1.0", key, built(t, "1.0", "geo_tools_duckdb_cpp_init"))
	if p := r.json(t); r.status != 429 || p["type"] != "urn:kista:problem:tenant-limit" || p["limit"] != "bytes" ||
		strings.ContainsAny(p["detail"].(string), "0123456789") {
		t.Fatalf("a publication at a full tenant: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", T+"/limits", sa, `{"bytes": "unlimited"}`); r.status != 200 {
		t.Fatal(r.status)
	}
	if r := m.upload(t, B+"1.0", key, built(t, "1.0", "geo_tools_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("with room: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", T+"/limits", sa, `{"bytes": 0}`); r.status != 200 {
		t.Fatal(r.status)
	}
	if r := m.upload(t, B+"1.0", key, built(t, "1.0", "geo_tools_duckdb_cpp_init")); r.status != 200 {
		t.Fatalf("the same body again: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`); r.status != 201 {
		t.Fatal(r.status)
	}
	dkey := m.addKey(t, da, "acme", "enterest-ci")
	if r := m.upload(t, T+"/catalogue/extensions/tile/builds?platform=linux_amd64&version=1.0", dkey,
		built(t, "1.0", "tile_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("a distributor's publication past the limit: %d %s", r.status, r.body)
	}
	if n := store.LimitRefusals()[store.LimitBytes]; n == 0 {
		t.Fatal("the refusal not counted")
	}
}
