package api_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
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

// Spec 0037 PR 1: a distribution admin reads a tenant's name, state and expected holder (no ETag,
// nothing else), an active or a suspended one, never one whose deletion started; each read is a
// tenant.read event. A tenant administrator's answer, and that of a token that is a server
// administrator as well, are unchanged.
func TestDistributionAdminTenantRead(t *testing.T) {
	m := newMgmt(t)
	da, ta, sa := m.toks["server dist admin"], m.toks["tenant admin"], m.toks["server admin"]
	both := m.keys.sign(t, opsURL, map[string]any{"sub": "both", "aud": serverAud, "tid": "t1", "roles": []any{"kista.admin", "kista.distribution"}})
	const T = "/api/v1/tenants/acme"
	call := func(want int, method, path, tok, body string, hdr ...string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body, hdr...)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	read := func(path string) map[string]any {
		t.Helper()
		r := call(200, "GET", path, da, "")
		if r.header.Get("ETag") != "" {
			t.Fatalf("an ETag: %v", r.header)
		}
		return r.json(t)
	}
	if got := read(T); len(got) != 2 || got["name"] != "acme" || got["state"] != "active" {
		t.Fatalf("without a holder: %v", got)
	}
	call(200, "PUT", T+"/holder", sa, `{"holder_id":"org-acme"}`, "If-Match", "*")
	if got := read(T); len(got) != 3 || got["holder_id"] != "org-acme" {
		t.Fatalf("with a holder: %v", got)
	}
	// the tenant administrator's answer, and a server administrator's who is a distribution admin too
	for _, tok := range []string{ta, both} {
		if r := call(200, "GET", T, tok, ""); r.header.Get("ETag") == "" || r.json(t)["etag"] == nil || r.json(t)["created_at"] == nil {
			t.Fatalf("the full answer: %v %s", r.header, r.body)
		}
	}
	// suspended: read too, and told; a deleting tenant is unknown
	if _, err := m.ten.SetTenantState(ctx, admin, "acme", "suspended", 0); err != nil {
		t.Fatal(err)
	}
	if got := read(T); got["state"] != "suspended" {
		t.Fatalf("suspended: %v", got)
	}
	call(404, "GET", T+"/publishers", da, "") // the exception is this route's only
	if r := call(200, "GET", T+"/events?kind=tenant.read", sa, ""); strings.Count(string(r.body), `"kind":"tenant.read"`) != 3 ||
		!strings.Contains(string(r.body), `"state":"suspended"`) || !strings.Contains(string(r.body), `"subject":"tenant:acme"`) {
		t.Fatalf("the reads' events: %s", r.body)
	}
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ten.Delete(ctx, admin, "acme", acme.Version, 0); err != nil {
		t.Fatal(err)
	}
	call(404, "GET", T, da, "")
	call(404, "GET", "/api/v1/tenants/nope", da, "")
}

