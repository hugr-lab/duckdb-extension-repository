package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// licenceOf is a licence of the holder sub.
func (m *mgmt) licenceOf(sub, ext, plan string) string {
	now := time.Now().Unix()
	return vectors.Sign(m.licKey, vectors.Header(m.licKey), fmt.Sprintf(
		`{"v":1,"iss":"https://issuer.example","jti":"lic-%s-%s-%s","sub":%q,"ext":%q,"plan":%q,"iat":%d,"exp":%d}`,
		sub, ext, plan, sub, ext, plan, now-60, now+86400))
}

// Spec 0028: the expected holder over the API: set and cleared by server administrators with
// If-Match, read by the tenant's administrators, and enforced on an import.
func TestTenantHolderRoutes(t *testing.T) {
	m := newMgmt(t)
	sa, ta := m.toks["server admin"], m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"

	// at creation
	r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"beta","holder_id":"org-b"}`)
	if r.status != 201 || r.json(t)["holder_id"] != "org-b" {
		t.Fatalf("create with a holder: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/beta", sa, ""); r.status != 200 || r.json(t)["holder_id"] != "org-b" {
		t.Fatalf("read: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"gamma","holder_id":"`+strings.Repeat("a", 129)+`"}`); r.status != 400 {
		t.Fatalf("a holder of 129 bytes: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T, ta, ""); r.status != 200 || r.json(t)["holder_id"] != nil {
		t.Fatalf("no holder: %d %s", r.status, r.body)
	}

	// only server administrators set and clear it; a tenant administrator is answered as by a
	// route it may not use
	body := `{"holder_id":"acme"}`
	for _, who := range []string{"tenant admin", "channel adm", "ext admin", "server user", "other admin"} {
		if r := m.call(t, "PUT", T+"/holder", m.toks[who], body, "If-Match", "*"); r.status != 404 && r.status != 401 {
			t.Errorf("PUT by %s: %d %s", who, r.status, r.body)
		}
		if r := m.call(t, "DELETE", T+"/holder", m.toks[who], "", "If-Match", "*"); r.status != 404 && r.status != 401 {
			t.Errorf("DELETE by %s: %d %s", who, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", T, ta, ""); r.json(t)["holder_id"] != nil {
		t.Fatalf("a refused PUT set it: %s", r.body)
	}
	r = m.call(t, "GET", T, sa, "")
	tag := r.header.Get("ETag")
	if r := m.call(t, "PUT", T+"/holder", sa, body); r.status != 428 {
		t.Errorf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "PUT", T+"/holder", sa, body, "If-Match", `"v99"`); r.status != 412 {
		t.Errorf("a stale If-Match: %d", r.status)
	}
	for _, bad := range []string{`{}`, `{"holder_id":""}`, `{"holder_id":"x","other":1}`, `{"holder_id":"` + strings.Repeat("a", 129) + `"}`} {
		if r := m.call(t, "PUT", T+"/holder", sa, bad, "If-Match", tag); r.status != 400 {
			t.Errorf("body %s: %d %s", bad, r.status, r.body)
		}
	}
	r = m.call(t, "PUT", T+"/holder", sa, body, "If-Match", tag)
	if r.status != 200 || r.json(t)["holder_id"] != "acme" || r.header.Get("ETag") == tag {
		t.Fatalf("set: %d %s", r.status, r.body)
	}
	tag = r.header.Get("ETag")
	if r := m.call(t, "GET", T, ta, ""); r.status != 200 || r.json(t)["holder_id"] != "acme" || r.header.Get("ETag") != tag {
		t.Fatalf("the tenant administrator's read: %d %s", r.status, r.body)
	}

	// the import: another holder's licence is refused, naming both; the holder's is taken
	if _, err := storetest.LicenseKey(ctx, m.st, m.tenantID(t), "tresor", store.KeySourcePublisher, "nobody", &m.licKey.PublicKey); err != nil {
		t.Fatal(err)
	}
	r = m.call(t, "POST", T+"/licenses", ta, m.licenceOf("org-2", "tresor", "pro"))
	if r.status != 400 || r.json(t)["type"] != "urn:kista:problem:invalid" || !strings.Contains(string(r.body), "holder_mismatch") ||
		!strings.Contains(string(r.body), `\"acme\"`) || !strings.Contains(string(r.body), `\"org-2\"`) {
		t.Fatalf("another holder's licence: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/licenses", ta, m.licenceOf("acme", "tresor", "pro")); r.status != 201 {
		t.Fatalf("the holder's licence: %d %s", r.status, r.body)
	}

	// cleared: another holder's licence is taken; setting the holder again is refused, naming it
	if r := m.call(t, "DELETE", T+"/holder", sa, ""); r.status != 428 {
		t.Errorf("DELETE without If-Match: %d", r.status)
	}
	r = m.call(t, "DELETE", T+"/holder", sa, "", "If-Match", tag)
	if r.status != 200 || r.json(t)["holder_id"] != nil {
		t.Fatalf("clear: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/licenses", ta, m.licenceOf("org-2", "tresor", "max"), "If-Match", "*"); r.status != 200 {
		t.Fatalf("no holder, another holder's replacement: %d %s", r.status, r.body)
	}
	r = m.call(t, "PUT", T+"/holder", sa, body, "If-Match", "*")
	if r.status != 409 || !strings.Contains(string(r.body), "holder org-2") {
		t.Fatalf("over another holder's licence: %d %s", r.status, r.body)
	}
	// a tenant administrator still reads the tenant
	if r := m.call(t, "GET", T, ta, ""); r.status != 200 || r.json(t)["holder_id"] != nil {
		t.Fatalf("after a refused set: %d %s", r.status, r.body)
	}
}
