package api_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// licenceFor is a licence of tenant's for tresor.
func (m *mgmt) licenceFor(tenant, plan string) string {
	now := time.Now().Unix()
	return vectors.Sign(m.licKey, vectors.Header(m.licKey), fmt.Sprintf(
		`{"v":1,"iss":"https://issuer.example","jti":"lic-%s-%s","sub":%q,"ext":"tresor","plan":%q,"limits":{"sources":5},"iat":%d,"exp":%d}`,
		tenant, plan, tenant, plan, now-60, now+86400))
}

// Spec 0029: a licence manager manages the licences of every tenant and does nothing else.
func TestLicenseManager(t *testing.T) {
	m := newMgmt(t)
	lm := m.toks["server licence mgr"]
	const lmActor = "license-manager:ops|reconciler"

	// tenant other gets a live build of tresor too; each tenant a bundle-mode upstream (the relay's
	// routes answer its administrators, not 404)
	if _, err := m.env.ten.CreateChannel(ctx, admin, "other", "prod", store.ChannelSigned, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.env.ten.SetChannelVersions(ctx, admin, "other", "prod", []string{"v2.0.0"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(m.dir, "other.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.env.keys.Add(ctx, admin, "other", "prod", "file:other.pem", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.rel.Add(ctx, admin, "other", "prod", bytes.NewReader(ext(t, 21, cpp("1.0"))),
		release.AddOptions{Unchecked: true, Name: "tresor"}); err != nil {
		t.Fatal(err)
	}
	admins := map[string]string{"acme": m.toks["tenant admin"], "other": m.toks["other admin"]}
	for tn := range admins {
		if _, err := m.ups.Add(ctx, admin, tn, upstream.Spec{Name: "air", Kind: store.UpstreamCommunity, Channel: "prod",
			Mode: store.ModeBundle, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "x"}}}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tn := range []string{"acme", "other"} {
		tenant, err := m.st.GetTenant(ctx, tn)
		if err != nil {
			t.Fatal(err)
		}
		kid, err := storetest.LicenseKey(ctx, m.st, tenant.ID, "tresor", store.KeySourcePublisher, "nobody", &m.licKey.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		T := "/api/v1/tenants/" + tn

		// every licence route
		for _, p := range []string{T + "/license-keys", T + "/license-keys/tresor"} {
			if r := m.call(t, "GET", p, lm, ""); r.status != 200 || !strings.Contains(string(r.body), kid) {
				t.Fatalf("%s: %d %s", p, r.status, r.body)
			}
		}
		r := m.call(t, "POST", T+"/licenses", lm, m.licenceFor(tn, "pro"))
		if r.status != 201 || r.json(t)["plan"] != "pro" {
			t.Fatalf("%s: an import: %d %s", tn, r.status, r.body)
		}
		r = m.call(t, "POST", T+"/licenses", lm, m.licenceFor(tn, "max"), "If-Match", r.header.Get("ETag"))
		if r.status != 200 || r.json(t)["plan"] != "max" {
			t.Fatalf("%s: a replacement: %d %s", tn, r.status, r.body)
		}
		if got := names(m.call(t, "GET", T+"/licenses", lm, "").json(t), "licenses", "name"); !slices.Equal(got, []string{"tresor"}) {
			t.Fatalf("%s: list: %v", tn, got)
		}
		if r := m.call(t, "HEAD", T+"/licenses/tresor", lm, ""); r.status != 200 {
			t.Fatalf("%s: head: %d", tn, r.status)
		}
		r = m.call(t, "GET", T+"/licenses/tresor", lm, "")
		if r.status != 200 || r.json(t)["plan"] != "max" {
			t.Fatalf("%s: get: %d %s", tn, r.status, r.body)
		}
		// public is the tenant administrators' alone (its token is then served to anyone); a licence
		// manager restores private
		if r := m.call(t, "POST", T+"/licenses/tresor/public", lm, "", "If-Match", r.header.Get("ETag")); r.status != 404 {
			t.Fatalf("%s: public by a licence manager: %d %s", tn, r.status, r.body)
		}
		r = m.call(t, "POST", T+"/licenses/tresor/public", admins[tn], "", "If-Match", r.header.Get("ETag"))
		if r.status != 200 || r.json(t)["visibility"] != "public" {
			t.Fatalf("%s: public by the tenant administrator: %d %s", tn, r.status, r.body)
		}
		r = m.call(t, "POST", T+"/licenses/tresor/private", lm, "", "If-Match", r.header.Get("ETag"))
		if r.status != 200 || r.json(t)["visibility"] != "private" {
			t.Fatalf("%s: private: %d %s", tn, r.status, r.body)
		}
		if r := m.call(t, "DELETE", T+"/licenses/tresor", lm, "", "If-Match", r.header.Get("ETag")); r.status != 204 {
			t.Fatalf("%s: delete: %d %s", tn, r.status, r.body)
		}
		// its acts carry its actor
		evID := ""
		for _, kind := range []string{"license.import", "license.change", "license.remove"} {
			evs, err := m.st.ListEvents(ctx, tenant.ID, store.EventFilter{Kind: kind, Limit: 10})
			if err != nil || len(evs) == 0 || evs[0].Actor != lmActor {
				t.Fatalf("%s: %s events: %v %+v", tn, kind, err, evs)
			}
			evID = evs[0].ID
		}

		// nothing else: the upstreams' relay, the tenant's other routes, the server's (404 is the
		// API's forbidden); the tenant's administrators and the server administrator still reach
		// the relay
		for _, c := range []struct{ method, path string }{
			{"GET", T + "/upstreams/air/licenses"}, {"POST", T + "/upstreams/air/licenses/sync"},
		} {
			if r := m.call(t, c.method, c.path, lm, ""); r.status != 404 {
				t.Errorf("%s %s as a licence manager: %d", c.method, c.path, r.status)
			}
			for who, tok := range map[string]string{"tenant admin": admins[tn], "server admin": m.toks["server admin"]} {
				if r := m.call(t, c.method, c.path, tok, ""); r.status == 404 || r.status == 401 {
					t.Errorf("%s %s as %s: %d", c.method, c.path, who, r.status)
				}
			}
		}
		for _, c := range []struct{ method, path, body string }{
			{"GET", T, ""}, {"HEAD", T, ""}, {"GET", T + "/grants", ""}, {"POST", T + "/grants", `{"principal":"subject:corp|x","verbs":["admin"]}`},
			{"GET", T + "/issuers", ""}, {"GET", T + "/audiences", ""}, {"GET", T + "/publishers", ""},
			{"POST", T + "/publishers", `{"name":"p1"}`}, {"POST", T + "/publishers/nobody/license-keys", ""},
			{"DELETE", T + "/publishers/nobody/license-keys", ""}, {"GET", T + "/blocks", ""}, {"GET", T + "/upstreams", ""},
			{"GET", T + "/upstreams/air", ""}, {"POST", T + "/upstreams/air/sync", ""}, {"GET", T + "/events", ""},
			{"GET", T + "/channels/prod/keys", ""}, {"GET", T + "/channels/prod/releases", ""},
			{"GET", T + "/channels/prod/extensions/tresor/releases", ""}, {"GET", T + "/extension-groups", ""},
			{"GET", T + "/stats/usage", ""}, {"GET", T + "/feeds", ""}, {"GET", T + "/shadows", ""},
			{"POST", T + "/suspend", ""}, {"POST", T + "/resume", ""}, {"DELETE", T, ""}, {"GET", T + "/bundle-uploads", ""},
			{"POST", T + "/channels/prod/keys", `{"signer_ref":"file:x.pem"}`},
			{"GET", T + "/upstreams/air/usage", ""}, {"POST", T + "/upstreams/air/usage", `{"usage":"off"}`},
			{"GET", T + "/upstreams/air/cells", ""}, {"GET", T + "/whoami", ""}, {"GET", T + "/events/" + evID, ""},
			{"POST", T + "/audiences", `{"audience":"api://x"}`}, {"POST", T + "/audiences/remove", `{"audience":"api://x"}`},
			{"POST", T + "/channels/prod/extensions/tresor/releases", "x"},
			{"POST", T + "/channels/prod/extensions/tresor/releases/promote", `{"id":"x"}`},
			{"GET", T + "/extensions/tresor/attachments", ""}, {"POST", T + "/extensions/tresor/attachments", "x"},
			{"POST", T + "/bundle-uploads", `{"size":1}`}, {"GET", T + "/bundle-imports", ""},
			{"GET", T + "/attestation-keys/x", ""}, {"POST", T + "/feed", `{}`},
		} {
			if r := m.call(t, c.method, c.path, lm, c.body, "If-Match", "*"); r.status != 404 {
				t.Errorf("%s %s as a licence manager: %d %s", c.method, c.path, r.status, r.body)
			}
		}
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tenants", ""}, {"POST", "/api/v1/tenants", `{"name":"x1"}`}, {"GET", "/api/v1/events", ""},
		{"GET", "/api/v1/credentials", ""}, {"POST", "/api/v1/duckdb-versions", `{"name":"v2.1.0","kind":"release","c_api_maxima":["v1.5.6"],"core_extensions":["json"]}`},
		{"POST", "/api/v1/duckdb-versions/v2.0.0/c-apis", `{"c_api":"v3.0.0"}`},
		{"GET", "/api/v1/events/00000000-0000-7000-8000-000000000000", ""},
	} {
		if r := m.call(t, c.method, c.path, lm, c.body); r.status != 404 {
			t.Errorf("%s %s as a licence manager: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	m.events.Close(ctx)
	acme, _ := m.st.GetTenant(ctx, "acme")
	if evs, _ := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "authz.refused", Actor: lmActor, Limit: 100}); len(evs) == 0 { // one a second per actor
		t.Errorf("its refusals recorded under its actor: %d", len(evs))
	}

	// a tenant's readers see the kind, not who (as for a server administrator); the server
	// administrator sees who
	for tok, want := range map[string]string{m.toks["tenant admin"]: "license-manager", m.toks["server admin"]: lmActor} {
		r := m.call(t, "GET", "/api/v1/tenants/acme/events?kind=license.import", tok, "")
		evs, _ := r.json(t)["events"].([]any)
		if r.status != 200 || len(evs) == 0 || evs[0].(map[string]any)["actor"] != want {
			t.Errorf("license.import's actor, want %s: %d %s", want, r.status, r.body)
		}
	}

	// management rules as for everyone: a fresh token
	stale := m.keys.sign(t, opsURL, map[string]any{"sub": "reconciler", "aud": serverAud, "tid": "t1", "roles": []any{"kista.licenses"},
		"iat": time.Now().Add(-2 * time.Hour).Unix()})
	if r := m.call(t, "GET", "/api/v1/tenants/acme/licenses", stale, ""); r.status != 401 {
		t.Errorf("a stale licence manager's token: %d", r.status)
	}

	// whoami; a principal in both lists is a server administrator
	both := m.keys.sign(t, opsURL, map[string]any{"sub": "root", "aud": serverAud, "tid": "t1", "roles": []any{"kista.admin", "kista.licenses"}})
	for who, want := range map[string][2]bool{"server licence mgr": {false, true}, "server admin": {true, false}, "server user": {false, false},
		"both": {true, true}} {
		tok := m.toks[who]
		if who == "both" {
			tok = both
		}
		r := m.call(t, "GET", "/api/v1/whoami", tok, "")
		if j := r.json(t); r.status != 200 || j["administrator"] != want[0] || j["license_manager"] != want[1] {
			t.Errorf("whoami as %s: %d %s", who, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", "/api/v1/tenants", both, ""); r.status != 200 {
		t.Errorf("an administrator who is a licence manager too: %d", r.status)
	}

	// the role is configuration only: no grant gives license, and a tenant record at the server
	// issuer's URL, with the role the configuration names, makes no licence manager
	if r := m.call(t, "POST", "/api/v1/tenants/acme/grants", m.toks["tenant admin"], `{"principal":"subject:corp|x","verbs":["license"]}`); r.status != 400 {
		t.Errorf("a grant of license: %d %s", r.status, r.body)
	}
	if _, err := m.env.adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "ops", URL: opsURL,
		RequiredClaims: map[string]string{"tid": "t1"}, RolesClaim: []string{"roles"}}); err != nil {
		t.Fatal(err)
	}
	lookalike := m.keys.sign(t, opsURL, map[string]any{"sub": "reconciler", "aud": publicURL + "/acme", "tid": "t1", "roles": []any{"kista.licenses"}})
	if r := m.call(t, "GET", "/api/v1/tenants/acme/licenses", lookalike, ""); r.status != 404 {
		t.Errorf("a tenant token with the licence managers' role: %d", r.status)
	}

	// a suspended tenant is unknown to a licence manager, as to its administrators; the server
	// administrator still reaches it
	tag := m.call(t, "GET", "/api/v1/tenants/other", m.toks["server admin"], "").header.Get("ETag")
	if r := m.call(t, "POST", "/api/v1/tenants/other/suspend", m.toks["server admin"], "", "If-Match", tag); r.status != 200 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/other/licenses", lm, ""); r.status != 404 {
		t.Errorf("a suspended tenant's licences to a licence manager: %d", r.status)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/other/licenses", m.toks["server admin"], ""); r.status != 200 {
		t.Errorf("a suspended tenant's licences to the server administrator: %d", r.status)
	}
	// a tenant whose deletion started (spec 0019) too, with the unknown tenant's bytes
	tag = m.call(t, "GET", "/api/v1/tenants/other", m.toks["server admin"], "").header.Get("ETag")
	if r := m.call(t, "DELETE", "/api/v1/tenants/other", m.toks["server admin"], "", "If-Match", tag); r.status >= 300 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	unknown := m.call(t, "GET", "/api/v1/tenants/nope/licenses", lm, "")
	if unknown.status != 404 {
		t.Errorf("an unknown tenant: %d", unknown.status)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tenants/other/licenses", ""}, {"POST", "/api/v1/tenants/other/licenses", m.licenceFor("other", "pro")},
		{"GET", "/api/v1/tenants/other/license-keys", ""},
	} {
		if r := m.call(t, c.method, c.path, lm, c.body); r.status != 404 || !bytes.Equal(r.body, unknown.body) {
			t.Errorf("%s %s of a deleting tenant to a licence manager: %d %s", c.method, c.path, r.status, r.body)
		}
	}

	// a write needs a named writer, a licence manager's too
	unnamed := m.keys.sign(t, opsURL, map[string]any{"aud": serverAud, "tid": "t1", "roles": []any{"kista.licenses"}, "sub": nil})
	if r := m.call(t, "POST", "/api/v1/tenants/acme/licenses", unnamed, m.licenceFor("acme", "pro")); r.status != 401 {
		t.Errorf("an unnamed licence manager's import: %d %s", r.status, r.body)
	}
	// licensing off: an import is refused as for anyone (409)
	m.lic.Enabled = false
	r := m.call(t, "POST", "/api/v1/tenants/acme/licenses", lm, m.licenceFor("acme", "pro"))
	m.lic.Enabled = true
	if r.status != 409 || !strings.Contains(string(r.body), "licensing is off") {
		t.Errorf("an import with licensing off: %d %s", r.status, r.body)
	}
}
