package api_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Extension groups over the API (spec 0017): who reads and changes them, what a grant on a group
// gives in the index and on release routes, the grant filters.
func TestExtensionGroupRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	cli := m.env.adm
	if _, err := cli.CreateExtensionGroup(ctx, admin, "acme", "platform", "hugr"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.CreateExtensionGroup(ctx, admin, "acme", "other", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.ChangeExtensionGroupMembers(ctx, admin, "acme", "platform", []string{"acl"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ sub, channel, verb string }{{"gina", "", "admin"}, {"ina", "prod", "install"}} {
		if _, err := cli.AddGrant(ctx, admin, "acme", "subject:corp|"+g.sub, []string{g.verb},
			tenants.GrantOn{Channel: g.channel, ExtensionGroup: "platform"}); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(sub string) string {
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	gina, ina, tom, nobody := tok("gina"), tok("ina"), m.toks["tenant admin"], m.toks["no grants"]

	// the index: install through the group, in its channel only
	if got := names(m.call(t, "GET", T+"/channels/prod/extensions", ina, "").json(t), "extensions", "name"); !slices.Contains(got, "acl") {
		t.Errorf("ina's prod index: %v", got)
	}
	for _, c := range []struct{ tok, ch string }{{ina, "staging"}, {nobody, "prod"}} {
		if got := names(m.call(t, "GET", T+"/channels/"+c.ch+"/extensions", c.tok, "").json(t), "extensions", "name"); slices.Contains(got, "acl") {
			t.Errorf("%s index shows acl: %v", c.ch, got)
		}
	}
	// admin through the group: the member's releases, not another extension's
	if r := m.call(t, "GET", T+"/channels/prod/extensions/acl/releases", gina, ""); r.status != 200 {
		t.Errorf("gina on acl's releases: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/channels/prod/extensions/tresor/releases", gina, ""); r.status != 404 {
		t.Errorf("gina on tresor's releases: %d", r.status)
	}
	// whoami: the grant with its group; holds_admin for a group's administrator
	if w := m.call(t, "GET", T+"/whoami", gina, "").json(t); w["holds_admin"] != true ||
		w["grants"].([]any)[0].(map[string]any)["extension_group"] != "platform" {
		t.Errorf("gina's whoami: %v", w)
	}

	// reading groups: a tenant administrator every group with grant counts; a group's administrator
	// its own without them; others 404, as for a group that does not exist
	list := m.call(t, "GET", T+"/extension-groups", tom, "").json(t)
	if got := names(list, "extension_groups", "name"); !slices.Equal(got, []string{"other", "platform"}) ||
		list["extension_groups"].([]any)[1].(map[string]any)["grants"] != float64(2) {
		t.Errorf("tom's groups: %v", list)
	}
	list = m.call(t, "GET", T+"/extension-groups", gina, "").json(t)
	if got := names(list, "extension_groups", "name"); !slices.Equal(got, []string{"platform"}) ||
		list["extension_groups"].([]any)[0].(map[string]any)["grants"] != nil {
		t.Errorf("gina's groups: %v", list)
	}
	if got := names(m.call(t, "GET", T+"/extension-groups?extension=acl", tom, "").json(t), "extension_groups", "name"); !slices.Equal(got, []string{"platform"}) {
		t.Errorf("groups holding acl: %v", got)
	}
	r404 := m.call(t, "GET", T+"/extension-groups/nope", tom, "")
	for _, c := range []struct {
		method, path, tok, body string
		want                    int
	}{
		{"GET", T + "/extension-groups", ina, "", 404},
		{"GET", T + "/extension-groups?extension=acl", gina, "", 404},
		{"GET", T + "/extension-groups/platform", gina, "", 200},
		{"GET", T + "/extension-groups/platform", ina, "", 404},
		{"GET", T + "/extension-groups/other", gina, "", 404},
		{"GET", T + "/extension-groups/nope", gina, "", 404},
		{"GET", T + "/extension-groups/platform/members", gina, "", 200},
		{"GET", T + "/extension-groups/platform/members", nobody, "", 404},
		{"POST", T + "/extension-groups/platform/members", gina, `{"add":["tresor"]}`, 404},
		{"POST", T + "/extension-groups", gina, `{"name":"mine"}`, 404},
		{"POST", T + "/extension-groups/platform/description", gina, `{"description":"x"}`, 404},
	} {
		r := m.call(t, c.method, c.path, c.tok, c.body, "If-Match", "*")
		if r.status != c.want {
			t.Errorf("%s %s: %d, want %d: %s", c.method, c.path, r.status, c.want, r.body)
		}
		if r.status == 404 && string(r.body) != string(r404.body) {
			t.Errorf("%s %s: another 404: %s", c.method, c.path, r.body)
		}
	}

	// writes: create, members (skipping what is already so), description with If-Match, removal
	if r := m.call(t, "POST", T+"/extension-groups", tom, `{"name":"tools","description":"cli"}`); r.status != 201 ||
		r.header.Get("Location") != T+"/extension-groups/tools" || r.header.Get("ETag") != `"v1"` {
		t.Fatalf("create: %d %v %s", r.status, r.header, r.body)
	}
	if r := m.call(t, "POST", T+"/extension-groups", tom, `{"name":"tools"}`); r.status != 409 {
		t.Errorf("a second tools: %d", r.status)
	}
	r := m.call(t, "POST", T+"/extension-groups/platform/members", tom, `{"add":["tresor","acl"],"remove":["nope"]}`).json(t)
	if added := r["added"].([]any); len(added) != 1 || added[0] != "tresor" || len(r["removed"].([]any)) != 0 {
		t.Errorf("members: %v", r)
	}
	for _, b := range []string{`{"add":["Bad"]}`, `{}`} {
		if r := m.call(t, "POST", T+"/extension-groups/platform/members", tom, b); r.status != 400 {
			t.Errorf("members %s: %d", b, r.status)
		}
	}
	if got := names(m.call(t, "GET", T+"/extension-groups/platform/members?prefix=tr", gina, "").json(t), "members", "name"); !slices.Equal(got, []string{"tresor"}) {
		t.Errorf("members with a prefix: %v", got)
	}
	// gina now administers tresor's releases too: the name joined the group
	if r := m.call(t, "GET", T+"/channels/prod/extensions/tresor/releases", gina, ""); r.status != 200 {
		t.Errorf("gina on tresor after it joined: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extension-groups/tools/description", tom, `{"description":"x"}`); r.status != 428 {
		t.Errorf("description without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extension-groups/tools/description", tom, `{"description":"x"}`, "If-Match", `"v9"`); r.status != 412 {
		t.Errorf("a stale description: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extension-groups/tools/description", tom, `{"description":"tools"}`, "If-Match", `"v1"`); r.status != 200 ||
		r.json(t)["description"] != "tools" || r.header.Get("ETag") != `"v2"` {
		t.Errorf("description: %d %v %s", r.status, r.header, r.body)
	}
	if r := m.call(t, "DELETE", T+"/extension-groups/platform", tom, "", "If-Match", "*"); r.status != 409 {
		t.Errorf("removing a granted group: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/extension-groups/tools", tom, "", "If-Match", `"v2"`); r.status != 204 {
		t.Errorf("removing tools: %d %s", r.status, r.body)
	}

	// grants: on a group; both targets or a missing group refused; the filters
	if r := m.call(t, "POST", T+"/grants", tom, `{"principal":"subject:corp|zoe","verbs":["install"],"extension_group":"platform"}`); r.status != 201 ||
		r.json(t)["extension_group"] != "platform" {
		t.Errorf("a grant on a group: %d %s", r.status, r.body)
	}
	for _, b := range []string{
		`{"principal":"subject:corp|zoe","verbs":["install"],"extension_group":"platform","extension":"acl"}`,
		`{"principal":"subject:corp|zoe","verbs":["install"],"extension_group":"nope"}`,
		`{"principal":"subject:corp|zoe","verbs":["audit"],"extension_group":"platform"}`,
	} {
		if r := m.call(t, "POST", T+"/grants", tom, b); r.status != 400 {
			t.Errorf("grant %s: %d %s", b, r.status, r.body)
		}
	}
	if got := names(m.call(t, "GET", T+"/grants?extension_group=platform", tom, "").json(t), "grants", "principal"); len(got) != 3 {
		t.Errorf("grants on platform: %v", got)
	}
	reach := m.call(t, "GET", T+"/grants?extension=acl", tom, "").json(t)
	via := map[string]string{}
	for _, x := range reach["grants"].([]any) {
		g := x.(map[string]any)
		via[g["principal"].(string)] = g["via"].(string)
		if g["principal"] == "subject:corp|gina" && !slices.Equal(anyStrings(g["verbs_on_extension"]), []string{"install", "admin"}) {
			t.Errorf("gina's verbs on acl: %v", g)
		}
	}
	if via["subject:corp|gina"] != "extension_group" || via["subject:corp|tom"] != "tenant" || via["subject:corp|carl"] != "channel" {
		t.Errorf("grants reaching acl: %v", via)
	}
	if _, ok := via["subject:corp|eve"]; ok {
		t.Errorf("eve's grant on tresor reaches acl: %v", via)
	}
	for _, q := range []string{"?extension=Bad", "?extension_group=Bad"} {
		if r := m.call(t, "GET", T+"/grants"+q, tom, ""); r.status != 400 {
			t.Errorf("a bad filter %s: %d", q, r.status)
		}
	}
	// a grant on a group by id: read and removed like any grant
	gs := m.call(t, "GET", T+"/grants?extension_group=platform&principal=subject:corp|zoe", tom, "").json(t)["grants"].([]any)
	zoe := gs[0].(map[string]any)["id"].(string)
	if r := m.call(t, "GET", T+"/grants/"+zoe, tom, ""); r.status != 200 || r.json(t)["extension_group"] != "platform" {
		t.Errorf("a group grant by id: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/grants/"+zoe, tom, ""); r.status != 204 {
		t.Errorf("removing a group grant: %d", r.status)
	}
	if r := m.call(t, "GET", T+"/grants/"+zoe, tom, ""); r.status != 404 {
		t.Errorf("a removed group grant: %d", r.status)
	}

	// the group list: paged by name, with a prefix
	for _, n := range []string{"tools-a", "tools-b", "tools-c"} {
		if r := m.call(t, "POST", T+"/extension-groups", tom, `{"name":"`+n+`"}`); r.status != 201 {
			t.Fatalf("create %s: %d", n, r.status)
		}
	}
	page := m.call(t, "GET", T+"/extension-groups?prefix=tools&limit=2", tom, "").json(t)
	next, _ := page["next"].(string)
	if got := names(page, "extension_groups", "name"); !slices.Equal(got, []string{"tools-a", "tools-b"}) || next == "" {
		t.Fatalf("first page: %v", page)
	}
	if got := names(m.call(t, "GET", T+"/extension-groups?prefix=tools&limit=2&cursor="+next, tom, "").json(t), "extension_groups", "name"); !slices.Equal(got, []string{"tools-c"}) {
		t.Errorf("second page: %v", got)
	}

	// a name both added and removed is refused; an unchanged description changes nothing
	if r := m.call(t, "POST", T+"/extension-groups/platform/members", tom, `{"add":["acl"],"remove":["acl"]}`); r.status != 400 {
		t.Errorf("added and removed: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extension-groups/platform/description", tom, `{"description":"hugr"}`, "If-Match", `"v1"`); r.status != 200 ||
		r.header.Get("ETag") != `"v1"` {
		t.Errorf("an unchanged description: %d %v", r.status, r.header)
	}

	// the events: what actually changed, the grant's group, the removal's members before it
	if r := m.call(t, "POST", T+"/extension-groups/tools-a/members", tom, `{"add":["one","two"]}`); r.status != 200 {
		t.Fatal(r.status)
	}
	if r := m.call(t, "DELETE", T+"/extension-groups/tools-a", tom, "", "If-Match", "*"); r.status != 204 {
		t.Fatal(r.status)
	}
	acme, _ := m.st.GetTenant(ctx, "acme")
	evs, err := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Subject: "extension-group:", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	data := map[string][]string{}
	for _, e := range evs {
		if e.Subject == "extension-group:tools-a" || e.Subject == "extension-group:platform" && e.Kind == "extension_group.members" {
			data[e.Subject+" "+e.Kind] = append(data[e.Subject+" "+e.Kind], e.Data)
		}
	}
	for k, want := range map[string][]string{
		// newest first: the API's change, then the setup's
		"extension-group:platform extension_group.members": {`{"added":["tresor"],"removed":[]}`, `{"added":["acl"],"removed":[]}`},
		"extension-group:tools-a extension_group.members":  {`{"added":[],"removed":["one","two"]}`, `{"added":["one","two"],"removed":[]}`},
		"extension-group:tools-a extension_group.remove":   {`{"description":"","members":2,"name":"tools-a"}`},
	} {
		if !slices.Equal(data[k], want) {
			t.Errorf("%s: %q, want %q", k, data[k], want)
		}
	}
	gevs, _ := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "grant.remove", Limit: 100})
	if len(gevs) == 0 || !strings.Contains(gevs[0].Data, `"extension_group":"platform"`) {
		t.Errorf("grant.remove: %v", gevs)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The tenant's principals with their grant counts (spec 0015 phase 2a), both grant tables counted.
func TestGrantPrincipals(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	cli := m.env.adm
	if _, err := cli.CreateExtensionGroup(ctx, admin, "acme", "platform", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AddGrant(ctx, admin, "acme", "subject:corp|eve", []string{"install"}, tenants.GrantOn{ExtensionGroup: "platform"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AddGrant(ctx, admin, "acme", "role:corp|team", []string{"admin"}, tenants.GrantOn{ExtensionGroup: "platform"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AddPublisher(ctx, admin, "acme", "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AddGrant(ctx, admin, "acme", "publisher:ci", []string{"publish"}, tenants.GrantOn{Extension: "acl"}); err != nil {
		t.Fatal(err)
	}
	tom := m.toks["tenant admin"]
	r := m.call(t, "GET", T+"/grants/principals", tom, "")
	counts := map[string]float64{}
	for _, x := range r.json(t)["principals"].([]any) {
		counts[x.(map[string]any)["principal"].(string)] = x.(map[string]any)["grants"].(float64)
	}
	if counts["subject:corp|eve"] != 2 || counts["subject:corp|tom"] != 1 || counts["role:corp|team"] != 1 || counts["publisher:ci"] != 1 || len(counts) != 6 {
		t.Fatalf("principals: %s", r.body)
	}
	page := m.call(t, "GET", T+"/grants/principals?prefix=subject:corp|&limit=3", tom, "").json(t)
	next, _ := page["next"].(string)
	if got := names(page, "principals", "principal"); !slices.Equal(got, []string{"subject:corp|carl", "subject:corp|eve", "subject:corp|ivan"}) || next == "" {
		t.Fatalf("first page: %v", page)
	}
	if got := names(m.call(t, "GET", T+"/grants/principals?prefix=subject:corp|&limit=3&cursor="+next, tom, "").json(t), "principals", "principal"); !slices.Equal(got, []string{"subject:corp|tom"}) {
		t.Errorf("second page: %v", got)
	}
	if r := m.call(t, "GET", T+"/grants/principals?prefix="+strings.Repeat("x", 301), tom, ""); r.status != 400 {
		t.Errorf("a long prefix: %d", r.status)
	}
	team := m.keys.sign(t, idpURL, map[string]any{"sub": "tina", "aud": publicURL + "/acme"}) // no grant of its own
	for who, tok := range map[string]string{"an extension administrator": m.toks["ext admin"], "a token without grants": team} {
		if r := m.call(t, "GET", T+"/grants/principals", tok, ""); r.status != 404 {
			t.Errorf("%s: %d", who, r.status)
		}
	}
}
