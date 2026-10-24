package api_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func (m *mgmt) tenantID(t *testing.T) string {
	t.Helper()
	tn, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return tn.ID
}

func (m *mgmt) licence(ext, plan string) string {
	now := time.Now().Unix()
	return vectors.Sign(m.licKey, vectors.Header(m.licKey), fmt.Sprintf(
		`{"v":1,"iss":"https://issuer.example","jti":"lic-%s-%s","sub":"acme","ext":%q,"plan":%q,"limits":{"sources":5},"iat":%d,"exp":%d}`,
		ext, plan, ext, plan, now-60, now+86400))
}

// Spec 0012: a tenant's licences over the API, to its administrators.
func TestLicenseRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme/licenses"
	tom := m.toks["tenant admin"]

	// spec 0021: no live build of tresor declares a key yet
	pro := m.licence("tresor", "pro")
	if r := m.call(t, "POST", T, tom, pro); r.status != 400 || !strings.Contains(string(r.body), "kid_untrusted") {
		t.Fatalf("an import with no key: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/licenses/keys", m.toks["server admin"], ""); r.status != 404 {
		t.Fatalf("the old keys route: %d", r.status)
	}
	kid, err := storetest.LicenseKey(ctx, m.st, m.tenantID(t), "tresor", store.KeySourcePublisher, "nobody", &m.licKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys/tresor", tom, "")
	if ks := fmt.Sprint(r.json(t)["names"]); r.status != 200 || !strings.Contains(ks, kid) || !strings.Contains(ks, "builds:3") ||
		!strings.Contains(ks, "owner:publisher:nobody") {
		t.Fatalf("license keys: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys", m.toks["install"], ""); r.status != 404 {
		t.Fatalf("license keys to an installer: %d", r.status)
	}

	// an import, then one over it without If-Match, then with it
	r = m.call(t, "POST", T, tom, pro)
	if r.status != 201 || r.header.Get("Location") != T+"/tresor?source=publisher%3Anobody" || r.json(t)["plan"] != "pro" ||
		r.json(t)["status"] != "valid" || r.json(t)["source"] != "import" || r.json(t)["owner"] != "publisher:nobody" {
		t.Fatalf("an import: %d %s", r.status, r.body)
	}
	etag := r.header.Get("ETag")
	if r := m.call(t, "POST", T, tom, m.licence("tresor", "max")); r.status != 409 {
		t.Fatalf("an import over a licence without If-Match: %d %s", r.status, r.body)
	}
	r = m.call(t, "POST", T, tom, m.licence("tresor", "max"), "If-Match", etag)
	if r.status != 200 || r.json(t)["plan"] != "max" {
		t.Fatalf("a replacement: %d %s", r.status, r.body)
	}
	etag = r.header.Get("ETag")
	if r := m.call(t, "POST", T, tom, "not.a.licence"); r.status != 400 {
		t.Fatalf("a broken licence: %d %s", r.status, r.body)
	}

	// reading: the claims and the state, never the token; by its source (spec 0033 phase B), its
	// label or its id form, or without one while it is the name's only licence
	r = m.call(t, "GET", T+"/tresor", tom, "")
	if r.status != 200 || r.header.Get("ETag") != etag || r.json(t)["limits"] == nil {
		t.Fatalf("get: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/tresor?source=publisher:nobody", tom, ""); r.status != 200 || r.header.Get("ETag") != etag {
		t.Fatalf("get by source: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/tresor?source=upstream:nobody", tom, ""); r.status != 404 {
		t.Fatalf("get of another source: %d %s", r.status, r.body)
	}
	if got := names(m.call(t, "GET", T, tom, "").json(t), "licenses", "name"); !slices.Equal(got, []string{"tresor"}) {
		t.Fatalf("list: %v", got)
	}
	if got := names(m.call(t, "GET", T+"?source=publisher:other", tom, "").json(t), "licenses", "name"); len(got) != 0 {
		t.Fatalf("list of another source: %v", got)
	}
	// a key's owner is a tenant administrator's to give, to a source that declares the key
	owner := "/api/v1/tenants/acme/license-keys/tresor/" + kid + "/owner"
	if r := m.call(t, "PUT", owner, tom, `{"source":"publisher:nobody"}`); r.status != 200 || r.json(t)["owner"] != "publisher:nobody" ||
		r.json(t)["before"] != "publisher:nobody" {
		t.Fatalf("the owner as it is: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", owner, tom, `{"source":"tenant"}`); r.status != 400 {
		t.Fatalf("the tenant as an owner: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", owner, m.toks["server licence mgr"], `{"source":"publisher:nobody"}`); r.status != 404 {
		t.Fatalf("a licence manager gives a key: %d %s", r.status, r.body)
	}
	for _, who := range []string{"install", "ext admin", "channel adm", "no grants"} {
		if r := m.call(t, "GET", T, m.toks[who], ""); r.status != 404 {
			t.Errorf("%s lists licences: %d", who, r.status)
		}
	}
	for _, who := range []string{"anonymous", "other admin"} { // another tenant's token is none here
		if r := m.call(t, "GET", T, m.toks[who], ""); r.status != 401 {
			t.Errorf("%s lists licences: %d", who, r.status)
		}
	}

	// visibility and removal take If-Match
	if r := m.call(t, "POST", T+"/tresor/public", tom, ""); r.status != 428 {
		t.Fatalf("public without If-Match: %d", r.status)
	}
	r = m.call(t, "POST", T+"/tresor/public", tom, "", "If-Match", etag)
	if r.status != 200 || r.json(t)["visibility"] != "public" {
		t.Fatalf("public: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/tresor", tom, "", "If-Match", etag); r.status != 412 {
		t.Fatalf("a stale delete: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/tresor", tom, "", "If-Match", r.header.Get("ETag")); r.status != 204 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/tresor", tom, ""); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}

	// an upstream's relay: a missing upstream is 404
	if r := m.call(t, "POST", "/api/v1/tenants/acme/upstreams/nope/licenses/sync", tom, ""); r.status != 404 {
		t.Fatalf("sync of a missing upstream: %d %s", r.status, r.body)
	}
}
