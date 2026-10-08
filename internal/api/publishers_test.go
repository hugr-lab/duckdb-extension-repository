package api_test

import (
	"strings"
	"testing"
	"time"
)

// Spec 0008 phase 1b: a tenant administrator binds a publisher to a GitHub workflow; the workflow's
// OIDC token publishes and promotes, and nothing else.
func TestTrustedPublishing(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"

	// the publisher, its credential, its grants (by the tenant administrator, over the API)
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"acl-ci"}`); r.status != 201 || r.header.Get("Location") != T+"/publishers/acl-ci" {
		t.Fatalf("add publisher: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"acl-ci"}`); r.status != 409 {
		t.Errorf("the same publisher: %d", r.status)
	}
	cred := `{"owner_id":"10","repository_id":"20","workflow":"hugr-lab/duckdb-acl/.github/workflows/release.yml","ref":"refs/tags/v*"}`
	r := m.call(t, "POST", T+"/publishers/acl-ci/github", ta, cred)
	if r.status != 201 {
		t.Fatalf("add credential: %d %s", r.status, r.body)
	}
	credID := r.json(t)["id"].(string)
	for name, body := range map[string]string{
		"owner name":       `{"owner_id":"hugr-lab","repository_id":"20","workflow":"a/b/c.yml"}`,
		"unknown provider": `{"provider":"gitlab","owner_id":"1","repository_id":"2","workflow":"a/b/c.yml"}`,
		"bad ref":          `{"owner_id":"1","repository_id":"2","workflow":"a/b/c.yml","ref":"main"}`,
	} {
		if r := m.call(t, "POST", T+"/publishers/acl-ci/github", ta, body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"publish on staging":   {`{"principal":"publisher:acl-ci","verbs":["publish"],"channel":"staging","extension":"newext"}`, 201},
		"promote to prod":      {`{"principal":"publisher:acl-ci","verbs":["promote"],"channel":"prod","extension":"newext"}`, 201},
		"install is refused":   {`{"principal":"publisher:acl-ci","verbs":["install"]}`, 400},
		"admin is refused":     {`{"principal":"publisher:acl-ci","verbs":["admin"]}`, 400},
		"an unknown publisher": {`{"principal":"publisher:nope","verbs":["publish"]}`, 400},
	} {
		if r := m.call(t, "POST", T+"/grants", ta, tc.body); r.status != tc.want {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// an issuer record cannot take the provider's URL
	if r := m.call(t, "POST", T+"/issuers", ta, `{"name":"gh","url":"`+ghURL+`","required_claims":{"repository_owner_id":"10"}}`); r.status != 400 ||
		!strings.Contains(string(r.body), "trusted-publishing provider") {
		t.Errorf("an issuer record at the provider's URL: %d %s", r.status, r.body)
	}

	run := func(over map[string]any) string {
		c := map[string]any{"aud": publicURL + "/acme", "sub": "repo:hugr-lab/duckdb-acl:ref:refs/tags/v1.0",
			"repository_owner_id": "10", "repository_id": "20", "repository": "hugr-lab/duckdb-acl",
			"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/release.yml@refs/tags/v1.0", "ref": "refs/tags/v1.0",
			"event_name": "push", "sha": "abc123", "run_id": "42", "exp": time.Now().Add(10 * time.Minute).Unix()}
		for k, v := range over {
			c[k] = v
		}
		return m.keys.sign(t, ghURL, c)
	}
	ci := run(nil)
	S := T + "/channels/staging/extensions/newext/releases"
	r = m.upload(t, S+"?version=1.0&platform=linux_amd64", ci, built(t, "1.0", "newext_duckdb_cpp_init"))
	if r.status != 201 {
		t.Fatalf("publish from the workflow: %d %s", r.status, r.body)
	}
	rel := r.json(t)
	prov, _ := rel["provenance"].(map[string]any)
	if prov["repository_id"] != "20" || prov["sha"] != "abc123" || prov["actor"] != nil {
		t.Errorf("the publication's provenance (to the publisher: no actor): %v", prov)
	}
	// a tenant administrator sees who published it
	if g := m.call(t, "GET", T+"/channels/staging/extensions/newext/releases/"+rel["id"].(string), ta, "").json(t); g["created_by"] != "publisher:acme/acl-ci" {
		t.Errorf("created_by for the administrator: %v", g["created_by"])
	}
	// the workflow reads what it may promote, and promotes
	if got := m.call(t, "GET", S, ci, "").json(t)["releases"].([]any); len(got) != 1 {
		t.Errorf("the workflow's release list: %v", got)
	}
	r = m.call(t, "POST", T+"/channels/prod/extensions/newext/releases/promote", ci, `{"from_channel":"staging","version":"1.0"}`)
	if r.status != 201 {
		t.Fatalf("promote from the workflow: %d %s", r.status, r.body)
	}
	if pp, _ := r.json(t)["releases"].([]any)[0].(map[string]any)["provenance"].(map[string]any); pp["run_id"] != "42" || pp["from_channel"] != "staging" {
		t.Errorf("a promotion's provenance records the run: %v", pp)
	}
	if w := m.call(t, "GET", T+"/whoami", ci, "").json(t); w["publishers"] == nil || len(w["grants"].([]any)) != 2 ||
		w["publishers"].([]any)[0].(map[string]any)["credential"] != "github:"+credID {
		t.Errorf("whoami: %v", w)
	}
	// nothing else: management is not for it; the index is the public view
	if r := m.call(t, "GET", T+"/grants", ci, ""); r.status != 401 {
		t.Errorf("management: %d", r.status)
	}
	if r := m.call(t, "GET", T+"/channels/prod/extensions", ci, ""); r.status != 200 || r.header.Get("Cache-Control") != "public, no-cache" {
		t.Errorf("the index: %d %v", r.status, r.header)
	}
	if r := m.upload(t, T+"/channels/prod/extensions/newext/releases?version=1.1&platform=linux_amd64", ci,
		built(t, "1.1", "newext_duckdb_cpp_init")); r.status != 404 {
		t.Errorf("publishing where it holds no publish: %d", r.status)
	}
	// runs that do not match the credential
	for name, over := range map[string]map[string]any{
		"another repository": {"repository_id": "21"},
		"another workflow":   {"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/x.yml@refs/tags/v1.0"},
		"a branch":           {"ref": "refs/heads/main"},
		"a pull request":     {"event_name": "pull_request_target"},
		"another tenant":     {"aud": publicURL + "/other"},
		"stale":              {"iat": time.Now().Add(-2 * time.Hour).Unix(), "exp": time.Now().Add(-90 * time.Minute).Unix()},
	} {
		if r := m.upload(t, S+"?version=1.2&platform=linux_amd64", run(over), built(t, "1.2", "newext_duckdb_cpp_init")); r.status != 401 {
			t.Errorf("%s: %d", name, r.status)
		}
	}
	// a second publisher bound to the same run: both are the caller's principals
	m.call(t, "POST", T+"/publishers", ta, `{"name":"zz-ci"}`)
	m.call(t, "POST", T+"/publishers/zz-ci/github", ta, cred)
	if w := m.call(t, "GET", T+"/whoami", ci, "").json(t); len(w["publishers"].([]any)) != 2 {
		t.Errorf("two publishers: %v", w)
	}
	if r := m.call(t, "DELETE", T+"/publishers/zz-ci", ta, ""); r.status != 204 {
		t.Fatalf("remove zz-ci: %d", r.status)
	}
	if g := m.call(t, "GET", T+"/publishers/acl-ci/github/"+credID, ta, ""); g.status != 200 || g.json(t)["workflow"] == nil {
		t.Errorf("get credential: %d", g.status)
	}
	// a removed publisher (its credential still in place) stops the workflow at its next request
	if r := m.call(t, "DELETE", T+"/publishers/acl-ci", ta, ""); r.status != 204 {
		t.Fatalf("remove publisher: %d", r.status)
	}
	if r := m.call(t, "GET", S, ci, ""); r.status != 401 {
		t.Errorf("after the publisher's removal: %d", r.status)
	}
	if got := m.call(t, "GET", T+"/grants?principal=publisher:acl-ci", ta, "").json(t)["grants"].([]any); len(got) != 0 {
		t.Errorf("a removed publisher's grants: %v", got)
	}
}
