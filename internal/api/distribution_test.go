package api_test

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Spec 0030 part 2: a distribution admin reserves names in a tenant, marks publishers and manages
// the marked ones, and grants publish, promote and admin on extensions; nothing else. A tenant's
// administrators read its reserved names, and change none.
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

	// reserved names in the tenant
	r := call(200, "PUT", T+"/reserved-names/acme_tool", da, `{"holder":"enterest"}`)
	if !strings.Contains(string(r.body), `"changed":true`) || !strings.Contains(string(r.body), `"releases":[]`) {
		t.Fatalf("reserve: %s", r.body)
	}
	r = call(200, "PUT", T+"/reserved-names/tresor", da, `{"holder":"enterest"}`)
	if !strings.Contains(string(r.body), `"channel":"prod"`) || !strings.Contains(string(r.body), `"version":"1.0"`) {
		t.Fatalf("a name with releases: %s", r.body)
	}
	r = call(200, "PUT", T+"/reserved-names/tresor", sa, `{"holder":"other"}`)
	if !strings.Contains(string(r.body), `"previous_holder":"enterest"`) {
		t.Fatalf("another holder: %s", r.body)
	}
	call(400, "PUT", T+"/reserved-names/acme_tool", da, `{"holder":""}`)
	call(400, "PUT", T+"/reserved-names/http", da, `{"holder":"enterest"}`) // an alias
	call(404, "PUT", T+"/reserved-names/mine", ta, `{"holder":"me"}`)       // not a tenant administrator's
	call(404, "DELETE", T+"/reserved-names/acme_tool", ta, "")
	if r := call(200, "GET", T+"/reserved-names", ta, ""); !strings.Contains(string(r.body), `"name":"acme_tool"`) ||
		!strings.Contains(string(r.body), `"holder":"enterest"`) {
		t.Fatalf("the tenant administrator's list: %s", r.body)
	}
	call(200, "GET", T+"/reserved-names/acme_tool", da, "")
	call(404, "GET", T+"/reserved-names", m.toks["ext admin"], "")
	call(204, "DELETE", T+"/reserved-names/tresor", da, "")
	call(404, "DELETE", T+"/reserved-names/tresor", da, "")
	call(404, "PUT", "/api/v1/tenants/nope/reserved-names/x", da, `{"holder":"enterest"}`)

	// a marked publisher, its key and its grants
	call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci"}`)
	call(200, "POST", T+"/publishers/enterest-ci/license-keys", da, "")
	k := newSigner(t)
	kid := call(201, "POST", T+"/publishers/enterest-ci/keys", da, `{"public_key":"`+k.authKey+`"}`).json(t)["id"].(string)
	// spec 0033 phase A2: purging a key's held entries is a tenant administrator's until phase B
	call(404, "DELETE", T+"/publishers/enterest-ci/keys/"+kid+"?purge_held=true", da, "")
	if r := call(200, "GET", T+"/publishers/enterest-ci/keys", da, ""); !strings.Contains(string(r.body), kid) {
		t.Fatalf("the key after a refused purge: %s", r.body)
	}
	r = call(201, "POST", T+"/grants", da, `{"principal":"publisher:enterest-ci","verbs":["publish","promote"],"extension":"acme_tool"}`)
	grant := r.json(t)["id"].(string)
	call(201, "POST", T+"/grants", da, `{"principal":"subject:corp|dana","verbs":["admin"],"channel":"prod","extension":"acme_tool"}`)
	for name, body := range map[string]string{
		"a channel-wide grant":               `{"principal":"publisher:enterest-ci","verbs":["publish"],"channel":"prod"}`,
		"a tenant-wide grant":                `{"principal":"subject:corp|dana","verbs":["admin"]}`,
		"another verb":                       `{"principal":"subject:corp|dana","verbs":["install"],"extension":"acme_tool"}`,
		"an extension group":                 `{"principal":"subject:corp|dana","verbs":["admin"],"extension_group":"platform"}`,
		"a whole issuer":                     `{"principal":"issuer:corp","verbs":["install"],"extension":"acme_tool"}`,
		"the tenant's own publisher":         `{"principal":"publisher:own-ci","verbs":["publish"],"extension":"acme_tool"}`,
		"a publisher not there at all":       `{"principal":"publisher:ghost","verbs":["publish"],"extension":"acme_tool"}`,
		"a name the tenant does not reserve": `{"principal":"publisher:enterest-ci","verbs":["publish"],"extension":"tresor"}`,
	} {
		if name == "the tenant's own publisher" {
			call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`)
		}
		if r := m.call(t, "POST", T+"/grants", da, body); r.status < 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// the tenant's own publishers are its administrators': their keys, and marking one with grants
	call(404, "POST", T+"/publishers/own-ci/keys", da, `{"public_key":"`+newSigner(t).authKey+`"}`)
	call(404, "GET", T+"/publishers/own-ci/keys", da, "")
	if r := call(200, "GET", T+"/publishers/own-ci", da, ""); !strings.Contains(string(r.body), `"keys":[]`) {
		t.Fatalf("an unmarked publisher, read without its keys: %s", r.body)
	}
	call(201, "POST", T+"/grants", ta, `{"principal":"publisher:own-ci","verbs":["publish"]}`)
	call(400, "POST", T+"/publishers/own-ci/license-keys", da, "")
	// a grant the tenant administrator made on a reserved name stays theirs
	dana := call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|dana","verbs":["admin"],"extension":"acme_tool"}`).json(t)["id"].(string)
	call(404, "DELETE", T+"/grants/"+dana, da, "")
	call(404, "DELETE", T+"/publishers/enterest-ci", da, "")
	// the marked publisher's are no longer the tenant administrator's
	call(404, "POST", T+"/publishers/enterest-ci/keys", ta, `{"public_key":"`+newSigner(t).authKey+`"}`)
	// grants: its own kind only
	call(204, "DELETE", T+"/grants/"+grant, da, "")
	// unmarking answers the publisher
	call(201, "POST", T+"/publishers", da, `{"name":"spare-ci"}`)
	call(200, "POST", T+"/publishers/spare-ci/license-keys", da, "")
	call(200, "DELETE", T+"/publishers/spare-ci/license-keys", da, "")
	call(404, "DELETE", T+"/grants/"+m.other, da, "") // another tenant's
	gs := call(200, "GET", T+"/grants", da, "").json(t)["grants"].([]any)
	for _, g := range gs {
		x := g.(map[string]any)
		if x["principal"] == "subject:corp|tom" { // the tenant administrator's grant
			call(404, "DELETE", T+"/grants/"+x["id"].(string), da, "")
		}
	}
	// nothing else
	call(404, "GET", T+"/channels/prod/keys", da, "")
	call(404, "GET", T, da, "")
	call(404, "POST", "/api/v1/reserved-names", da, `{"names":["x_y"],"kind":"core"}`)
	call(404, "PUT", T+"/holder", da, `{"holder_id":"x"}`)
	if w := call(200, "GET", "/api/v1/whoami", da, "").json(t); w["distribution_admin"] != true || w["administrator"] != false {
		t.Fatalf("whoami: %v", w)
	}
}
