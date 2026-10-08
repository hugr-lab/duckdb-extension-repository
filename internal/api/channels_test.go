package api_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Every phase-3 route × caller: a channel administrator reaches its channel only, an extension
// administrator its extension's releases only.
func TestChannelAccess(t *testing.T) {
	m := newMgmt(t)
	const C = "/api/v1/tenants/acme/channels"
	channelAdmins := []string{"channel adm", "tenant admin", "server admin"}
	prodKey := m.keyOf(t, "prod")
	tresor12 := m.ids["tresor-1.2"]
	cases := []struct {
		method, path, body string
		ok                 []string
		code               int
	}{
		{"POST", C + "/prod/duckdb-versions", `{"duckdb_version":"v2.0.0"}`, channelAdmins, 409},
		{"GET", C + "/prod/keys/" + prodKey, "", channelAdmins, 200},
		{"POST", C + "/prod/keys/" + prodKey + "/retire", "", channelAdmins, 409},
		{"GET", C + "/prod/extensions/tresor/releases/" + tresor12, "", append([]string{"ext admin"}, channelAdmins...), 200},
		{"POST", C + "/prod/extensions/tresor/releases/" + tresor12 + "/current", "", append([]string{"ext admin"}, channelAdmins...), 200},
		{"POST", C, `{"name":"dev-zed","kind":"signed"}`, []string{"tenant admin", "server admin"}, 201},
		{"GET", C + "/prod/duckdb-versions", "", channelAdmins, 200},
		{"GET", C + "/staging/duckdb-versions", "", []string{"tenant admin", "server admin"}, 200},
		{"GET", C + "/prod/keys", "", channelAdmins, 200},
		{"GET", C + "/prod/keys/events", "", channelAdmins, 200},
		{"GET", C + "/prod/releases", "", channelAdmins, 200},
		{"GET", C + "/prod/extensions/tresor/releases", "", append([]string{"ext admin"}, channelAdmins...), 200},
		{"GET", C + "/prod/extensions/acl/releases", "", channelAdmins, 200},
		{"GET", C + "/staging/extensions/tresor/releases", "", []string{"tenant admin", "server admin"}, 200},
		{"POST", C + "/prod/keys", `{"signer_ref":"file:nope.pem"}`, []string{"server admin"}, 400},
	}
	var ref404 *resp
	for _, tc := range cases {
		for caller, tok := range m.toks {
			r := m.call(t, tc.method, tc.path, tok, strings.ReplaceAll(tc.body, "zed", strings.ReplaceAll(caller, " ", "-")), "If-Match", "*")
			exp := 404
			switch {
			case contains(tc.ok, caller):
				exp = tc.code
			case caller == "anonymous", caller == "other admin":
				exp = 401
			case caller == "stale admin" && contains(tc.ok, "tenant admin"), caller == "stale server":
				exp = 401
			}
			if r.status != exp {
				t.Errorf("%s %s as %s: %d, want %d: %s", tc.method, tc.path, caller, r.status, exp, r.body)
			}
			if r.status == 404 {
				if ref404 == nil {
					ref404 = &r
				} else if !bytes.Equal(r.body, ref404.body) || !sameHeaders(r.header, ref404.header) {
					t.Errorf("%s %s as %s: 404 differs", tc.method, tc.path, caller)
				}
			}
		}
	}
}

// keyOf is a channel's active key id.
func (m *mgmt) keyOf(t *testing.T, channel string) string {
	t.Helper()
	ks, err := m.env.keys.List(ctx, admin, "acme", channel)
	if err != nil || len(ks) == 0 {
		t.Fatalf("keys of %s: %v", channel, err)
	}
	return ks[0].ID
}

