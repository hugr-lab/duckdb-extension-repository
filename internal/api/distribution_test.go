package api_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0037: the distribution admin's matrix. It keeps distributors' publishers (their creation,
// reads, licence-key marks, holds, keys and their distributor's rename), their catalogue entries
// (read, purged, withdrawn, their metadata), a distributor's statistics, the tenant's state and the
// server's reserved names (read); it loses the tenant's own publishers (as missing ones; a name one
// holds is only taken, a refusal audited, as is a key one holds), ordinary publishers, the removal
// of publishers, grants, tenant reservations (removed: the routes are gone for everyone) and the
// tenant's minimum hold for distributors' publishers, which its administrators set (it reads it
// only as its publishers' effective_hold).
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
	// its catalogue entries: read, a build purged, a version withdrawn, their metadata; a
	// distributor's statistics
	key := m.addKey(t, da, "acme", "enterest-ci")
	q := "?platform=linux_amd64&version="
	for _, v := range []string{"1.0", "1.1"} {
		if r := m.upload(t, T+"/catalogue/extensions/geo_tools/builds"+q+v, key, built(t, v, "geo_tools_duckdb_cpp_init")); r.status != 201 {
			t.Fatalf("geo_tools %s: %d %s", v, r.status, r.body)
		}
	}
	const E = T + "/catalogue/publisher:enterest-ci/extensions/geo_tools"
	vs := call(200, "GET", E, da, "").json(t)["versions"].([]any)
	for _, v := range vs {
		if x := v.(map[string]any); x["version"] == "1.1" {
			call(204, "DELETE", T+"/catalogue/builds/"+x["builds"].([]any)[0].(map[string]any)["id"].(string), da, "")
		}
	}
	call(200, "POST", E+"/versions/1.0/withdraw", da, `{"reason":"CVE-2026-0001"}`)
	call(200, "PUT", E+"/meta", da, `{"summary":"Geometry tools"}`)
	call(200, "GET", E+"/meta", da, "")
	// reached (a tenant administrator gets 404): no week is computed yet here
	call(400, "GET", "/api/v1/distributor-stats/downloads?distributor=enterest-eu", da, "")
	call(404, "GET", "/api/v1/distributor-stats/downloads?distributor=enterest-eu", ta, "")
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
	// a key one of the tenant's publishers holds is unique in the tenant: the 409 is a refusal
	call(409, "POST", T+"/publishers/enterest-ci/keys", da, `{"public_key":"`+m.signers[ownKey].authKey+`"}`)
	// one a distributor's publisher holds already: a plain 409, no probe
	again := m.addKey(t, da, "acme", "enterest-ci")
	call(409, "POST", T+"/publishers/enterest-ci/keys", da, `{"public_key":"`+m.signers[again].authKey+`"}`)
	call(404, "DELETE", T+"/publishers/enterest-ci", da, "") // a publisher's removal is the tenant's
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
	call(404, "GET", T+"/grants/principals", da, "")
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
	distAdmin := authz.Actor{Kind: authz.ActorDistributionAdmin, ID: "ops|enterest"}
	if _, err := m.env.adm.DistributorHold(ctx, distAdmin, "acme"); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("the service, a distribution admin's read: %v", err)
	}
	if _, err := m.env.adm.SetDistributorHold(ctx, distAdmin, "acme", 0); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("the service, a distribution admin's change: %v", err)
	}
	call(400, "PUT", T+"/distributor-hold", ta, `{"hold":"forever"}`)
	call(200, "PUT", T+"/distributor-hold", ta, `{"hold":"72h"}`)
	call(200, "PUT", T+"/distributor-hold", ta, `{"hold":"72h"}`) // unchanged: no event
	if r := call(200, "GET", T+"/publishers/enterest-ci", da, "").json(t); r["effective_hold"] != "72h0m0s" {
		t.Fatalf("over the publisher's: %v", r)
	}
	if r := call(200, "PUT", T+"/distributor-hold", sa, `{"hold":"0s"}`).json(t); r["hold"] != "0s" {
		t.Fatalf("set by a server administrator: %v", r)
	}
	if r := call(200, "GET", T+"/events?kind=catalogue.hold&subject=tenant:acme", ta, ""); len(r.json(t)["events"].([]any)) != 2 ||
		!strings.Contains(string(r.body), `"source":"distributors"`) {
		t.Fatalf("the minimum's events, one per change: %s", r.body)
	}
	if r := call(200, "GET", T+"/publishers/own-ci", ta, "").json(t); r["effective_hold"] != nil {
		t.Fatalf("the tenant's own publisher's hold is its own: %v", r)
	}
	// the refusals are recorded, each apart
	m.events.Close(ctx)
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "authz.refused", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"publisher-taken", "key-taken"} {
		if !slices.ContainsFunc(evs, func(e store.Event) bool { return strings.Contains(e.Data, `"reason":"`+reason+`"`) }) {
			t.Errorf("no refusal for %s: %+v", reason, evs)
		}
	}
	keyTaken := 0
	for _, e := range evs {
		if strings.Contains(e.Data, `"reason":"key-taken"`) {
			keyTaken++
		}
	}
	if keyTaken != 1 {
		t.Errorf("key-taken refusals: %d, want the own publisher's one only: %+v", keyTaken, evs)
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

// Spec 0037: raising the tenant's minimum holds longer what a removed distributor's publisher's
// entries are held now (its record kept; a removal through the API purges its held entries and keeps
// those placed early, so here the store removes it), and a live one's.
func TestDistributorHoldRaiseRemoved(t *testing.T) {
	m := newMgmt(t)
	da := m.toks["server dist admin"]
	const T = "/api/v1/tenants/acme"
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var builds []store.Build
	for i, name := range []string{"gone-ci", "live-ci"} {
		if r := m.call(t, "POST", T+"/publishers", da, `{"name":"`+name+`","distributor":"enterest"}`); r.status != 201 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
			p, err := tx.GetPublisher(ctx, acme.ID, name)
			if err != nil {
				return err
			}
			b := store.Build{TenantID: acme.ID, Source: store.PublisherSource(p.ID), Name: "geo_tools", PackageVersion: "0.1", Platform: "linux_amd64",
				ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat(string(rune('e'+i)), 64), Origin: store.OriginPublication,
				CreatedBy: "os:1:t"}
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			builds = append(builds, b)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeletePublisher(ctx, acme.ID, "gone-ci", "os:1:t") }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.env.adm.SetDistributorHold(ctx, admin, "acme", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
			got, err := tx.GetBuild(ctx, b.ID)
			if err == nil && !got.AvailableAt.Equal(got.CreatedAt.Add(48*time.Hour)) {
				t.Errorf("%s: held until %s, arrived %s", b.Source, got.AvailableAt, got.CreatedAt)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
