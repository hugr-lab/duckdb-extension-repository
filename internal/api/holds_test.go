package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0033 phase A2: a tenant's administrators set an eager mirror's and a publisher's hold, a
// channel's administrators its channel's; a publisher with a hold is refused on a channel's route
// (409, …/held) and publishes into the catalogue, which shows the entry held; placing it takes
// early, a channel administrator's; revoking its key with purge_held purges it.
func TestHoldRoutes(t *testing.T) {
	m := newMgmt(t)
	ta, carl := m.toks["tenant admin"], m.toks["channel adm"]
	const T = "/api/v1/tenants/acme"
	// an eager mirror's hold; a lazy mirror takes none
	for _, u := range []string{
		`{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`,
		`{"name":"lazy","kind":"duckdb-community","channel":"staging","fetch":"lazy","platforms":["linux_amd64"],"extensions":[{"name":"h3"}]}`,
	} {
		if r := m.call(t, "POST", T+"/upstreams", ta, u); r.status != 201 {
			t.Fatalf("an upstream: %d %s", r.status, r.body)
		}
	}
	for who, want := range map[string]int{"anonymous": 401, "install": 404, "channel adm": 404, "ext admin": 404, "server dist admin": 404} {
		if r := m.call(t, "PUT", T+"/upstreams/core/hold", m.toks[who], `{"hold":"1h"}`); r.status != want {
			t.Errorf("%s sets an upstream's hold: %d", who, r.status)
		}
	}
	if r := m.call(t, "PUT", T+"/upstreams/core/hold", ta, `{"hold":"1h"}`); r.status != 200 || r.json(t)["hold"] != "1h0m0s" {
		t.Fatalf("an upstream's hold: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", T+"/upstreams/core/hold", ta, `{"hold":"0s"}`, "If-Match", `"v1"`); r.status != 412 {
		t.Fatalf("an upstream's hold at an old version: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/upstreams/core", ta, ""); r.json(t)["hold"] != "1h0m0s" {
		t.Fatalf("the upstream shows its hold: %s", r.body)
	}
	if r := m.call(t, "PUT", T+"/upstreams/lazy/hold", ta, `{"hold":"1h"}`); r.status != 400 {
		t.Fatalf("a lazy mirror's hold: %d %s", r.status, r.body)
	}
	for body, want := range map[string]int{`{"hold":"-1h"}`: 400, `{"hold":"x"}`: 400, `{}`: 400, `{"hold":"2200h"}`: 400, `{"hold":"1.5s"}`: 400} {
		if r := m.call(t, "PUT", T+"/upstreams/core/hold", ta, body); r.status != want {
			t.Errorf("%s: %d %s", body, r.status, r.body)
		}
	}
	// a channel's hold, its administrators'
	for who, want := range map[string]int{"install": 404, "ext admin": 404, "channel adm": 200, "tenant admin": 200} {
		if r := m.call(t, "GET", T+"/channels/prod/hold", m.toks[who], ""); r.status != want {
			t.Errorf("%s reads prod's hold: %d", who, r.status)
		}
	}
	if r := m.call(t, "PUT", T+"/channels/prod/hold", carl, `{"hold":"2h"}`); r.status != 200 || r.json(t)["hold"] != "2h0m0s" {
		t.Fatalf("a channel's hold: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/channels/prod/hold", carl, ""); r.json(t)["hold"] != "2h0m0s" {
		t.Fatalf("the channel's hold read: %s", r.body)
	}
	if r := m.call(t, "PUT", T+"/channels/prod/hold", carl, `{"hold":"0"}`); r.status != 200 || r.json(t)["hold"] != "0s" {
		t.Fatalf("no hold: %d %s", r.status, r.body)
	}
	// a publisher's hold, its tenant administrators'
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"ci"}`); r.status != 201 {
		t.Fatalf("a publisher: %d %s", r.status, r.body)
	}
	ci := m.addKey(t, ta, "acme", "ci")
	for _, on := range []tenants.GrantOn{{Channel: "prod"}, {Package: "newext"}} {
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "publisher:ci", []string{"publish"}, on); err != nil {
			t.Fatal(err)
		}
	}
	for who, want := range map[string]int{"channel adm": 404, "server dist admin": 404} {
		if r := m.call(t, "PUT", T+"/publishers/ci/hold", m.toks[who], `{"hold":"1h"}`); r.status != want {
			t.Errorf("%s sets a publisher's hold: %d", who, r.status)
		}
	}
	if r := m.call(t, "PUT", T+"/publishers/ci/hold", ta, `{"hold":"1h"}`); r.status != 200 || r.json(t)["hold"] != "1h0m0s" {
		t.Fatalf("a publisher's hold: %d %s", r.status, r.body)
	}
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	q := "?version=1.0&platform=linux_amd64"
	r := m.upload(t, T+"/channels/prod/extensions/newext/releases"+q, ci, file)
	if r.status != 409 || r.json(t)["type"] != "urn:kista:problem:held" || !strings.Contains(r.json(t)["detail"].(string), "catalogue") {
		t.Fatalf("a held publisher on a channel's route: %d %s", r.status, r.body)
	}
	r = m.upload(t, T+"/catalogue/extensions/newext/builds"+q, ci, file)
	if b := r.json(t); r.status != 201 || b["state"] != "held" || b["available_at"] == nil {
		t.Fatalf("into the catalogue: %d %s", r.status, r.body)
	}
	id := r.json(t)["id"].(string)
	if r := m.call(t, "GET", T+"/catalogue?name=newext", ta, ""); !strings.Contains(string(r.body), `"held":["1.0"]`) {
		t.Fatalf("the catalogue's held versions: %s", r.body)
	}
	if r := m.call(t, "GET", T+"/catalogue/publisher:ci/extensions/newext", ta, ""); !strings.Contains(string(r.body), `"state":"held"`) {
		t.Fatalf("the entry: %s", r.body)
	}
	// placing it: early, by a channel administrator
	const P = T + "/channels/prod/extensions/newext/releases/place"
	// placing takes promote (admin does not imply it); early, the channel's administration too
	if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|carl", []string{"promote"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "POST", P, carl, `{"source":"publisher:ci","version":"1.0"}`); r.status != 409 || r.json(t)["type"] != "urn:kista:problem:held" {
		t.Fatalf("placed without early: %d %s", r.status, r.body)
	}
	if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|pia", []string{"promote"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	pia := m.keys.sign(t, idpURL, map[string]any{"sub": "pia", "aud": publicURL + "/acme"})
	if r := m.call(t, "POST", P, pia, `{"source":"publisher:ci","version":"1.0","early":true}`); r.status != 404 {
		t.Fatalf("early by a holder of promote: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", P, carl, `{"source":"publisher:ci","version":"1.0","early":true}`); r.status != 201 {
		t.Fatalf("placed early: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/events?kind=release.place", ta, ""); !strings.Contains(string(r.body), `\"early\":true`) &&
		!strings.Contains(string(r.body), `"early":true`) {
		t.Fatalf("the early placement's event: %s", r.body)
	}
	// revoking the key with purge_held purges what it published still held and placed nowhere
	if r := m.upload(t, T+"/catalogue/extensions/newext/builds?version=1.1&platform=linux_amd64", ci, built(t, "1.1", "newext_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("1.1: %d %s", r.status, r.body)
	}
	key := m.signers[ci].id
	if r := m.call(t, "DELETE", T+"/publishers/ci/keys/"+key+"?purge_held=maybe", ta, ""); r.status != 400 {
		t.Fatalf("purge_held=maybe: %d %s", r.status, r.body)
	}
	// the answer lists what it purged, and the entry placed early it kept, revoked, with its channels
	r = m.call(t, "DELETE", T+"/publishers/ci/keys/"+key+"?purge_held=true", ta, "")
	var purge struct {
		Purged []struct{ Build, Version string } `json:"purged"`
		Kept   []struct {
			Build, Version string
			Channels       int
		} `json:"kept"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &purge) != nil || len(purge.Purged) != 1 || purge.Purged[0].Version != "1.1" ||
		len(purge.Kept) != 1 || purge.Kept[0].Build != id || purge.Kept[0].Channels != 1 {
		t.Fatalf("revoked: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/catalogue/publisher:ci/extensions/newext", ta, ""); strings.Contains(string(r.body), `"1.1"`) ||
		!strings.Contains(string(r.body), id) || !strings.Contains(string(r.body), `"revoked_at"`) {
		t.Fatalf("after the purge: %s", r.body)
	}
	if r := m.call(t, "GET", T+"/catalogue?name=newext", ta, ""); !strings.Contains(string(r.body), `"revoked":["1.0"]`) {
		t.Fatalf("the catalogue after the purge: %s", r.body)
	}
	if r := m.call(t, "GET", T+"/events?kind=publisher.key.remove", ta, ""); !strings.Contains(string(r.body), id) {
		t.Fatalf("the key's removal event, with the entry kept: %s", r.body)
	}
	if r := m.call(t, "GET", T+"/events?kind=catalogue.purge", ta, ""); !strings.Contains(string(r.body), "publisher.key.remove") {
		t.Fatalf("the purge's event: %s", r.body)
	}
}
