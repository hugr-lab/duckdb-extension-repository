package api_test

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0015: the console's sign-in configuration, and an issuer record's console client set and
// removed by a tenant administrator with the record's id as If-Match.
func TestConsoleRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom, eve := m.toks["tenant admin"], m.toks["ext admin"]
	if r := m.call(t, "GET", "/api/v1/console", "", ""); r.status != 200 || r.json(t)["audience"] == "" ||
		r.json(t)["admin_token_max_age"].(float64) != 3600 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the server console: %d %s", r.status, r.body)
	}
	issuers := func() []any {
		t.Helper()
		r := m.call(t, "GET", T+"/console", "", "")
		if r.status != 200 {
			t.Fatalf("the tenant console: %d %s", r.status, r.body)
		}
		if aud, _ := r.json(t)["audience"].(string); !strings.HasSuffix(aud, "/acme") {
			t.Fatalf("the canonical audience: %q", aud) // phase 1b
		}
		return r.json(t)["issuers"].([]any)
	}
	if got := issuers(); len(got) != 0 {
		t.Fatalf("before any client: %v", got)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/nobody/console", "", ""); r.status != 404 {
		t.Errorf("an unknown tenant's console: %d", r.status)
	}
	tag := m.call(t, "GET", T+"/issuers/corp", tom, "").header.Get("ETag")
	if tag == "" {
		t.Fatal("no issuer ETag")
	}
	const body = `{"client_id":"kista-console","scopes":["openid","offline_access"]}`
	if r := m.call(t, "POST", T+"/issuers/corp/console", eve, body, "If-Match", tag); r.status != 404 {
		t.Errorf("an extension administrator sets a console client: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, body); r.status != 428 {
		t.Errorf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, body, "If-Match", `"0190a7b6-0000-7000-8000-000000000000"`); r.status != 412 {
		t.Errorf("another record's id: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, `{"client_id":"x","audience":"api://someone-else"}`, "If-Match", tag); r.status != 400 {
		t.Errorf("an audience not the tenant's: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, body, "If-Match", "*"); r.status != 428 {
		t.Errorf("If-Match *: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, body, "If-Match", tag); r.status != 200 || r.json(t)["client_id"] != "kista-console" {
		t.Fatalf("set: %d %s", r.status, r.body)
	}
	got := issuers()
	if len(got) != 1 {
		t.Fatalf("after set: %v", got)
	}
	is := got[0].(map[string]any)
	if is["name"] != "corp" || is["client_id"] != "kista-console" || is["issuer"] == "" || is["audience"] != "https://kista.example/acme" {
		t.Errorf("the listed issuer: %v", is)
	}
	// an assigned audience: accepted, advertised, and kept while a console client asks for it
	sa := m.toks["server admin"]
	if r := m.call(t, "POST", T+"/audiences", sa, `{"audience":"api://kista-acme"}`); r.status/100 != 2 {
		t.Fatalf("assign an audience: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/issuers/corp/console", tom, `{"client_id":"kista-console","audience":"api://kista-acme"}`, "If-Match", tag); r.status != 200 {
		t.Fatalf("set with an assigned audience: %d %s", r.status, r.body)
	}
	if got := issuers(); got[0].(map[string]any)["audience"] != "api://kista-acme" {
		t.Errorf("the advertised audience: %v", got)
	}
	if r := m.call(t, "POST", T+"/audiences/remove", sa, `{"audience":"api://kista-acme"}`); r.status != 400 {
		t.Errorf("removing an audience a console client asks for: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/issuers/corp/console", tom, "", "If-Match", "*"); r.status != 428 {
		t.Errorf("remove with If-Match *: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/issuers/corp/console", tom, "", "If-Match", tag); r.status != 204 {
		t.Fatalf("remove: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/issuers/corp/console", tom, "", "If-Match", tag); r.status != 404 {
		t.Errorf("remove again: %d", r.status)
	}
	if got := issuers(); len(got) != 0 {
		t.Errorf("after remove: %v", got)
	}
	// a server issuer is never a tenant console's
	if _, err := m.adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "ops", URL: opsURL, RequiredClaims: map[string]string{"tid": "t1"}}); err != nil {
		t.Fatal(err)
	}
	opsTag := m.call(t, "GET", T+"/issuers/ops", tom, "").header.Get("ETag")
	if r := m.call(t, "POST", T+"/issuers/ops/console", tom, body, "If-Match", opsTag); r.status != 400 {
		t.Errorf("a console client at a server issuer: %d %s", r.status, r.body)
	}
	// a suspended tenant's console is not found
	ttag := m.call(t, "GET", T, m.toks["server admin"], "").header.Get("ETag")
	if r := m.call(t, "POST", T+"/suspend", m.toks["server admin"], "", "If-Match", ttag); r.status != 200 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/console", "", ""); r.status != 404 {
		t.Errorf("a suspended tenant's console: %d", r.status)
	}
}
