package api_test

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0037 PR 1: the answers name a publisher by its id: every publisher answer, a signed whoami
// (with a distributor's publisher's distributor), a catalogue-only publication (the publisher that
// published, omitted for a tenant principal's publication), and the publisher.add and
// publisher.remove events.
func TestPublisherIDs(t *testing.T) {
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
	idOf := func(r resp) string {
		t.Helper()
		id, _ := r.json(t)["id"].(string)
		if id == "" {
			t.Fatalf("no id: %s", r.body)
		}
		return id
	}

	own := idOf(call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`))
	dist := idOf(call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`))
	for _, r := range []resp{
		call(200, "GET", T+"/publishers/own-ci", ta, ""),
		call(200, "PUT", T+"/publishers/own-ci/hold", ta, `{"hold":"0s"}`),
		call(200, "POST", T+"/publishers/own-ci/license-keys", sa, ""),
	} {
		if idOf(r) != own {
			t.Fatalf("the tenant's publisher's id: %s", r.body)
		}
	}
	for _, r := range []resp{
		call(200, "GET", T+"/publishers/enterest-ci", da, ""),
		call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"0s"}`),
		call(200, "PUT", T+"/publishers/enterest-ci/distributor", da, `{"distributor":"enterest-eu"}`),
	} {
		if idOf(r) != dist {
			t.Fatalf("the distributor's publisher's id: %s", r.body)
		}
	}
	if r := call(200, "GET", T+"/publishers", ta, ""); !strings.Contains(string(r.body), `"id":"`+own+`"`) ||
		!strings.Contains(string(r.body), `"id":"`+dist+`"`) {
		t.Fatalf("the list: %s", r.body)
	}

	// a signed whoami: each publisher with its id, and a distributor's with its distributor
	key := m.addKey(t, da, "acme", "enterest-ci")
	ownKey := m.addKey(t, sa, "acme", "own-ci")
	pub := func(handle string) map[string]any {
		t.Helper()
		ps := call(200, "GET", T+"/whoami", handle, "").json(t)["publishers"].([]any)
		if len(ps) != 1 {
			t.Fatalf("whoami: %v", ps)
		}
		return ps[0].(map[string]any)
	}
	if p := pub(key); p["id"] != dist || p["distributor"] != "enterest-eu" || p["publisher"] != "enterest-ci" {
		t.Fatalf("a distributor's publisher's whoami: %v", p)
	}
	if p := pub(ownKey); p["id"] != own || p["distributor"] != nil {
		t.Fatalf("the tenant's publisher's whoami: %v", p)
	}

	// a catalogue-only publication names the publisher that published; a tenant principal's does not
	q := "?version=1.0&platform=linux_amd64"
	if b := m.upload(t, T+"/catalogue/extensions/geo_tools/builds"+q, key, built(t, "1.0", "geo_tools_duckdb_cpp_init")).json(t); b["publisher_id"] != dist ||
		b["distributor"] != "enterest-eu" {
		t.Fatalf("a distributor's publication: %v", b)
	}
	call(201, "POST", T+"/grants", sa, `{"principal":"publisher:own-ci","verbs":["publish"]}`) // marked: a server administrator's
	if b := m.upload(t, T+"/catalogue/extensions/own_ext/builds"+q, ownKey, built(t, "1.0", "own_ext_duckdb_cpp_init")).json(t); b["publisher_id"] != own ||
		b["distributor"] != nil {
		t.Fatalf("the tenant's publisher's publication: %v", b)
	}
	if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|tom", []string{"publish"}, tenants.GrantOn{}); err != nil {
		t.Fatal(err)
	}
	if r := m.upload(t, T+"/catalogue/extensions/tom_ext/builds"+q, ta, built(t, "1.0", "tom_ext_duckdb_cpp_init")); r.status != 201 ||
		strings.Contains(string(r.body), "publisher_id") || strings.Contains(string(r.body), "distributor") {
		t.Fatalf("a tenant principal's publication: %d %s", r.status, r.body)
	}

	// the events carry the id
	call(204, "DELETE", T+"/publishers/own-ci", ta, "")
	for _, kind := range []string{"publisher.add", "publisher.remove"} {
		if r := call(200, "GET", T+"/events?kind="+kind, ta, ""); !strings.Contains(string(r.body), `"id":"`+own+`"`) {
			t.Fatalf("%s: %s", kind, r.body)
		}
	}
}
