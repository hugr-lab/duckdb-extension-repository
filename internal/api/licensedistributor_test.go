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
// and is refused with a 404 of its own type that says why, recorded with what it was. A licence older
// (by iat) than one the tenant holds from the same issuer for the same holder, in any entry, is
// refused (rule older), before the stored one is compared; an equal iat is not older, and a stored
// licence past its grace or whose key is dead does not count.
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
	distributorsEntry := func(r resp) {
		t.Helper()
		if r.status != 404 || r.json(t)["type"] != "urn:kista:problem:distributors-entry" ||
			r.json(t)["detail"] != "a distributor's entry: its licence manager or a server administrator writes it" {
			t.Fatalf("not refused as a distributor's entry: %d %s", r.status, r.body)
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
	const E = T + "/licenses?source=publisher:enterest-ci"
	const L = T + "/licenses/tresor"
	const Q = "?source=publisher:enterest-ci"

	// the writers of a distributor's entry: not a tenant administrator
	distributorsEntry(m.call(t, "POST", E, tom, lic("pro", now-60)))
	r := call(201, "POST", E, lm, lic("pro", now-60))
	etag := r.header.Get("ETag")
	if r := call(200, "GET", L+Q, tom, ""); r.json(t)["distributor"] != "enterest" {
		t.Fatalf("the tenant administrator's read: %s", r.body)
	}
	if r := call(200, "GET", T+"/licenses", tom, ""); !strings.Contains(string(r.body), `"distributor":"enterest"`) {
		t.Fatalf("the list: %s", r.body)
	}
	distributorsEntry(m.call(t, "POST", E, tom, lic("pro", now-60), "If-Match", etag))
	distributorsEntry(m.call(t, "DELETE", L+Q, tom, "", "If-Match", etag))
	distributorsEntry(m.call(t, "POST", L+"/public"+Q, tom, "", "If-Match", etag))
	distributorsEntry(m.call(t, "POST", L+"/private"+Q, tom, "", "If-Match", etag))
	// a licence manager writes it, never public; a server administrator writes everything
	if r := call(404, "POST", L+"/public"+Q, lm, "", "If-Match", etag); r.json(t)["type"] != "urn:kista:problem:not-found" {
		t.Fatalf("public by a licence manager: %s", r.body)
	}
	etag = call(200, "POST", L+"/public"+Q, sa, "", "If-Match", etag).header.Get("ETag")
	etag = call(200, "POST", L+"/private"+Q, lm, "", "If-Match", etag).header.Get("ETag")

	// older: refused on an import and a replacement, before the stored licence is compared (no 409)
	for _, hdr := range [][]string{nil, {"If-Match", etag}} {
		if r := call(400, "POST", E, lm, lic("old", now-120), hdr...); !strings.Contains(r.json(t)["detail"].(string), "the licence is refused (older)") ||
			!strings.Contains(r.json(t)["detail"].(string), "publisher:enterest-ci") {
			t.Fatalf("an older licence: %s", r.body)
		}
	}
	// the same file again: an equal iat is not older
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")
	// in another entry where the key is live, the tenant's own: refused while the later one is stored
	if r := call(400, "POST", T+"/licenses?source=publisher:ci", tom, lic("old", now-120)); !strings.Contains(string(r.body), "(older)") {
		t.Fatalf("an older licence into another entry: %s", r.body)
	}
	call(201, "POST", T+"/licenses?source=publisher:ci", tom, lic("ci", now-60))
	// a later licence whose key is live in its entry counts; once the server refuses its key, not
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	twin := m.declare(t, "twin", "d", &other.PublicKey)
	later := store.License{TenantID: tid, PackageType: pkgtype.Default, Name: "tresor", Entry: store.PublisherSource(twin), LicenseID: "lic-later", Issuer: "https://issuer.example",
		HolderID: "acme", Plan: "later", Token: sign(other, "later", now-30), Key: extfile.Fingerprint(&other.PublicKey), IssuedAt: now - 30,
		ExpiresAt: now + 86400, GraceUntil: now + 2*86400, Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:t"}
	put := func(l store.License) {
		t.Helper()
		if err := m.st.InTenantTx(ctx, tid, []string{store.LicenseLock(tid)}, func(tx *store.Tx) error { return tx.PutLicense(ctx, &l, 0) }); err != nil {
			t.Fatal(err)
		}
	}
	put(later)
	if r := call(400, "POST", E, lm, lic("pro", now-60), "If-Match", etag); !strings.Contains(string(r.body), "(older)") ||
		!strings.Contains(string(r.body), "publisher:twin") {
		t.Fatalf("a later licence with a live key: %s", r.body)
	}
	m.lic.Refuse = map[string]bool{later.Key: true}
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")
	m.lic.Refuse = nil
	// a later licence past its grace counts neither
	if err := m.st.InTenantTx(ctx, tid, []string{store.LicenseLock(tid)}, func(tx *store.Tx) error {
		l, err := tx.GetLicense(ctx, tid, later.PackageType, "tresor", later.Entry)
		if err != nil {
			return err
		}
		return tx.DeleteLicense(ctx, l, 0)
	}); err != nil {
		t.Fatal(err)
	}
	later.GraceUntil, later.ExpiresAt = now-1, now-2
	put(later)
	etag = call(200, "POST", E, lm, lic("pro", now-60), "If-Match", etag).header.Get("ETag")

	// removed: its entry, named by its id, is still the distributor's
	var id string
	for _, p := range call(200, "GET", T+"/publishers", tom, "").json(t)["publishers"].([]any) {
		if x := p.(map[string]any); x["name"] == "enterest-ci" {
			id = x["id"].(string)
		}
	}
	call(204, "DELETE", T+"/publishers/enterest-ci", tom, "")
	distributorsEntry(m.call(t, "DELETE", L+"?source=publisher-id:"+id, tom, "", "If-Match", "*"))
	call(204, "DELETE", L+"?source=publisher-id:"+id, sa, "", "If-Match", "*")

	// the refusals are recorded with what they refused
	m.events.Close(ctx)
	evs, err := m.st.ListEvents(ctx, tid, store.EventFilter{Kind: "authz.refused", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e store.Event) bool {
		return strings.Contains(e.Data, `"operation":"import"`) && strings.Contains(e.Data, `"name":"tresor"`) &&
			strings.Contains(e.Data, `"source":"publisher:enterest-ci"`) && strings.Contains(e.Data, `"route":"POST tenants/{t}/licenses"`)
	}) {
		t.Fatalf("the refusal's event: %+v", evs)
	}
}
