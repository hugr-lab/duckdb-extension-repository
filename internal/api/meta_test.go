package api_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func pngIcon(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 3)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func iconBody(summary string, data []byte) string {
	return `{"summary":"` + summary + `","icon":{"content_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(data) + `"}}`
}

// Spec 0036 phase 1: the metadata and icon routes. Their access rule admits any verified caller of
// the tenant, a signed request from any of its publishers included, and the service decides by the
// entry's source; every refusal is a 404. A write replaces the whole metadata; the icon is served
// with kista's own headers; the catalogue answers the metadata; the bounds, the body limit, stale
// and changed writes and a full catalogue each have their answer.
func TestEntryMetaRoutes(t *testing.T) {
	m := newMgmt(t)
	m.apiH.SetMetaRate(1e6, 1e6) // the actor's rate is tested at the end
	da, ta, sa := m.toks["server dist admin"], m.toks["tenant admin"], m.toks["server admin"]
	const T = "/api/v1/tenants/acme"
	call := func(want int, method, path, tok, body string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	ownID := call(201, "POST", T+"/publishers", ta, `{"name":"own-ci"}`).json(t)["id"].(string)
	dist := call(201, "POST", T+"/publishers", da, `{"name":"enterest-ci","distributor":"enterest"}`).json(t)["id"].(string)
	key := m.addKey(t, da, "acme", "enterest-ci")
	ownKey := m.addKey(t, sa, "acme", "own-ci")
	call(201, "POST", T+"/grants", sa, `{"principal":"publisher:own-ci","verbs":["publish"]}`)
	q := "?version=1.0&platform=linux_amd64"
	for _, x := range []struct{ path, handle, name string }{
		{T + "/catalogue/extensions/geo_tools/builds" + q, key, "geo_tools"},
		{T + "/catalogue/extensions/own_ext/builds" + q, ownKey, "own_ext"},
	} {
		if r := m.upload(t, x.path, x.handle, built(t, "1.0", x.name+"_duckdb_cpp_init")); r.status != 201 {
			t.Fatalf("publish %s: %d %s", x.name, r.status, r.body)
		}
	}
	own, geo := T+"/catalogue/publisher:own-ci/extensions/own_ext", T+"/catalogue/publisher:enterest-ci/extensions/geo_tools"

	// the tenant's own publisher, signed, its own entry: no set_by in its answer
	r := call(200, "PUT", own+"/meta", ownKey, `{"summary":"Own tools"}`).json(t)
	if r["summary"] != "Own tools" || r["icon"] != nil || r["set_by"] != nil || r["signed_at"] == nil {
		t.Fatalf("a publisher's write: %v", r)
	}
	// a tenant administrator: the tenant's source, with an icon; set_by answered
	ic := pngIcon(t, 64)
	r = call(200, "PUT", own+"/meta", ta, iconBody("Own tools", ic)).json(t)
	icon, _ := r["icon"].(map[string]any)
	if icon == nil || icon["width"] != float64(64) || icon["sha256"] == "" || r["set_by"] == nil {
		t.Fatalf("an administrator's write: %v", r)
	}
	sha := icon["sha256"].(string)
	if got := call(200, "GET", own+"/meta", ta, "").json(t); got["summary"] != "Own tools" || got["icon"] == nil {
		t.Fatalf("read back: %v", got)
	}
	// the icon: kista's type and headers, the bytes as sent
	ir := call(200, "GET", own+"/icon", ta, "")
	for h, want := range map[string]string{"Content-Type": "image/png", "X-Content-Type-Options": "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox", "Cross-Origin-Resource-Policy": "same-origin",
		"Content-Disposition": `inline; filename="icon.png"`, "Cache-Control": "private, no-store"} {
		if got := ir.header.Get(h); got != want {
			t.Errorf("%s: %q", h, got)
		}
	}
	if !bytes.Equal(ir.body, ic) {
		t.Fatal("the icon's bytes")
	}
	// the summary alone, the icon kept by its hash; a hash that changed is a conflict
	call(200, "PUT", own+"/meta", ta, `{"summary":"Own","icon":{"sha256":"`+sha+`"}}`)
	if r := call(409, "PUT", own+"/meta", ta, `{"summary":"Own","icon":{"sha256":"`+strings.Repeat("0", 64)+`"}}`); !strings.Contains(string(r.body), "problem:conflict") {
		t.Fatalf("a changed icon: %s", r.body)
	}

	// a distributor's entry: the tenant's administrators read it and never write it; its
	// distribution admin and its publisher write it, by either name
	call(404, "PUT", geo+"/meta", ta, `{"summary":"Restyled"}`)
	call(200, "GET", geo+"/meta", ta, "")
	if r := call(200, "PUT", geo+"/meta", da, iconBody("Geometry", ic)).json(t); r["distributor"] != "enterest" || r["set_by"] == nil {
		t.Fatalf("a distribution admin's write: %v", r)
	}
	call(404, "PUT", own+"/meta", da, `{"summary":"x"}`)
	// a token's write moved the mark to the server's clock: a request signed before it is stale, one
	// signed after it is not
	m.signers[key].at = time.Now().Add(-time.Second)
	if r := m.call(t, "PUT", geo+"/meta", key, `{"summary":"Geometry tools"}`); r.status != 409 || !strings.Contains(string(r.body), "problem:stale") {
		t.Fatalf("a request signed before the token's write: %d %s", r.status, r.body)
	}
	m.signers[key].at = time.Now().Add(2 * time.Second)
	call(200, "PUT", T+"/catalogue/publisher-id:"+dist+"/extensions/geo_tools/meta", key, `{"summary":"Geometry tools"}`)
	call(404, "PUT", own+"/meta", key, `{"summary":"x"}`)
	call(404, "PUT", geo+"/meta", ownKey, `{"summary":"x"}`)
	// the signer's own entry named by another source's label: refused, even where that label's
	// source has no entry of the name
	call(404, "PUT", T+"/catalogue/publisher:own-ci/extensions/geo_tools/meta", key, `{"summary":"x"}`)
	call(404, "PUT", T+"/catalogue/tenant/extensions/geo_tools/meta", key, `{"summary":"x"}`)
	if r := call(200, "GET", geo+"/meta", key, "").json(t); r["summary"] != "Geometry tools" || r["icon"] != nil || r["set_by"] != nil {
		t.Fatalf("a signed read, the icon cleared by the write that named none: %v", r)
	}
	call(404, "GET", geo+"/icon", key, "")
	// another body in the same second is stale: each signed write signs later
	if r := m.call(t, "PUT", geo+"/meta", key, iconBody("Geometry tools", ic)); r.status != 409 || !strings.Contains(string(r.body), "problem:stale") {
		t.Fatalf("another body of the same second: %d %s", r.status, r.body)
	}
	m.signers[key].at = m.signers[key].at.Add(time.Second)
	call(200, "PUT", geo+"/meta", key, iconBody("Geometry tools", ic))
	if r := call(200, "GET", geo+"/icon", key, ""); !bytes.Equal(r.body, ic) {
		t.Fatal("a signed icon read")
	}
	// refusals: unknown entries and sources, callers without rights, anonymous
	call(404, "GET", T+"/catalogue/publisher:own-ci/extensions/nope/meta", ta, "")
	call(404, "GET", T+"/catalogue/upstream:nope/extensions/own_ext/meta", ta, "")
	call(404, "GET", own+"/meta", m.toks["no grants"], "")
	call(404, "GET", own+"/meta", m.toks["server user"], "")
	call(404, "GET", own+"/meta", m.toks["server licence mgr"], "")
	call(401, "GET", own+"/meta", "", "")
	call(401, "GET", "/api/v1/tenants/other/catalogue/publisher:own-ci/extensions/own_ext/meta", ta, "") // not a token of that tenant

	// the bounds
	for name, body := range map[string]string{
		"a line break":     `{"summary":"two\nlines"}`,
		"161 characters":   `{"summary":"` + strings.Repeat("x", 161) + `"}`,
		"a filler":         `{"summary":"a` + "\xe3\x85\xa4" + `b"}`, // U+3164
		"not base64":       `{"icon":{"content_type":"image/png","data":"%%%"}}`,
		"a JPEG type":      `{"icon":{"content_type":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString(ic) + `"}}`,
		"too small":        iconBody("x", pngIcon(t, 32)),
		"new and kept":     `{"icon":{"content_type":"image/png","data":"AA==","sha256":"` + sha + `"}}`,
		"an unknown field": `{"summary":"x","homepage":"https://example.com"}`,
		"SVG":              `{"icon":{"content_type":"image/png","data":"` + base64.StdEncoding.EncodeToString([]byte("<svg/>")) + `"}}`,
		"no content type":  `{"icon":{"data":"AA=="}}`,
	} {
		if r := m.call(t, "PUT", own+"/meta", ta, body); r.status != 400 || !strings.Contains(string(r.body), "problem:invalid") {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	big := `{"summary":"` + strings.Repeat("x", 400<<10) + `"}`
	if r := m.call(t, "PUT", own+"/meta", ta, big); r.status != 413 || !strings.Contains(string(r.body), "the body is at most 384 KiB") {
		t.Fatalf("over the body limit: %d %s", r.status, r.body)
	}
	// another route keeps 64 KiB
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"`+strings.Repeat("x", 70<<10)+`"}`); r.status != 413 ||
		!strings.Contains(string(r.body), "the body is at most 64 KiB") {
		t.Fatalf("another route's limit: %d %s", r.status, r.body)
	}

	// stale: a request signed before the entry's last signed write
	m.signers[key].at = time.Now().Add(-2 * time.Minute)
	if r := m.call(t, "PUT", geo+"/meta", key, `{"summary":"older"}`); r.status != 409 || !strings.Contains(string(r.body), "problem:stale") {
		t.Fatalf("a stale request: %d %s", r.status, r.body)
	}
	m.signers[key].at = time.Now().Add(10 * time.Second) // past every signed write above

	// a full catalogue: the distributor's publisher and its distribution admin are bounded
	m.apiRel.MaxBytes = 1
	for _, tok := range []string{key, da} {
		if r := m.call(t, "PUT", geo+"/meta", tok, iconBody("Geometry tools", pngIcon(t, 96))); r.status != 429 ||
			!strings.Contains(string(r.body), "problem:catalogue-full") || r.header.Get("Retry-After") != "" {
			t.Fatalf("beyond the cap: %d %s", r.status, r.body)
		}
	}
	call(200, "PUT", own+"/meta", ta, iconBody("Own", pngIcon(t, 96))) // an administrator is not bounded
	m.apiRel.MaxBytes = 0

	// the catalogue's reads answer the metadata and a distributor's entry's distributor
	cat := call(200, "GET", T+"/catalogue", ta, "")
	var geoEntry, ownEntry map[string]any
	for _, e := range cat.json(t)["entries"].([]any) {
		switch x := e.(map[string]any); x["name"] {
		case "geo_tools":
			geoEntry = x
		case "own_ext":
			ownEntry = x
		}
	}
	if geoEntry["distributor"] != "enterest" || geoEntry["summary"] != "Geometry tools" || geoEntry["icon"] == nil {
		t.Fatalf("the distributor's entry: %v", geoEntry)
	}
	if ownEntry["distributor"] != nil || ownEntry["summary"] != "Own" {
		t.Fatalf("the tenant's entry: %v", ownEntry)
	}
	if e := call(200, "GET", geo, ta, "").json(t); e["distributor"] != "enterest" || e["summary"] != "Geometry tools" || e["icon"] == nil {
		t.Fatalf("the entry's read: %v", e)
	}
	if e := call(200, "GET", geo, key, "").json(t); e["summary"] != "Geometry tools" {
		t.Fatalf("the entry's signed read: %v", e)
	}

	// the events: the kind is listed; a signed write names its key; the distribution admin's
	if r := call(200, "GET", "/api/v1/event-kinds", "", ""); !strings.Contains(string(r.body), "catalogue.meta") {
		t.Fatal("event-kinds without catalogue.meta")
	}
	evs := call(200, "GET", T+"/events?kind=catalogue.meta", ta, "")
	if !strings.Contains(string(evs.body), `"key":"`+m.signers[key].fp+`"`) || !strings.Contains(string(evs.body), "distribution-admin") {
		t.Fatalf("the events: %s", evs.body)
	}

	// the actor's rate: a burst of one, then 429 with Retry-After; a write refused before its body is
	// read takes none
	m.apiH.SetMetaRate(0.001, 1)
	call(404, "PUT", geo+"/meta", ta, `{"summary":"x"}`)
	call(200, "PUT", own+"/meta", ta, `{"summary":"Own"}`)
	if r := m.call(t, "PUT", own+"/meta", ta, `{"summary":"Own"}`); r.status != 429 || r.header.Get("Retry-After") == "" ||
		!strings.Contains(string(r.body), "problem:rate-limited") {
		t.Fatalf("over the actor's rate: %d %s", r.status, r.body)
	}
	// a captured request replayed (stale) spends none of its publisher's rate
	m.signers[key].at = time.Now().Add(-4 * time.Minute)
	for range 3 {
		call(409, "PUT", geo+"/meta", key, `{"summary":"replayed"}`)
	}
	m.signers[key].at = time.Now().Add(4 * time.Minute)
	call(200, "PUT", geo+"/meta", key, `{"summary":"Geometry tools"}`)
	m.signers[key].at = time.Now().Add(4*time.Minute + time.Second)
	call(429, "PUT", geo+"/meta", key, `{"summary":"Geometry tools, again"}`)
	m.apiH.SetMetaRate(1e6, 1e6)

	// a row whose object is missing: the icon is not found (and logged), never served
	tn, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: "publisher:" + ownID,
			Name: "own_ext", IconSHA256: strings.Repeat("e", 64), IconWidth: 64, IconHeight: 64, IconBytes: 10, SetBy: "os:1:t",
			SetAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	call(404, "GET", own+"/icon", ta, "")

	// a stale request is a refused event (written by the asynchronous writer, flushed by its close)
	m.events.Close(ctx)
	if evs := call(200, "GET", T+"/events?kind=catalogue.meta&outcome=refused", ta, ""); !strings.Contains(string(evs.body), `"reason":"stale"`) ||
		!strings.Contains(string(evs.body), `"key":"`+m.signers[key].fp+`"`) {
		t.Fatalf("the stale write's event: %s", evs.body)
	}
}
