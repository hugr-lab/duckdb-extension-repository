package api_test

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// declare adds a publisher of the tenant's own with a build of tresor of its own declaring the
// licence key pub (spec 0035: the key is live in its entry), and returns the publisher's id.
func (m *mgmt) declare(t *testing.T, name, hash string, pub *rsa.PublicKey) string {
	t.Helper()
	var id string
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		p := store.Publisher{TenantID: m.tenantID(t), Name: name, CreatedBy: "os:1:t"}
		if err := tx.InsertPublisher(ctx, &p); err != nil {
			return err
		}
		id = p.ID
		if _, err := tx.SetPublisherLicenseKeys(ctx, p.TenantID, name, true); err != nil {
			return err
		}
		b := store.Build{TenantID: p.TenantID, Source: store.PublisherSource(p.ID), Name: "tresor", PackageVersion: "9.0", Platform: "linux_amd64",
			ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat(hash, 64), Origin: store.OriginPublication, CreatedBy: "os:1:t", Checked: true}
		if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
			return err
		}
		pemKey, err := extfile.MarshalPublicKeyPEM(pub)
		if err != nil {
			return err
		}
		_, err = tx.RecordLicenseKeys(ctx, p.TenantID, b.ID, store.KeySourcePublisher, p.ID, []store.LicenseKey{{Kid: extfile.Fingerprint(pub), PEM: pemKey}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// Spec 0037 PR 1: a distributor's publisher's licence entry, live or removed, is written by its
// licence manager (never public) and by server administrators; a tenant administrator only reads it,
// and is refused with a 404 of its own type that says why, recorded with what it was (a copy of the
// distributor's licence in another entry is theirs to make public: not prevented). A licence older
// (by iat) than one the tenant holds from the same issuer for the same holder, in any entry, is
// refused (rule older), before the stored one is compared, and recorded: a distributor's entry's
// licence counts whatever its key (a rotated key supersedes), another entry's only when verified by
// the same key (a licence signed with another key never blocks); an equal iat is not older; a stored
// licence past its grace does not count, but one whose entry's source was removed still does.
func TestDistributorsLicences(t *testing.T) {
	m := newMgmt(t)
	tom, lm, sa := m.toks["tenant admin"], m.toks["server licence mgr"], m.toks["server admin"]
	const T = "/api/v1/tenants/acme"
	tid := m.tenantID(t)
	now := time.Now().Unix()
	sign := func(key *rsa.PrivateKey, plan string, iat int64) string {
		return vectors.Sign(key, vectors.Header(key), fmt.Sprintf(
			`{"v":1,"iss":"https://issuer.example","jti":"lic-%s-%d","sub":"acme","ext":"tresor","plan":%q,"iat":%d,"exp":%d}`,
			plan, iat, plan, iat, now+86400))
	}
	lic := func(plan string, iat int64) string { return sign(m.licKey, plan, iat) }
	call := func(want int, method, path, tok, body string, hdr ...string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body, hdr...)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	const why = "a distributor's entry: its licence manager or a server administrator writes it"
	refused := func(r resp, detail string) {
		t.Helper()
		if r.status != 404 || r.json(t)["type"] != "urn:kista:problem:distributors-entry" || !strings.Contains(fmt.Sprint(r.json(t)["detail"]), detail) {
			t.Fatalf("not refused as a distributor's entry: %d %s", r.status, r.body)
		}
	}
	older := func(r resp, in string) {
		t.Helper()
		if d := fmt.Sprint(r.json(t)["detail"]); r.status != 400 || !strings.Contains(d, "the licence is refused (older)") || !strings.Contains(d, in) {
			t.Fatalf("not refused as older (from %s): %d %s", in, r.status, r.body)
		}
	}
	if _, err := storetest.LicenseKey(ctx, m.st, tid, "tresor", store.KeySourcePublisher, "enterest-ci", &m.licKey.PublicKey); err != nil {
		t.Fatal(err)
	}
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		_, err := tx.SetPublisherDistributor(ctx, tid, "enterest-ci", "enterest")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m.declare(t, "ci", "c", &m.licKey.PublicKey) // the tenant's own publisher, the same key live in its entry
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m.declare(t, "twin", "d", &other.PublicKey) // the tenant's own, a key of the tenant administrator's
	const E = T + "/licenses?source=publisher:enterest-ci"
	const CI = T + "/licenses?source=publisher:ci"
	const L = T + "/licenses/tresor"
	const Q = "?source=publisher:enterest-ci"
	etagOf := func(path, tok string) string { return call(200, "GET", path, tok, "").header.Get("ETag") }

	// the writers of a distributor's entry: not a tenant administrator
	refused(m.call(t, "POST", E, tom, lic("pro", now-60)), why)
	etag := call(201, "POST", E, lm, lic("pro", now-60)).header.Get("ETag")
	if r := call(200, "GET", L+Q, tom, ""); r.json(t)["distributor"] != "enterest" {
		t.Fatalf("the tenant administrator's read: %s", r.body)
	}
	if r := call(200, "GET", T+"/licenses", tom, ""); !strings.Contains(string(r.body), `"distributor":"enterest"`) {
		t.Fatalf("the list: %s", r.body)
	}
	refused(m.call(t, "POST", E, tom, lic("pro", now-60), "If-Match", etag), why)
	refused(m.call(t, "DELETE", L+Q, tom, "", "If-Match", etag), why)
	refused(m.call(t, "POST", L+"/public"+Q, tom, "", "If-Match", etag), why)
	refused(m.call(t, "POST", L+"/private"+Q, tom, "", "If-Match", etag), why)
	// a licence manager writes it, never public; a server administrator writes everything
	if r := call(404, "POST", L+"/public"+Q, lm, "", "If-Match", etag); r.json(t)["type"] != "urn:kista:problem:not-found" {
		t.Fatalf("public by a licence manager: %s", r.body)
	}
	etag = call(200, "POST", L+"/public"+Q, sa, "", "If-Match", etag).header.Get("ETag")
	etag = call(200, "POST", L+"/private"+Q, lm, "", "If-Match", etag).header.Get("ETag")
	etag = call(200, "POST", E, sa, lic("pro", now-60), "If-Match", etag).header.Get("ETag") // the same file, a server administrator's import

	// older: refused on an import and a replacement, before the stored licence is compared (no 409)
	for _, hdr := range [][]string{nil, {"If-Match", etag}} {
		older(m.call(t, "POST", E, lm, lic("old", now-120), hdr...), "publisher:enterest-ci")
	}
	// the same file again: an equal iat is not older
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")
	// in another entry where the key is live, the tenant's own: refused while the later one is stored
	older(m.call(t, "POST", CI, tom, lic("old", now-120)), "publisher:enterest-ci")

	// another key never blocks: the tenant administrator's licence with the issuer's and holder's
	// claims, signed with a key of their own live in their entry, issued later
	call(201, "POST", T+"/licenses?source=publisher:twin", tom, sign(other, "self", now-30))
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")

	// a copy of the distributor's licence (the same licence) in another entry where its key is live is
	// the tenant administrator's to make public: covered by the terms and spec 0035 phase 2's warning,
	// not prevented
	call(201, "POST", CI, tom, lic("pro", now-60))
	if r := call(200, "POST", L+"/public?source=publisher:ci", tom, "", "If-Match", etagOf(L+"?source=publisher:ci", tom)); r.json(t)["visibility"] != "public" {
		t.Fatalf("a copy made public: %s", r.body)
	}
	// a later licence of the same key in the tenant's own entry blocks the distributor's older one
	call(200, "POST", CI, tom, lic("later", now-30), "If-Match", etagOf(L+"?source=publisher:ci", tom))
	older(m.call(t, "POST", E, lm, lic("pro", now-60), "If-Match", etag), "publisher:ci")
	// once its grace has ended it counts no more
	call(204, "DELETE", L+"?source=publisher:ci", tom, "", "If-Match", "*")
	past := store.License{TenantID: tid, PackageType: pkgtype.Default, Name: "tresor", Entry: "", LicenseID: "lic-past", Issuer: "https://issuer.example",
		HolderID: "acme", Plan: "past", Token: lic("past", now-30), Key: extfile.Fingerprint(&m.licKey.PublicKey), IssuedAt: now - 30,
		ExpiresAt: now - 2, GraceUntil: now - 1, Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:t"}
	if err := m.st.InTenantTx(ctx, tid, []string{store.LicenseLock(tid)}, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, tid, "ci")
		if err != nil {
			return err
		}
		past.Entry = store.PublisherSource(p.ID)
		return tx.PutLicense(ctx, &past, 0)
	}); err != nil {
		t.Fatal(err)
	}
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")
	call(204, "DELETE", L+"?source=publisher:ci", tom, "", "If-Match", "*")

	// the issuer rotates its key (K1 to K2): the distributor's entry's K2 licence counts against a
	// stale K1 licence imported into the tenant's own entry, whatever their keys
	k2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.LicenseKey(ctx, m.st, tid, "tresor", store.KeySourcePublisher, "enterest-ci", &k2.PublicKey); err != nil {
		t.Fatal(err)
	}
	etag = call(200, "POST", E, lm, sign(k2, "rotated", now-15), "If-Match", etag).header.Get("ETag")
	older(m.call(t, "POST", CI, tom, lic("stale", now-20)), "publisher:enterest-ci")

	// removed: the distributor's licence still counts (a tenant administrator removing its publisher
	// does not unblock an older one), and its entry, named by its id, is still the distributor's
	call(200, "POST", E, lm, lic("newer", now-10), "If-Match", etag)
	var id string
	for _, p := range call(200, "GET", T+"/publishers", tom, "").json(t)["publishers"].([]any) {
		if x := p.(map[string]any); x["name"] == "enterest-ci" {
			id = x["id"].(string)
		}
	}
	call(204, "DELETE", T+"/publishers/enterest-ci", tom, "")
	older(m.call(t, "POST", CI, tom, lic("old", now-20)), "publisher-id:"+id)
	refused(m.call(t, "DELETE", L+"?source=publisher-id:"+id, tom, "", "If-Match", "*"), why)
	// a removed entry takes no import and no visibility change, from anyone (spec 0035)
	for _, c := range []struct{ method, path, body string }{
		{"POST", T + "/licenses?source=publisher-id:" + id, lic("newer", now-10)},
		{"POST", L + "/public?source=publisher-id:" + id, ""}, {"POST", L + "/private?source=publisher-id:" + id, ""},
	} {
		for _, tok := range []string{tom, sa} {
			if r := m.call(t, c.method, c.path, tok, c.body, "If-Match", "*"); r.status != 400 || !strings.Contains(string(r.body), "removed") {
				t.Fatalf("%s %s into a removed entry: %d %s", c.method, c.path, r.status, r.body)
			}
		}
	}
	call(204, "DELETE", L+"?source=publisher-id:"+id, sa, "", "If-Match", "*")

	// the refusals are recorded with what they refused, each operation apart
	m.events.Close(ctx)
	evs, err := m.st.ListEvents(ctx, tid, store.EventFilter{Kind: "authz.refused", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{`"operation":"import"`, `"name":"tresor"`, `"name_unverified":true`, `"source":"publisher:enterest-ci"`, `"entry":"publisher-id:`},
		{`"operation":"remove"`, `"source":"publisher:enterest-ci"`},
		{`"operation":"public"`, `"source":"publisher:enterest-ci"`},
		{`"operation":"private"`, `"source":"publisher:enterest-ci"`},
	} {
		if !slices.ContainsFunc(evs, func(e store.Event) bool {
			return !slices.ContainsFunc(want, func(s string) bool { return !strings.Contains(e.Data, s) })
		}) {
			t.Errorf("no refusal with %v: %+v", want, evs)
		}
	}
	evs, err = m.st.ListEvents(ctx, tid, store.EventFilter{Kind: "license.refused", Limit: 100})
	if err != nil || len(evs) == 0 || !strings.Contains(evs[0].Data, `"rule":"older"`) || !strings.Contains(evs[0].Data, `"issuer":"https://issuer.example"`) {
		t.Fatalf("the older refusals' events: %+v %v", evs, err)
	}
}