// Spec 0037 PR 1: a distributor's publisher reads its own catalogue entries with a signed request, in
// the distribution admin's view (no rules, no placements, its id on each entry): source, when named,
// is publisher:<its name> or publisher-id:<its id>, which always mean the signer; any other source is
// not found, and the tenant's own publisher's signed request is refused.
func TestSignedCatalogueReads(t *testing.T) {
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
	q := "?version=1.0&platform=linux_amd64"
	publish := func(handle, name, version string) {
		t.Helper()
		if r := m.upload(t, T+"/catalogue/extensions/"+name+"/builds?version="+version+"&platform=linux_amd64", handle,
			built(t, version, name+"_duckdb_cpp_init")); r.status != 201 {
			t.Fatalf("%s %s: %d %s", name, version, r.status, r.body)
		}
	}
	old := call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`).json(t)["id"].(string)
	key := m.addKey(t, da, "acme", "enterest-ci")
	publish(key, "geo_tools", "1.0")
	call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`)
	own := m.addKey(t, ta, "acme", "own-ci")
	call(201, "POST", T+"/grants", ta, `{"principal":"publisher:own-ci","verbs":["publish"]}`)
	if r := m.upload(t, T+"/catalogue/extensions/own_ext/builds"+q, own, built(t, "1.0", "own_ext_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("own_ext: %d %s", r.status, r.body)
	}
	call(201, "POST", T+"/channels/prod/extensions/geo_tools/releases/place", sa, `{"source":"publisher:enterest-ci","version":"1.0"}`)

	// the list: its own entries only, without rules
	for _, query := range []string{"", "?source=publisher:enterest-ci", "?source=publisher-id:" + old, "?type=duckdb_extension&name=geo_tools"} {
		r := call(200, "GET", T+"/catalogue"+query, key, "")
		es := r.json(t)["entries"].([]any)
		if len(es) != 1 {
			t.Fatalf("%q: %s", query, r.body)
		}
		e := es[0].(map[string]any)
		if e["source"] != "publisher:enterest-ci" || e["publisher_id"] != old || len(e["rules"].(map[string]any)) != 0 || strings.Contains(string(r.body), "prod") {
			t.Fatalf("%q: %s", query, r.body)
		}
	}
	missing := call(404, "GET", T+"/catalogue/nope/extensions/x", da, "").body
	for _, query := range []string{"?source=publisher:own-ci", "?source=tenant", "?source=publisher-id:" + old + "x"} {
		if r := call(404, "GET", T+"/catalogue"+query, key, ""); string(r.body) != string(missing) {
			t.Fatalf("another source: %s", r.body)
		}
	}
	// refused, so audited
	m.events.Close(ctx)
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if evs, err := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "authz.refused", Limit: 100}); err != nil ||
		!slices.ContainsFunc(evs, func(e store.Event) bool {
			return strings.HasPrefix(e.Actor, "publisher:") && strings.Contains(e.Data, "GET tenants/{t}/catalogue")
		}) {
		t.Fatalf("another source's refusal: %+v %v", evs, err)
	}
	// an entry: without placements
	for _, src := range []string{"publisher:enterest-ci", "publisher-id:" + old} {
		if r := call(200, "GET", T+"/catalogue/"+src+"/extensions/geo_tools", key, ""); strings.Contains(string(r.body), `"placed"`) ||
			!strings.Contains(string(r.body), `"version":"1.0"`) {
			t.Fatalf("%s: %s", src, r.body)
		}
	}
	call(404, "GET", T+"/catalogue/publisher:own-ci/extensions/own_ext", key, "")
	call(404, "GET", T+"/catalogue/tenant/extensions/tresor", key, "")
	// the tenant's own publisher reads nothing here
	call(404, "GET", T+"/catalogue", own, "")
	call(404, "GET", T+"/catalogue/publisher:own-ci/extensions/own_ext", own, "")

	// removed and created again under its name: publisher:<name> is the signer, the new one; the
	// removed one's key is gone with it
	call(204, "DELETE", T+"/publishers/enterest-ci", ta, "")
	if r := m.call(t, "GET", T+"/catalogue", key, ""); r.status != 401 {
		t.Fatalf("a removed publisher's key: %d %s", r.status, r.body)
	}
	cur := call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`).json(t)["id"].(string)
	key2 := m.addKey(t, da, "acme", "enterest-ci")
	publish(key2, "geo_tools", "2.0")
	r := call(200, "GET", T+"/catalogue?source=publisher:enterest-ci", key2, "")
	if es := r.json(t)["entries"].([]any); len(es) != 1 || es[0].(map[string]any)["publisher_id"] != cur ||
		es[0].(map[string]any)["source"] != "publisher-id:"+cur {
		t.Fatalf("the new one's list: %s", r.body)
	}
	if r := call(200, "GET", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools", key2, ""); !strings.Contains(string(r.body), `"version":"2.0"`) ||
		strings.Contains(string(r.body), `"version":"1.0"`) {
		t.Fatalf("the new one's entry: %s", r.body)
	}
	call(404, "GET", T+"/catalogue/publisher-id:"+old+"/extensions/geo_tools", key2, "")
}

// Spec 0037 PR 1: a distribution admin acts on distributors' publishers only. On a tenant's own
// publisher, with the licence-key mark or without, its mark and unmark, hold, distributor mark, key
// routes and grant paths answer the same 404 as a missing publisher (the regression test for the
// marked() hole: the tenant's CI publisher a server administrator marked holds the tenant's grants).
func TestDistributionAdminOwnPublishers(t *testing.T) {
	m := newMgmt(t)
	da, sa := m.toks["server dist admin"], m.toks["server admin"]
	const T = "/api/v1/tenants/acme"
	call := func(want int, method, path, tok, body string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	call(200, "PUT", T+"/reserved-names/acme_tool", da, `{"holder":"enterest"}`)
	call(201, "POST", T+"/publishers", sa, `{"name":"plain-ci"}`)
	call(201, "POST", T+"/publishers", sa, `{"name":"marked-ci"}`)
	call(200, "POST", T+"/publishers/marked-ci/license-keys", sa, "")
	missing := call(404, "GET", T+"/publishers/ghost-ci/keys", da, "").body
	for _, name := range []string{"plain-ci", "marked-ci"} {
		key := m.addKey(t, sa, "acme", name)
		kid := m.signers[key].id
		grant := call(201, "POST", T+"/grants", sa, `{"principal":"publisher:`+name+`","verbs":["publish"],"extension":"acme_tool"}`).json(t)["id"].(string)
		P := T + "/publishers/" + name
		for _, c := range []struct{ method, path, body string }{
			{"POST", P + "/license-keys", ""},
			{"DELETE", P + "/license-keys", ""},
			{"PUT", P + "/hold", `{"hold":"1h"}`},
			{"PUT", P + "/distributor", `{"distributor":"enterest"}`},
			{"GET", P + "/keys", ""},
			{"GET", P + "/keys/" + kid, ""},
			{"POST", P + "/keys", `{"public_key":"` + newSigner(t).authKey + `"}`},
			{"DELETE", P + "/keys/" + kid, ""},
			{"DELETE", P + "/keys/" + kid + "?purge_held=true", ""},
			{"POST", T + "/grants", `{"principal":"publisher:` + name + `","verbs":["promote"],"extension":"acme_tool"}`},
			{"DELETE", T + "/grants/" + grant, ""},
		} {
			if r := call(404, c.method, c.path, da, c.body); string(r.body) != string(missing) {
				// every 404 has constant bytes (spec 0007): this guards against a refusal that says
				// more (a typed or detailed one), not against the cases being told apart otherwise
				t.Fatalf("%s %s %s: not a missing publisher's answer: %s", name, c.method, c.path, r.body)
			}
		}
		// nothing changed: its one key, its one grant, its mark and its hold (none) are as they were
		if r := call(200, "GET", P+"/keys", sa, ""); len(r.json(t)["keys"].([]any)) != 1 || !strings.Contains(string(r.body), kid) {
			t.Fatalf("%s's keys: %s", name, r.body)
		}
		call(200, "GET", T+"/grants/"+grant, sa, "")
		n := 0
		for _, g := range call(200, "GET", T+"/grants", sa, "").json(t)["grants"].([]any) {
			if g.(map[string]any)["principal"] == "publisher:"+name {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s's grants: %d", name, n)
		}
		if r := call(200, "GET", P, sa, ""); r.json(t)["license_keys"] != (name == "marked-ci") || r.json(t)["distributor"] != nil ||
			r.json(t)["hold"] != nil {
			t.Fatalf("%s: %s", name, r.body)
		}
	}
	// the list of keys leaves the tenant's own publishers' out
	if r := call(200, "GET", T+"/publishers", da, ""); strings.Contains(string(r.body), `"fingerprint"`) {
		t.Fatalf("a distribution admin's list shows the tenant's publishers' keys: %s", r.body)
	}
	// a distributor's publisher stays the distribution admin's
	call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`)
	call(200, "POST", T+"/publishers/enterest-ci/license-keys", da, "")
	call(200, "DELETE", T+"/publishers/enterest-ci/license-keys", da, "")
	call(200, "PUT", T+"/publishers/enterest-ci/hold", da, `{"hold":"1h"}`)
	call(200, "PUT", T+"/publishers/enterest-ci/distributor", da, `{"distributor":"enterest-eu"}`)
	m.addKey(t, da, "acme", "enterest-ci")
	if r := call(200, "GET", T+"/publishers", da, ""); !strings.Contains(string(r.body), `"fingerprint"`) {
		t.Fatalf("its distributor's publisher's keys: %s", r.body)
	}

	// the server's reserved names, to a distribution admin, are each a name and its kind
	if r := call(200, "GET", "/api/v1/reserved-names?name=json", da, ""); !strings.Contains(string(r.body), `"name":"json"`) ||
		strings.Contains(string(r.body), "sources") || strings.Contains(string(r.body), "added_at") {
		t.Fatalf("a distribution admin's reserved names: %s", r.body)
	}
	if r := call(200, "GET", "/api/v1/reserved-names?name=json", sa, ""); !strings.Contains(string(r.body), `"sources"`) {
		t.Fatalf("a server administrator's reserved names: %s", r.body)
	}
}