// Ids are looked up within the path's channel: another channel's key or release is not found,
// whoever asks.
func TestOtherChannelIDs(t *testing.T) {
	m := newMgmt(t)
	const C = "/api/v1/tenants/acme/channels/prod"
	stagingKey, stagingACL := m.keyOf(t, "staging"), m.ids["acl-staging"]
	for _, tok := range []string{m.toks["channel adm"], m.toks["server admin"]} {
		for _, c := range []struct{ method, path string }{
			{"GET", C + "/keys/" + stagingKey},
			{"POST", C + "/keys/" + stagingKey + "/activate"},
			{"GET", C + "/extensions/acl/releases/" + stagingACL},
			{"POST", C + "/extensions/acl/releases/" + stagingACL + "/yank"},
		} {
			if r := m.call(t, c.method, c.path, tok, "", "If-Match", "*"); r.status != 404 {
				t.Errorf("%s %s: %d", c.method, c.path, r.status)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestReleaseChanges(t *testing.T) {
	m := newMgmt(t)
	const E = "/api/v1/tenants/acme/channels/prod/extensions/"
	eve := m.toks["ext admin"]

	// an extension administrator cannot reach another extension's release by id
	acl := m.ids["acl"]
	if r := m.call(t, "GET", E+"tresor/releases/"+acl, eve, ""); r.status != 404 {
		t.Errorf("another extension's id: %d", r.status)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+acl+"/yank", eve, "", "If-Match", "*"); r.status != 404 {
		t.Errorf("yanking another extension's release: %d", r.status)
	}
	// even the channel administrator's path must name the release's own extension
	if r := m.call(t, "POST", E+"tresor/releases/"+acl+"/yank", m.toks["channel adm"], "", "If-Match", "*"); r.status != 404 {
		t.Errorf("a release under another extension's path: %d", r.status)
	}

	id := m.ids["tresor-1.0"]
	r := m.call(t, "GET", E+"tresor/releases/"+id, eve, "")
	if r.status != 200 || r.json(t)["state"] != "active" || r.header.Get("ETag") == "" {
		t.Fatalf("get release: %d %s", r.status, r.body)
	}
	tag := r.header.Get("ETag")
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/deprecate", eve, ""); r.status != 428 {
		t.Errorf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/deprecate", eve, "", "If-Match", `"v99"`); r.status != 412 {
		t.Errorf("stale If-Match: %d", r.status)
	}
	r = m.call(t, "POST", E+"tresor/releases/"+id+"/deprecate", eve, "", "If-Match", tag)
	if r.status != 200 || r.json(t)["state"] != "deprecated" || r.header.Get("ETag") == tag {
		t.Fatalf("deprecate: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/deprecate", eve, "", "If-Match", "*"); r.status != 409 {
		t.Errorf("deprecating a deprecated release: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/explode", eve, "", "If-Match", "*"); r.status != 404 {
		t.Errorf("an unknown change: %d", r.status)
	}
	// the index (resolved like the DuckDB routes) sees the change at once
	if r := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/extensions/tresor/versions/1.0?duckdb_version=v2.0.0&platform=linux_amd64", "", ""); r.json(t)["status"] != "deprecated" {
		t.Errorf("the index after deprecate: %s", r.body)
	}

	// lists: filter by state, page oldest first without repeats
	if got := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/releases?state=yanked", m.toks["channel adm"], "").json(t)["releases"].([]any); len(got) != 1 {
		t.Errorf("yanked releases: %v", got)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/releases?state=gone", m.toks["channel adm"], ""); r.status != 400 {
		t.Errorf("a bad state filter: %d", r.status)
	}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for ; pages < 20; pages++ {
		pg := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/releases?limit=2&cursor="+cursor, m.toks["channel adm"], "").json(t)
		for _, x := range pg["releases"].([]any) {
			id := x.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("release %s on two pages", id)
			}
			seen[id] = true
		}
		next, _ := pg["next"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Errorf("paged %d releases, want 7", len(seen))
	}
}

func TestKeyManagement(t *testing.T) {
	m := newMgmt(t)
	const K = "/api/v1/tenants/acme/channels/prod/keys"
	sa, carl := m.toks["server admin"], m.toks["channel adm"]
	if err := signer.GenerateKeyFile(filepath.Join(m.dir, "b.pem")); err != nil {
		t.Fatal(err)
	}

	v := m.call(t, "GET", K, carl, "").json(t)
	if v["active_key"] == nil || v["active_key"] != v["serving_key"] || v["unsigned_by_active"].(float64) != 0 || v["resigner"] != nil {
		t.Fatalf("key view: %v", v)
	}
	if k := v["keys"].([]any)[0].(map[string]any); k["signer_ref"] != nil {
		t.Errorf("a channel administrator sees the signer reference: %v", k)
	}

	// a server administrator registers a key by signer reference
	r := m.call(t, "POST", K, sa, `{"signer_ref":"file:b.pem"}`)
	if r.status != 201 || r.json(t)["state"] != "trusted" || r.json(t)["signer_ref"] != "file:b.pem" {
		t.Fatalf("add key: %d %s", r.status, r.body)
	}
	kb, tag := r.json(t)["id"].(string), r.header.Get("ETag")
	if r := m.call(t, "POST", K, sa, `{"signer_ref":"file:b.pem"}`); r.status != 409 {
		t.Errorf("the same key twice: %d %s", r.status, r.body)
	}

	// a CLI-made record shows no OS user to a channel administrator
	if k := v["keys"].([]any)[0].(map[string]any); k["created_by"] != "server" {
		t.Errorf("created_by for a channel administrator: %v", k["created_by"])
	}

	// activation: force is the server's, decided before anything else; If-Match; too soon
	m.keySvc.MinTrusted = time.Hour
	for _, tok := range []string{carl, m.toks["tenant admin"]} {
		if r := m.call(t, "POST", K+"/"+kb+"/activate?force=true", tok, ""); r.status != 404 {
			t.Errorf("force without If-Match by a tenant principal: %d", r.status)
		}
	}
	if r := m.call(t, "POST", K+"/"+kb+"/activate", carl, "", "If-Match", `"v99"`); r.status != 412 {
		t.Errorf("activate with a stale If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", K+"/"+kb+"/retire", carl, "", "If-Match", `"v99"`); r.status != 412 {
		t.Errorf("retire with a stale If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", K+"/"+v["serving_key"].(string)+"/retire", carl, "", "If-Match", "*"); r.status != 409 {
		t.Errorf("retiring the active key: %d", r.status)
	}
	if r := m.call(t, "POST", K+"/"+kb+"/activate", carl, ""); r.status != 428 {
		t.Errorf("activate without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", K+"/"+kb+"/activate", carl, "", "If-Match", tag); r.status != 409 || !strings.Contains(string(r.body), "too soon") ||
		strings.Contains(string(r.body), "force") {
		t.Errorf("activate too soon: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", K+"/"+kb+"/activate?force=true", carl, "", "If-Match", tag); r.status != 404 {
		t.Errorf("force by a channel administrator: %d", r.status)
	}
	r = m.call(t, "POST", K+"/"+kb+"/activate?force=true", sa, "", "If-Match", tag)
	if r.status != 200 || r.json(t)["state"] != "active" {
		t.Fatalf("forced activation: %d %s", r.status, r.body)
	}
	// the releases now wait for the new key's signatures; the serving key stays until a re-sign
	v = m.call(t, "GET", K, carl, "").json(t)
	if v["active_key"] != kb || v["serving_key"] == kb || v["unsigned_by_active"].(float64) != 6 {
		t.Errorf("after activation: %v", v)
	}
	// a re-signer's lease shows when it last held the channel; its host only to server administrators
	sc, err := m.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m.st.AcquireLease(ctx, "resign/"+sc.ID, "host-1/x", time.Minute); err != nil || !ok {
		t.Fatal(err)
	}
	if rs := m.call(t, "GET", K, carl, "").json(t)["resigner"].(map[string]any); rs["working"] != true || rs["holder"] != nil {
		t.Errorf("resigner for a channel administrator: %v", rs)
	}
	if err := m.st.ReleaseLease(ctx, "resign/"+sc.ID, "host-1/x"); err != nil {
		t.Fatal(err)
	}
	if rs := m.call(t, "GET", K, sa, "").json(t)["resigner"].(map[string]any); rs["working"] != false || rs["holder"] != "host-1/x" ||
		rs["last_held_at"] == "" {
		t.Errorf("resigner for a server administrator: %v", rs)
	}
	// events page by (at, id)
	evs := m.call(t, "GET", K+"/events?limit=1", carl, "").json(t)
	if len(evs["events"].([]any)) != 1 || evs["next"] == nil {
		t.Errorf("events: %v", evs)
	}
	if r := m.call(t, "GET", K+"/"+kb, carl, ""); r.status != 200 || r.header.Get("ETag") == tag {
		t.Errorf("get key: %d %v", r.status, r.header)
	}
	if r := m.call(t, "GET", K+"/nope", carl, ""); r.status != 404 {
		t.Errorf("an unknown key: %d", r.status)
	}

	// the serving key, though trusted now, cannot be retired before a re-sign
	if r := m.call(t, "POST", K+"/"+v["serving_key"].(string)+"/retire?force=true", sa, "", "If-Match", "*"); r.status != 409 {
		t.Errorf("retiring the serving key: %d %s", r.status, r.body)
	}

	// channels: create (Location answers), a duplicate is 409
	r = m.call(t, "POST", "/api/v1/tenants/acme/channels", m.toks["tenant admin"], `{"name":"beta","kind":"signed"}`)
	if r.status != 201 || r.header.Get("ETag") != "" {
		t.Fatalf("create channel: %d %v", r.status, r.header)
	}
	if g := m.call(t, "GET", r.header.Get("Location"), "", ""); g.status != 200 || g.json(t)["kind"] != "signed" {
		t.Errorf("the created channel: %d %s", g.status, g.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/channels", m.toks["tenant admin"], `{"name":"beta","kind":"signed"}`); r.status != 409 {
		t.Errorf("a duplicate channel: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/acme/channels/nochan/duckdb-versions", m.toks["tenant admin"], `{"duckdb_version":"v2.0.0"}`); r.status != 404 {
		t.Errorf("versions of a missing channel: %d", r.status)
	}

	// channel DuckDB versions: add, a missing one is 400, remove, removing again is 404
	if _, err := m.env.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6"}); err != nil {
		t.Fatal(err)
	}
	const V = "/api/v1/tenants/acme/channels/prod/duckdb-versions"
	if got := m.call(t, "POST", V, carl, `{"duckdb_version":"v2.1.0"}`).json(t)["duckdb_versions"].([]any); len(got) != 3 || got[2] != "v2.1.0" {
		t.Errorf("after add: %v", got)
	}
	if r := m.call(t, "POST", V, carl, `{"duckdb_version":"v9.9.9"}`); r.status != 400 {
		t.Errorf("an unknown version: %d", r.status)
	}
	if r := m.call(t, "DELETE", V+"/v2.1.0", carl, ""); r.status != 200 {
		t.Errorf("remove: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", V+"/v2.1.0", carl, ""); r.status != 404 {
		t.Errorf("remove again: %d", r.status)
	}
}
