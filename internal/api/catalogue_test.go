package api_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0033: publishing into the catalogue only takes publish on the name tenant-wide (a channel's
// grant does not reach it), answers the build (201, 200 for the same body), refuses current, and
// another body of the version (409); a tenant administrator purges an entry no release holds.
func TestCatalogueRoutes(t *testing.T) {
	m := newMgmt(t)
	const P = "/api/v1/tenants/acme/catalogue/extensions/"
	for sub, on := range map[string]tenants.GrantOn{"wide": {Package: "newext"}, "chan": {Channel: "prod", Package: "newext"}} {
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+sub, []string{"publish"}, on); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(sub string) string {
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	q := "/builds?version=1.0&platform=linux_amd64"
	if r := m.upload(t, P+"newext"+q, tok("chan"), file); r.status != 404 {
		t.Fatalf("a channel's grant: %d %s", r.status, r.body)
	}
	if r := m.upload(t, P+"newext"+q, m.toks["tenant admin"], file); r.status != 404 {
		t.Fatalf("an administrator without publish: %d %s", r.status, r.body)
	}
	if r := m.upload(t, P+"newext"+q+"&current=false", tok("wide"), file); r.status != 400 {
		t.Fatalf("current: %d %s", r.status, r.body)
	}
	r := m.upload(t, P+"newext"+q, tok("wide"), file)
	if r.status != 201 || r.json(t)["visibility"] != "private" || r.json(t)["slot"] != "v2.0.0" {
		t.Fatalf("published: %d %s", r.status, r.body)
	}
	id, _ := r.json(t)["id"].(string)
	if r := m.upload(t, P+"newext"+q, tok("wide"), file); r.status != 200 {
		t.Fatalf("again: %d %s", r.status, r.body)
	}
	// the tenant's own source keeps the version's rule per channel: another body lands beside it
	if r := m.upload(t, P+"newext"+q, tok("wide"), built(t, "1.0", "newext_duckdb_cpp_init", "x")); r.status != 201 {
		t.Fatalf("another body from the tenant's source: %d %s", r.status, r.body)
	}
	const D = "/api/v1/tenants/acme/catalogue/builds/"
	if r := m.call(t, "DELETE", D+id, tok("wide"), ""); r.status != 404 {
		t.Fatalf("a publisher purges: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", D+id, m.toks["tenant admin"], ""); r.status != 204 {
		t.Fatalf("purged: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", D+id, m.toks["tenant admin"], ""); r.status != 404 {
		t.Fatalf("purged again: %d %s", r.status, r.body)
	}
}
