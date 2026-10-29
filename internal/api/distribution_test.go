package api_test

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Spec 0037: the distribution admin's matrix. It keeps distributors' publishers (their creation,
// reads, licence-key marks, holds, keys and their distributor's rename), their catalogue entries,
// the tenant's state and the server's reserved names (read); it loses the tenant's own publishers
// (as missing ones; a name one holds is only taken), ordinary publishers, grants, tenant
// reservations (removed: the routes are gone for everyone) and the tenant's minimum hold for
// distributors' publishers, which its administrators set.
func TestDistributionAdmin(t *testing.T) {
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

	// keeps: a distributor's publisher, created marked, read with its id and effective hold
	created := call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`).json(t)
	if created["id"] == "" || created["distributor"] != "enterest" || created["effective_hold"] != "24h0m0s" {
		t.Fatalf("created: %v", created)
	}
	if r := call(200, "GET", T+"/publishers/enterest-ci", da, "").json(t); r["id"] != created["id"] || r["effective_hold"] != "24h0m0s" ||
		r["hold"] != nil {
		t.Fatalf("read: %v", r)
	}
	call(200, "POST", T+"/publishers/enterest-ci/license-keys", da, "")
	k := newSigner(t)
	kid := call(201, "POST", T+"/publishers/enterest-ci/keys", da, `{"public_key":"`+k.authKey+`"}`).json(t)["id"].(string)
	if r := call(200, "GET", T+"/publishers/enterest-ci/keys", da, ""); !strings.Contains(string(r.body), kid) {
		t.Fatalf("its keys: %s", r.body)
	}
	call(200, "GET", T+"/publishers/enterest-ci/keys/"+kid, da, "")
	call(200, "DELETE", T+"/publishers/enterest-ci/keys/"+kid+"?purge_held=true", da, "")
	call(200, "PUT", T+"/publishers/enterest-ci/distributor", da, `{"distributor":"enterest-eu"}`) // renamed: unused (no hold yet)
	// its hold: anything, but its entries are held for the tenant's minimum at least
	if r := call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"0s"}`).json(t); r["hold"] != nil || r["effective_hold"] != "24h0m0s" {
		t.Fatalf("a hold of 0 under the minimum: %v", r)
	}
	if r := call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"48h"}`).json(t); r["effective_hold"] != "48h0m0s" {
		t.Fatalf("a hold over the minimum: %v", r)
	}
	call(200, "DELETE", T+"/publishers/enterest-ci/license-keys", da, "")
	call(200, "GET", T+"/catalogue", da, "")
	call(200, "GET", T, da, "")
	if r := call(200, "GET", "/api/v1/reserved-names?name=json", da, ""); strings.Contains(string(r.body), "sources") {
		t.Fatalf("the reserved names without their sources: %s", r.body)
	}

	// loses: the tenant's own publishers, as missing ones, with or without the licence-key mark
	call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`)
	call(201, "POST", T+"/publishers", sa, `{"name":"marked-ci"}`)
	call(200, "POST", T+"/publishers/marked-ci/license-keys", sa, "")
	for _, own := range []string{"own-ci", "marked-ci", "ghost-ci"} {
		P := T + "/publishers/" + own
		call(404, "GET", P, da, "")
		call(404, "POST", P+"/license-keys", da, "")
		call(404, "DELETE", P+"/license-keys", da, "")
		call(404, "PUT", P+"/hold", da, `{"hold":"1h"}`)
		call(404, "PUT", P+"/distributor", da, `{"distributor":"enterest"}`)
		call(404, "GET", P+"/keys", da, "")
		call(404, "POST", P+"/keys", da, `{"public_key":"`+newSigner(t).authKey+`"}`)
		call(404, "DELETE", P, da, "")
	}
	ownKey := m.addKey(t, ta, "acme", "own-ci")
	call(404, "GET", T+"/publishers/own-ci/keys/"+m.signers[ownKey].id, da, "")
	call(404, "DELETE", T+"/publishers/own-ci/keys/"+m.signers[ownKey].id, da, "")
	ps := call(200, "GET", T+"/publishers", da, "").json(t)["publishers"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["name"] != "enterest-ci" {
		t.Fatalf("the list holds the distributors' publishers only: %v", ps)
	}
	if r := call(200, "GET", T+"/publishers", ta, ""); !strings.Contains(string(r.body), `"name":"own-ci"`) {
		t.Fatalf("the tenant administrator's list: %s", r.body)
	}
	// a name a tenant's own publisher holds is taken, and nothing else is said
	for _, own := range []string{"own-ci", "marked-ci"} {
		r := call(409, "POST", T+"/publishers", da, `{"name":"`+own+`","distributor":"enterest"}`)
		if j := r.json(t); j["type"] != "urn:kista:problem:publisher-taken" || j["detail"] != "the name is taken by a publisher of the tenant's own" {
			t.Fatalf("a taken name: %s", r.body)
		}
	}
	if r := call(409, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`); r.json(t)["type"] != "urn:kista:problem:conflict" {
		t.Fatalf("a distributor's publisher's name: %s", r.body)
	}
	if r := call(409, "POST", T+"/publishers", sa, `{"name":"own-ci","distributor":"enterest"}`); r.json(t)["type"] != "urn:kista:problem:conflict" {
		t.Fatalf("a server administrator is told it exists: %s", r.body)
	}
	// ordinary publishers, grants
	call(404, "POST", T+"/publishers", da, `{"name":"plain-ci"}`)
	grant := call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|dana","verbs":["admin"],"channel":"prod","extension":"geo"}`).json(t)["id"].(string)
	call(404, "GET", T+"/grants", da, "")
	call(404, "POST", T+"/grants", da, `{"principal":"subject:corp|dana","verbs":["admin"],"channel":"prod","extension":"geo"}`)
	call(404, "GET", T+"/grants/"+grant, da, "")
	call(404, "DELETE", T+"/grants/"+grant, da, "")
	call(200, "GET", T+"/grants/"+grant, ta, "")
	// tenant reservations: removed, for everyone
	for _, tok := range []string{da, ta, sa} {
		call(404, "GET", T+"/reserved-names", tok, "")
		call(404, "PUT", T+"/reserved-names/acme_tool", tok, `{"holder":"enterest"}`)
		call(404, "DELETE", T+"/reserved-names/acme_tool", tok, "")
	}
	// the tenant's minimum hold for distributors' publishers: its administrators'
	call(404, "GET", T+"/distributor-hold", da, "")
	call(404, "PUT", T+"/distributor-hold", da, `{"hold":"0s"}`)
	if r := call(200, "GET", T+"/distributor-hold", ta, "").json(t); r["hold"] != "24h0m0s" {
		t.Fatalf("the default minimum: %v", r)
	}
	call(400, "PUT", T+"/distributor-hold", ta, `{"hold":"forever"}`)
	call(200, "PUT", T+"/distributor-hold", ta, `{"hold":"72h"}`)
	if r := call(200, "GET", T+"/publishers/enterest-ci", da, "").json(t); r["effective_hold"] != "72h0m0s" {
		t.Fatalf("over the publisher's: %v", r)
	}
	if r := call(200, "PUT", T+"/distributor-hold", sa, `{"hold":"0s"}`).json(t); r["hold"] != "0s" {
		t.Fatalf("set by a server administrator: %v", r)
	}
	if r := call(200, "GET", T+"/events?kind=catalogue.hold", ta, ""); !strings.Contains(string(r.body), `"subject":"tenant:acme"`) ||
		!strings.Contains(string(r.body), `"source":"distributors"`) {
		t.Fatalf("the minimum's event: %s", r.body)
	}
	// nothing else
	call(404, "GET", T+"/channels/prod/keys", da, "")
	call(404, "GET", T+"/issuers", da, "")
	call(404, "POST", "/api/v1/reserved-names", da, `{"names":["x_y"],"kind":"core"}`)
	call(404, "PUT", T+"/holder", da, `{"holder_id":"x"}`)
	if w := call(200, "GET", "/api/v1/whoami", da, "").json(t); w["distribution_admin"] != true || w["administrator"] != false {
		t.Fatalf("whoami: %v", w)
	}
}
