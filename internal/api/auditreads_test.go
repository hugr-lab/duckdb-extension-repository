package api_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Spec 0040: a tenant-wide auditor reads the tenant's structure through the administrators' routes,
// with their answers, and writes nothing; nobody else gains a read.
func TestAuditReads(t *testing.T) {
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
	tok := func(sub string, claims ...any) string {
		c := map[string]any{"sub": sub, "aud": publicURL + "/acme"}
		for i := 0; i+1 < len(claims); i += 2 {
			c[claims[i].(string)] = claims[i+1]
		}
		return m.keys.sign(t, idpURL, c)
	}
	call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|aud","verbs":["audit"]}`)
	aud := tok("aud")

	// the structure: groups (one with a member and its administrator), the tenant's publisher and a
	// distributor's with keys, an upstream
	call(201, "POST", T+"/extension-groups", ta, `{"name":"platform"}`)
	call(201, "POST", T+"/extension-groups", ta, `{"name":"tools"}`)
	call(200, "POST", T+"/extension-groups/platform/members", ta, `{"add":["acl"]}`)
	gid := call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|gina","verbs":["admin"],"extension_group":"platform"}`).json(t)["id"].(string)
	call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`)
	kid := call(201, "POST", T+"/publishers/own-ci/keys", ta, `{"public_key":"`+newSigner(t).authKey+`"}`).json(t)["id"].(string)
	call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`)
	call(201, "POST", T+"/publishers/enterest-ci/keys", da, `{"public_key":"`+newSigner(t).authKey+`"}`)
	call(201, "POST", T+"/upstreams", ta, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"tresor"}]}`)

	// the issuer's id is the one a principal's created_by names; its console client, once set
	is := call(200, "GET", T+"/issuers/corp", aud, "")
	id, _ := is.json(t)["id"].(string)
	if id == "" || is.json(t)["console"] != nil {
		t.Fatalf("the issuer before a console client: %s", is.body)
	}
	if g := call(200, "GET", T+"/grants/"+gid, aud, "").json(t); g["created_by"] != "principal:acme/"+id+"|tom" {
		t.Fatalf("the grant's created_by names the issuer's id %s: %v", id, g)
	}
	call(200, "POST", T+"/issuers/corp/console", ta, `{"client_id":"kista-console","scopes":["openid"]}`, "If-Match", is.header.Get("ETag"))
	con, _ := call(200, "GET", T+"/issuers/corp", aud, "").json(t)["console"].(map[string]any)
	if con["client_id"] != "kista-console" || con["created_by"] != "principal:acme/"+id+"|tom" || con["created_at"] == "" ||
		con["audience_parameter"] != "" || con["audience"] != "" {
		t.Fatalf("the console client: %v", con)
	}

	// the administrators' answers, byte for byte, the group list's full view and ?extension= included
	reads := []string{T + "/issuers", T + "/issuers/corp", T + "/audiences", T + "/grants", T + "/grants?extension=acl",
		T + "/grants/" + gid, T + "/extension-groups", T + "/extension-groups?extension=acl", T + "/extension-groups/platform",
		T + "/extension-groups/platform/members", T + "/publishers", T + "/publishers/own-ci", T + "/publishers/enterest-ci",
		T + "/publishers/own-ci/keys", T + "/publishers/own-ci/keys/" + kid, T + "/upstreams", T + "/upstreams/core"}
	for _, p := range reads {
		a, b := call(200, "GET", p, aud, ""), call(200, "GET", p, ta, "")
		if !bytes.Equal(a.body, b.body) || a.header.Get("ETag") != b.header.Get("ETag") {
			t.Errorf("%s: the auditor's answer differs:\n%s\n%s", p, a.body, b.body)
		}
	}
	if g := call(200, "GET", T+"/extension-groups", aud, "").json(t)["extension_groups"].([]any); len(g) != 2 || g[0].(map[string]any)["grants"] == nil {
		t.Errorf("the full view: %v", g)
	}
	if p := call(200, "GET", T+"/publishers/enterest-ci", aud, "").json(t); p["created_by"] != "distribution-admin" ||
		len(p["keys"].([]any)) != 1 || p["distributor"] != "enterest" {
		t.Errorf("a distributor's publisher: %v", p)
	}
	if strings.Contains(string(call(200, "GET", T+"/upstreams/core", aud, "").body), "token") {
		t.Error("an upstream's answer names a token")
	}

	// every write, the grants' principals and an upstream's sub-routes: 404
	for _, c := range []struct{ method, path, body string }{
		{"POST", T + "/issuers", `{"name":"x","url":"https://x.example"}`},
		{"DELETE", T + "/issuers/corp", ""},
		{"POST", T + "/issuers/corp/console", `{"client_id":"x"}`},
		{"DELETE", T + "/issuers/corp/console", ""},
		{"POST", T + "/audiences", `{"audience":"api://x"}`},
		{"POST", T + "/grants", `{"principal":"subject:corp|zed","verbs":["install"]}`},
		{"DELETE", T + "/grants/" + gid, ""},
		{"POST", T + "/extension-groups", `{"name":"x"}`},
		{"DELETE", T + "/extension-groups/tools", ""},
		{"POST", T + "/extension-groups/platform/description", `{"description":"x"}`},
		{"POST", T + "/extension-groups/platform/members", `{"add":["x"]}`},
		{"POST", T + "/publishers", `{"name":"x"}`},
		{"DELETE", T + "/publishers/own-ci", ""},
		{"POST", T + "/publishers/own-ci/keys", `{"public_key":"` + newSigner(t).authKey + `"}`},
		{"DELETE", T + "/publishers/own-ci/keys/" + kid, ""},
		{"PUT", T + "/publishers/own-ci/hold", `{"hold":"1h"}`},
		{"POST", T + "/upstreams", `{"name":"x","kind":"duckdb-core","channel":"prod","platforms":["linux_amd64"]}`},
		{"DELETE", T + "/upstreams/core", ""},
		{"POST", T + "/upstreams/core/pause", ""},
		{"GET", T + "/grants/principals", ""},
		{"GET", T + "/upstreams/core/cells", ""},
		{"GET", T + "/upstreams/core/usage", ""},
		{"GET", T + "/upstreams/core/revocations", ""},
		{"GET", T + "/upstreams/core/names", ""},
		{"GET", T + "/upstreams/core/licenses", ""},
		{"GET", T + "/upstreams/core/extensions", ""},
		{"GET", T + "/upstreams/core/platforms", ""},
		{"GET", T + "/upstreams/core/keys", ""},
	} {
		call(404, c.method, c.path, aud, c.body, "If-Match", "*")
	}

	// ?type= narrows for an auditor as for an administrator
	unregister := pkgtype.RegisterForTest("test_package")
	defer func() { unregister() }()
	call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|dan","verbs":["admin"],"package_type":"test_package"}`)
	for _, p := range []string{T + "/grants?type=test_package", T + "/extension-groups?type=test_package", T + "/publishers?type=test_package"} {
		a, b := m.call(t, "GET", p, aud, ""), m.call(t, "GET", p, ta, "")
		if a.status != b.status || !bytes.Equal(a.body, b.body) {
			t.Errorf("%s: %d %s, an administrator's %d %s", p, a.status, a.body, b.status, b.body)
		}
	}

	// refused: an administrator of one type, a group's administrator (but its own group), another
	// tenant's auditor, a server token without a role, an installer, a licence manager; a stale token
	call(201, "POST", "/api/v1/tenants/other/grants", m.toks["other admin"], `{"principal":"subject:corp|oscar","verbs":["audit"]}`)
	gina := tok("gina")
	own := map[string]bool{T + "/extension-groups": true, T + "/extension-groups/platform": true, T + "/extension-groups/platform/members": true}
	for who, c := range map[string]struct {
		tok  string
		want int
	}{
		"type admin":            {tok("dan"), 404},
		"group admin":           {gina, 404},
		"other's auditor":       {tok("oscar"), 404}, // no grant in acme
		"other's auditor token": {m.keys.sign(t, otherIdPURL, map[string]any{"sub": "oscar", "aud": publicURL + "/other"}), 401},
		"server user":           {m.toks["server user"], 404},
		"install":               {m.toks["install"], 404},
		"licence manager":       {m.toks["server licence mgr"], 404},
		"stale auditor":         {tok("aud", "iat", time.Now().Add(-2*time.Hour).Unix()), 401},
	} {
		for _, p := range reads {
			want := c.want
			if who == "group admin" && own[p] {
				want = 200
			}
			if r := m.call(t, "GET", p, c.tok, ""); r.status != want {
				t.Errorf("%s on %s: %d, want %d", who, p, r.status, want)
			}
		}
	}
	// the group's administrator keeps its narrow view: its group, without grants, no ?extension=
	if g := call(200, "GET", T+"/extension-groups", gina, "").json(t)["extension_groups"].([]any); len(g) != 1 || g[0].(map[string]any)["grants"] != nil {
		t.Errorf("a group administrator's list: %v", g)
	}

	// a distribution admin: the distributors' publishers and keys only
	pubs := call(200, "GET", T+"/publishers", da, "").json(t)["publishers"].([]any)
	if len(pubs) != 1 || pubs[0].(map[string]any)["name"] != "enterest-ci" {
		t.Errorf("a distribution admin's publishers: %v", pubs)
	}
	call(200, "GET", T+"/publishers/enterest-ci", da, "")
	call(200, "GET", T+"/publishers/enterest-ci/keys", da, "")
	for _, p := range reads {
		if !strings.Contains(p, "/publishers") || strings.Contains(p, "own-ci") {
			call(404, "GET", p, da, "")
		}
	}
	// a suspended tenant's publishers: nobody's but a server administrator's
	call(200, "POST", T+"/suspend", sa, "", "If-Match", call(200, "GET", T, sa, "").header.Get("ETag"))
	for _, tk := range []string{da, aud} {
		call(404, "GET", T+"/publishers", tk, "")
		call(404, "GET", T+"/publishers/enterest-ci", tk, "")
	}

	// the console client removed: absent again
	call(200, "POST", T+"/resume", sa, "", "If-Match", call(200, "GET", T, sa, "").header.Get("ETag"))
	call(204, "DELETE", T+"/issuers/corp/console", ta, "", "If-Match", `"`+id+`"`)
	if c := call(200, "GET", T+"/issuers/corp", aud, "").json(t)["console"]; c != nil {
		t.Errorf("a removed console client: %v", c)
	}
}
