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

	// spec 0035: every write names its entry; an entry whose builds declare no key takes no licence
	pro := m.licence("tresor", "pro")
	if r := m.call(t, "POST", T, tom, pro); r.status != 400 || !strings.Contains(string(r.body), "?source=") {
		t.Fatalf("an import without its entry: %d %s", r.status, r.body)
	}
	var other store.Publisher
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		other = store.Publisher{TenantID: m.tenantID(t), Name: "other", CreatedBy: "os:1:t"}
		return tx.InsertPublisher(ctx, &other)
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "POST", T+"?source=publisher:other", tom, pro); r.status != 400 || !strings.Contains(string(r.body), "kid_untrusted") ||
		!strings.Contains(string(r.body), "publisher:other") {
		t.Fatalf("an import into an entry with no key: %d %s", r.status, r.body)
	}
	for _, bad := range []string{"tenant", "publisher:nope", "upstream-id:", "nonsense"} {
		if r := m.call(t, "POST", T+"?source="+bad, tom, pro); r.status != 400 || !strings.Contains(string(r.body), "?source=") {
			t.Fatalf("an import into %s: %d %s", bad, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", "/api/v1/licenses/keys", m.toks["server admin"], ""); r.status != 404 {
		t.Fatalf("the old keys route: %d", r.status)
	}
	kid, err := storetest.LicenseKey(ctx, m.st, m.tenantID(t), "tresor", store.KeySourcePublisher, "nobody", &m.licKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// the keys: each with the entries it is live for and their builds; of one entry with ?source=
	r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys/tresor", tom, "")
	if ks := fmt.Sprint(r.json(t)["names"]); r.status != 200 || !strings.Contains(ks, kid) || !strings.Contains(ks, "builds:3") ||
		!strings.Contains(ks, "kind:publisher") || !strings.Contains(ks, "name:nobody") || strings.Contains(ks, "owner") {
		t.Fatalf("license keys: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys/tresor?source=publisher:nobody", tom, ""); r.status != 200 ||
		!strings.Contains(string(r.body), kid) {
		t.Fatalf("one entry's keys: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys?source=publisher:other", tom, ""); r.status != 200 ||
		len(r.json(t)["names"].([]any)) != 0 {
		t.Fatalf("an entry with no key: %d %s", r.status, r.body)
	}
	for _, bad := range []string{"publisher:nope", "tenant", "x"} {
		if r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys?source="+bad, tom, ""); r.status != 400 {
			t.Fatalf("the keys of %s: %d %s", bad, r.status, r.body)
		}
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/license-keys", m.toks["install"], ""); r.status != 404 {
		t.Fatalf("license keys to an installer: %d", r.status)
	}
	// the owner route is gone
	if r := m.call(t, "PUT", "/api/v1/tenants/acme/license-keys/tresor/"+kid+"/owner", tom, `{"source":"publisher:nobody"}`); r.status != 404 {
		t.Fatalf("the owner route: %d %s", r.status, r.body)
	}

	// an import, then one over it without If-Match, then with it
	r = m.call(t, "POST", T+"?source=publisher:nobody", tom, pro)
	if r.status != 201 || r.header.Get("Location") != T+"/tresor?source=publisher%3Anobody" || r.json(t)["plan"] != "pro" ||
		r.json(t)["status"] != "valid" || r.json(t)["source"] != "import" || r.json(t)["entry"] != "publisher:nobody" || r.json(t)["owner"] != nil {
		t.Fatalf("an import: %d %s", r.status, r.body)
	}
	etag := r.header.Get("ETag")
	if r := m.call(t, "POST", T+"?source=publisher:nobody", tom, m.licence("tresor", "max")); r.status != 409 {
		t.Fatalf("an import over a licence without If-Match: %d %s", r.status, r.body)
	}
	r = m.call(t, "POST", T+"?source=publisher:nobody", tom, m.licence("tresor", "max"), "If-Match", etag)
	if r.status != 200 || r.json(t)["plan"] != "max" {
		t.Fatalf("a replacement: %d %s", r.status, r.body)
	}
	etag = r.header.Get("ETag")
	if r := m.call(t, "POST", T+"?source=publisher:nobody", tom, "not.a.licence"); r.status != 400 {
		t.Fatalf("a broken licence: %d %s", r.status, r.body)
	}

	// reading: the claims and the state, never the token; by its entry (its source's label or id
	// form), or without one while it is the name's only licence
	r = m.call(t, "GET", T+"/tresor", tom, "")
	if r.status != 200 || r.header.Get("ETag") != etag || r.json(t)["limits"] == nil {
		t.Fatalf("get: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/tresor?source=publisher:nobody", tom, ""); r.status != 200 || r.header.Get("ETag") != etag {
		t.Fatalf("get by entry: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/tresor?source=upstream:nobody", tom, ""); r.status != 404 {
		t.Fatalf("get of another entry: %d %s", r.status, r.body)
	}
	if got := names(m.call(t, "GET", T, tom, "").json(t), "licenses", "name"); !slices.Equal(got, []string{"tresor"}) {
		t.Fatalf("list: %v", got)
	}
	if got := names(m.call(t, "GET", T+"?source=publisher:other", tom, "").json(t), "licenses", "name"); len(got) != 0 {
		t.Fatalf("list of another entry: %v", got)
	}
	// licences in two entries: without ?source= a read is 409 naming them; by the id form
	put := func(entry string) {
		t.Helper()
		if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
			l := store.License{TenantID: m.tenantID(t), Name: "tresor", Entry: entry, LicenseID: "lic-" + entry, Issuer: "i", HolderID: "acme",
				Plan: "old", Token: "t", Key: kid, IssuedAt: 1, ExpiresAt: 2, GraceUntil: 4102444800, Source: store.LicenseImport,
				Visibility: store.Private, Actor: "os:1:t"}
			return tx.PutLicense(ctx, &l, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	put(store.PublisherSource(other.ID))
	if r := m.call(t, "GET", T+"/tresor", tom, ""); r.status != 409 || r.json(t)["type"] != "urn:kista:problem:conflict" ||
		!strings.Contains(fmt.Sprint(r.json(t)["detail"]), "several entries: publisher:nobody, publisher:other; name one with ?source=") {
		t.Fatalf("a name in two entries: %d %s", r.status, r.body)
	}
	var nobody string
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, m.tenantID(t), "nobody")
		nobody = p.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "GET", T+"/tresor?source=publisher-id:"+nobody, tom, ""); r.status != 200 || r.json(t)["entry"] != "publisher:nobody" {
		t.Fatalf("by the id form: %d %s", r.status, r.body)
	}
	// a write without its entry: 400, whatever the name holds
	for _, c := range []struct{ method, path string }{{"DELETE", T + "/tresor"}, {"POST", T + "/tresor/public"}, {"POST", T + "/tresor/private"}} {
		if r := m.call(t, c.method, c.path, tom, "", "If-Match", "*"); r.status != 400 || !strings.Contains(string(r.body), "?source=") {
			t.Fatalf("%s %s without ?source=: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	if r := m.call(t, "DELETE", T+"/tresor?source=publisher:other", tom, "", "If-Match", versionTag(1)); r.status != 204 {
		t.Fatalf("removing the other one: %d %s", r.status, r.body)
	}
	// a removed source's licence: listed under its id form, removable by it, then 400
	put(store.PublisherSource(other.ID))
	if err := store.ExecRaw(ctx, m.st, "DELETE FROM publishers WHERE id = ?", other.ID); err != nil {
		t.Fatal(err)
	}
	gone := "publisher-id:" + other.ID
	if r := m.call(t, "GET", T+"/tresor?source="+gone, tom, ""); r.status != 200 || r.json(t)["entry"] != gone {
		t.Fatalf("a removed source's: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/tresor/private?source="+gone, tom, "", "If-Match", "*"); r.status != 400 {
		t.Fatalf("a removed source's visibility: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/tresor?source=publisher:other", tom, "", "If-Match", "*"); r.status != 400 {
		t.Fatalf("a removed source by its name: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/tresor?source="+gone, tom, "", "If-Match", "*"); r.status != 204 {
		t.Fatalf("a removed source's removal: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/tresor?source="+gone, tom, "", "If-Match", "*"); r.status != 400 {
		t.Fatalf("a removed source's entry with no licence: %d %s", r.status, r.body)
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
	r = m.call(t, "POST", T+"/tresor/public?source=publisher:nobody", tom, "", "If-Match", etag)
	if r.status != 200 || r.json(t)["visibility"] != "public" {
		t.Fatalf("public: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/tresor?source=publisher:nobody", tom, "", "If-Match", etag); r.status != 412 {
		t.Fatalf("a stale delete: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/tresor?source=publisher:nobody", tom, "", "If-Match", r.header.Get("ETag")); r.status != 204 {
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
