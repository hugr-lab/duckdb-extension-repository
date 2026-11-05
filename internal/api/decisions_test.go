package api_test

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// catalogueEntry lands a version of a name in acme's catalogue from the tenant's own source.
func (m *mgmt) catalogueEntry(t *testing.T, name, version string) store.Build {
	t.Helper()
	b, _, err := m.apiRel.AddToCatalogue(ctx, admin, "acme", bytes.NewReader(built(t, version, name+"_duckdb_cpp_init")), release.AddOptions{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Spec 0041: the decision routes. A tenant administrator writes and replaces (201, then 200) and
// removes with If-Match (412 when stale); its auditors read every decision, a channel's
// administrators that channel's, and nobody else anything; ids of another scope are 404; a
// situation is stored canonically; the checks answer 400 and 404.
func TestDecisionRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	ta, carl := m.toks["tenant admin"], m.toks["channel adm"]
	call := func(want int, method, path, tok, body string, hdr ...string) resp {
		t.Helper()
		r := m.call(t, method, path, tok, body, hdr...)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	m.catalogueEntry(t, "acl", "1.0")
	m.catalogueEntry(t, "acl", "1.1")
	call(200, "PUT", T+"/channels/prod/sources/acl", ta, `{"source":"tenant","mode":"manual"}`)
	call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|aud","verbs":["audit"]}`)
	aud := m.keys.sign(t, idpURL, map[string]any{"sub": "aud", "aud": publicURL + "/acme"})
	call(201, "POST", T+"/publishers", ta, `{"name":"ci"}`)
	pub := m.addKey(t, ta, "acme", "ci")

	// a skip, then a skip of a newer version replacing it
	r := call(201, "PUT", T+"/decisions", ta, `{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.0"}`)
	skip := r.json(t)
	if r.header.Get("ETag") != `"v1"` || skip["etag"] != `"v1"` || skip["channel"] != "prod" || skip["version"] != "1.0" ||
		skip["situation"] != nil || skip["decided_by"] == "" || skip["decided_at"] == "" || skip["package_type"] != nil {
		t.Fatalf("a skip: %s %v", r.body, r.header)
	}
	r = call(200, "PUT", T+"/decisions", ta, `{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.1"}`)
	if again := r.json(t); again["id"] != skip["id"] || again["etag"] != `"v2"` || again["version"] != "1.1" {
		t.Fatalf("a replacement: %s", r.body)
	}
	// the tenant's dismissal, its channels sent unsorted and twice, stored canonically; sent sorted, it is
	// the same decision
	r = call(201, "PUT", T+"/decisions", ta, `{"kind":"dismiss","source":"tenant","name":"acl","situation":["staging","prod","prod"]}`)
	tenant := r.json(t)
	if b, _ := json.Marshal(tenant["situation"]); string(b) != `["prod","staging"]` || tenant["channel"] != nil {
		t.Fatalf("the tenant's dismissal: %s", r.body)
	}
	call(200, "PUT", T+"/decisions", ta, `{"kind":"dismiss","source":"tenant","name":"acl","situation":["prod","staging"]}`)
	call(201, "PUT", T+"/decisions", ta, `{"kind":"dismiss","channel":"prod","source":"tenant","name":"acl","situation":"reserved"}`)

	// the checks
	for body, want := range map[string]int{
		`{"kind":"later","channel":"prod","source":"tenant","name":"acl","version":"1.0"}`:                                  400,
		`{"kind":"skip","source":"tenant","name":"acl","version":"1.0"}`:                                                    400, // a skip is a channel's
		`{"kind":"skip","channel":"prod","source":"tenant","name":"acl"}`:                                                   400,
		`{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.0","situation":"x"}`:                   400,
		`{"kind":"dismiss","channel":"prod","source":"tenant","name":"acl","version":"1.0","situation":"x"}`:                400,
		`{"kind":"dismiss","channel":"prod","source":"tenant","name":"acl"}`:                                                400,
		`{"kind":"dismiss","channel":"prod","source":"tenant","name":"acl","situation":["prod"]}`:                           400, // a follow skip's is a string
		`{"kind":"dismiss","source":"tenant","name":"acl","situation":"reserved"}`:                                          400, // the tenant's is an array
		`{"kind":"dismiss","source":"tenant","name":"acl","situation":["nope"]}`:                                            400, // not a channel
		`{"kind":"dismiss","channel":"prod","source":"tenant","name":"acl","situation":"` + strings.Repeat("x", 401) + `"}`: 400,
		`{"kind":"skip","channel":"prod","source":"publisher:nope","name":"acl","version":"1.0"}`:                           400,
		`{"kind":"skip","channel":"prod","source":"tenant","name":"Bad Name","version":"1.0"}`:                              400,
		`{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.0","type":"nope"}`:                     400,
		`{"kind":"skip","channel":"prod","source":"publisher:ci","name":"acl","version":"1.0"}`:                             404, // no entry of it
		`{"kind":"skip","channel":"prod","source":"tenant","name":"geo","version":"1.0"}`:                                   404,
		`{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"9.9"}`:                                   404,
		`{"kind":"skip","channel":"nope","source":"tenant","name":"acl","version":"1.0"}`:                                   404,
	} {
		call(want, "PUT", T+"/decisions", ta, body)
	}

	// the reads: every decision with its etag; by channel, type and page; an auditor reads the same
	list := func(r resp) []map[string]any {
		t.Helper()
		var out struct{ Decisions []map[string]any }
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Decisions
	}
	all := call(200, "GET", T+"/decisions", ta, "")
	if ds := list(all); len(ds) != 3 || ds[0]["etag"] == nil || ds[1]["etag"] == nil || ds[2]["etag"] == nil {
		t.Fatalf("the decisions: %s", all.body)
	}
	if a := call(200, "GET", T+"/decisions", aud, ""); !bytes.Equal(a.body, all.body) {
		t.Fatalf("an auditor's: %s", a.body)
	}
	if ds := list(call(200, "GET", T+"/decisions?channel=prod", ta, "")); len(ds) != 2 {
		t.Fatalf("prod's: %v", ds)
	}
	if ds := list(call(200, "GET", T+"/decisions?type=duckdb_extension", ta, "")); len(ds) != 3 {
		t.Fatalf("of the type: %v", ds)
	}
	call(404, "GET", T+"/decisions?channel=nope", ta, "")
	first := call(200, "GET", T+"/decisions?limit=2", ta, "").json(t)
	next, _ := first["next"].(string)
	if rest := list(call(200, "GET", T+"/decisions?limit=2&cursor="+url.QueryEscape(next), ta, "")); next == "" || len(rest) != 1 {
		t.Fatalf("paging: %v %v", first, rest)
	}
	// a channel's administrator reads its channel's, on the channel's route only, and writes nothing
	if ds := list(call(200, "GET", T+"/channels/prod/decisions", carl, "")); len(ds) != 2 || ds[0]["channel"] != "prod" {
		t.Fatalf("prod's administrator: %v", ds)
	}
	if ds := list(call(200, "GET", T+"/channels/staging/decisions", ta, "")); len(ds) != 0 {
		t.Fatalf("staging's: %v", ds)
	}
	call(200, "GET", T+"/channels/prod/decisions", aud, "")
	for _, c := range []struct{ method, path, tok, body string }{
		{"GET", T + "/channels/staging/decisions", carl, ""},
		{"GET", T + "/decisions", carl, ""},
		{"PUT", T + "/decisions", carl, `{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.0"}`},
		{"DELETE", T + "/decisions/" + skip["id"].(string), carl, ""},
		{"PUT", T + "/decisions", aud, `{"kind":"skip","channel":"prod","source":"tenant","name":"acl","version":"1.0"}`},
		{"DELETE", T + "/decisions/" + skip["id"].(string), aud, ""},
		{"GET", T + "/decisions", m.toks["server dist admin"], ""},
		{"GET", T + "/channels/prod/decisions", m.toks["server dist admin"], ""},
		{"GET", T + "/decisions", m.toks["install"], ""},
		{"GET", T + "/channels/prod/decisions", m.toks["install"], ""},
		{"GET", T + "/channels/prod/decisions", m.toks["ext admin"], ""},
		// an id of another tenant: the same as an unknown one
		{"DELETE", "/api/v1/tenants/other/decisions/" + skip["id"].(string), m.toks["other admin"], ""},
		{"DELETE", T + "/decisions/unknown", ta, ""},
	} {
		call(404, c.method, c.path, c.tok, c.body, "If-Match", "*")
	}
	for _, path := range []string{T + "/decisions", T + "/channels/prod/decisions"} {
		if r := m.call(t, "GET", path, pub, ""); r.status != 401 { // not a token here, as on every management route
			t.Fatalf("a publisher reads %s: %d %s", path, r.status, r.body)
		}
	}

	// Undo: If-Match is required, a stale one is 412, the current one removes it
	id := skip["id"].(string)
	call(428, "DELETE", T+"/decisions/"+id, ta, "")
	call(412, "DELETE", T+"/decisions/"+id, ta, "", "If-Match", `"v1"`)
	call(204, "DELETE", T+"/decisions/"+id, ta, "", "If-Match", `"v2"`)
	call(404, "DELETE", T+"/decisions/"+id, ta, "", "If-Match", `"v2"`)

	// the events: decision.set (before when it replaced one) and decision.clear
	evs := call(200, "GET", T+"/events?kind=decision.*", ta, "").json(t)["events"].([]any)
	slices.Reverse(evs) // oldest first
	var got []string
	for _, e := range evs {
		e := e.(map[string]any)
		d := e["data"].(map[string]any)
		got = append(got, e["kind"].(string)+" "+e["subject"].(string)+" "+d["kind"].(string))
		if e["kind"] == "decision.set" && d["version"] == "1.1" && d["before"] != "1.0" {
			t.Errorf("a replacement's before: %v", d)
		}
	}
	if strings.Join(got, ", ") != "decision.set channel:prod/ext:acl skip, decision.set channel:prod/ext:acl skip, decision.set ext:acl dismiss, "+
		"decision.set ext:acl dismiss, decision.set channel:prod/ext:acl dismiss, decision.clear channel:prod/ext:acl skip" {
		t.Fatalf("the events: %v", got)
	}
}

// Spec 0041: a channel's administrators read their channel's events: subject channel:<c> is that
// subject and those under channel:<c>/ (never channel:<c>2's), a longer prefix too, and an event by
// id they could list; install and extension.use never; nothing without such a subject. An auditor
// and a tenant administrator read as before.
func TestChannelAdminEvents(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	ta, carl := m.toks["tenant admin"], m.toks["channel adm"]
	call := func(want int, path, tok string) resp {
		t.Helper()
		r := m.call(t, "GET", path, tok, "")
		if r.status != want {
			t.Fatalf("%s: %d, want %d: %s", path, r.status, want, r.body)
		}
		return r
	}
	m.catalogueEntry(t, "acl", "1.0")
	for _, c := range []struct{ method, path, body string }{
		{"POST", T + "/grants", `{"principal":"subject:corp|aud","verbs":["audit"]}`},
		{"POST", T + "/channels", `{"name":"prod2","kind":"signed"}`},
		{"PUT", T + "/channels/prod/hold", `{"hold":"1h"}`},
		{"PUT", T + "/channels/prod2/hold", `{"hold":"1h"}`},
		{"PUT", T + "/channels/staging/hold", `{"hold":"1h"}`},
		{"PUT", T + "/channels/prod/sources/acl", `{"source":"tenant","mode":"manual"}`},
	} {
		if r := m.call(t, c.method, c.path, ta, c.body); r.status >= 300 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	m.toks["auditor"] = m.keys.sign(t, idpURL, map[string]any{"sub": "aud", "aud": publicURL + "/acme"})
	tid := m.tenantID(t)
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.Event(ctx, tid, "principal:acme/x|ivan", "install", "channel:prod/ext:acl/release:r1",
			map[string]any{"release": "r1", "name": "acl"}); err != nil {
			return err
		}
		return tx.Event(ctx, tid, "principal:acme/x|ivan", "extension.use", "channel:prod/ext:acl", map[string]any{"version": "1.0"})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := func(r resp) (out []string, ids map[string]string) {
		t.Helper()
		ids = map[string]string{}
		for _, e := range r.json(t)["events"].([]any) {
			e := e.(map[string]any)
			k := e["kind"].(string) + " " + e["subject"].(string)
			out, ids[k] = append(out, k), e["id"].(string)
		}
		return out, ids
	}
	// prod's own events and those under it (the fixture's releases too), never prod2's nor the
	// installers'
	got, ids := kinds(call(200, T+"/events?subject=channel:prod", carl))
	for _, k := range got {
		_, subject, _ := strings.Cut(k, " ")
		if subject != "channel:prod" && !strings.HasPrefix(subject, "channel:prod/") || strings.HasPrefix(k, "install ") ||
			strings.HasPrefix(k, "extension.use ") {
			t.Errorf("in prod's events: %s", k)
		}
	}
	if ids["catalogue.hold channel:prod"] == "" || ids["source.set channel:prod/ext:acl"] == "" {
		t.Fatalf("prod's events: %v", got)
	}
	if got, _ := kinds(call(200, T+"/events?subject=channel:prod/ext:acl&kind=source.*", carl)); len(got) == 0 ||
		slices.ContainsFunc(got, func(k string) bool { return k != "source.set channel:prod/ext:acl" }) {
		t.Fatalf("a longer prefix: %v", got)
	}
	if got, _ := kinds(call(200, T+"/events?subject=channel:prod&kind=catalogue.*,install", carl)); strings.Join(got, ", ") != "catalogue.hold channel:prod" {
		t.Fatalf("a kind list naming install too: %v", got)
	}
	if got, _ := kinds(call(200, T+"/events?subject=channel:prod/ext:acl&kind=!source.set", carl)); slices.Contains(got, "source.set channel:prod/ext:acl") ||
		slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "install ") || strings.HasPrefix(k, "extension.use ") }) {
		t.Fatalf("an exclusion: %v", got)
	}
	call(200, T+"/events/"+ids["source.set channel:prod/ext:acl"], carl)
	// the installers' kinds, other channels' events, and reads without the channel's subject: 404
	_, all := kinds(call(200, T+"/events", ta))
	for _, p := range []string{
		T + "/events?subject=channel:prod&kind=install", T + "/events?subject=channel:prod&kind=extension.use",
		T + "/events", T + "/events?subject=channel:", T + "/events?subject=channel:staging", T + "/events?subject=channel:prod2",
		T + "/events?subject=ext:acl", T + "/events?subject=tenant:acme",
		T + "/events/" + all["install channel:prod/ext:acl/release:r1"], T + "/events/" + all["extension.use channel:prod/ext:acl"],
		T + "/events/" + all["catalogue.hold channel:prod2"], T + "/events/" + all["catalogue.hold channel:staging"],
	} {
		call(404, p, carl)
	}
	// an install holder reads nothing; an auditor and a tenant administrator read as before (a bare
	// prefix, every kind)
	call(404, T+"/events?subject=channel:prod", m.toks["install"])
	for _, tok := range []string{ta, m.toks["auditor"]} {
		_, ids := kinds(call(200, T+"/events?subject=channel:prod", tok))
		if ids["catalogue.hold channel:prod2"] == "" || ids["install channel:prod/ext:acl/release:r1"] == "" {
			t.Fatalf("a tenant-wide reader's: %v", ids)
		}
	}
}

// Spec 0041: held_until in the catalogue's list, waiting and waits_until on a rule, and what a
// blocked body is.
func TestCatalogueHomeReads(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	ta := m.toks["tenant admin"]
	call := func(want int, method, path, body string) resp {
		t.Helper()
		r := m.call(t, method, path, ta, body)
		if r.status != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, want, r.body)
		}
		return r
	}
	b := m.catalogueEntry(t, "tiles", "1.0")
	held := m.catalogueEntry(t, "tiles", "1.1")
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if err := store.ExecRaw(ctx, m.st, "UPDATE builds SET available_at = ? WHERE id = ?", store.TimeArg(m.st, until), held.ID); err != nil {
		t.Fatal(err)
	}
	es := call(200, "GET", T+"/catalogue?name=tiles", "").json(t)["entries"].([]any)
	if e := es[0].(map[string]any); len(es) != 1 || e["held_until"].(map[string]any)["1.1"] != until.Format(time.RFC3339) {
		t.Fatalf("held_until: %v", es)
	}
	call(200, "PUT", T+"/channels/prod/sources/tiles", `{"source":"tenant","mode":"manual"}`)
	if s := call(200, "GET", T+"/channels/prod/sources/tiles", "").json(t); s["waiting"] != "1.0" || s["waits_until"] != nil {
		t.Fatalf("a manual rule's waiting: %v", s)
	}
	// an auto rule whose candidate is held: waits_until, nothing waiting
	call(201, "POST", T+"/publishers", `{"name":"ci"}`)
	pid, err := m.st.ResolveSource(ctx, m.tenantID(t), "publisher:ci")
	if err != nil {
		t.Fatal(err)
	}
	geo := store.Build{TenantID: m.tenantID(t), Source: pid, Name: "geo", PackageVersion: "1.0", Platform: "linux_amd64", ABI: store.ABICPP,
		DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat("e", 64), Origin: store.OriginPublication, Checked: true, CreatedBy: "os:1:t"}
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &geo) }); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(ctx, m.st, "UPDATE builds SET available_at = ? WHERE id = ?", store.TimeArg(m.st, until), geo.ID); err != nil {
		t.Fatal(err)
	}
	call(200, "PUT", T+"/channels/prod/sources/geo", `{"source":"publisher:ci","mode":"auto"}`)
	found := false
	for _, x := range call(200, "GET", T+"/channels/prod/sources", "").json(t)["sources"].([]any) {
		if s := x.(map[string]any); s["name"] == "geo" {
			if found = true; s["waits_until"] != until.Format(time.RFC3339) || s["waiting"] != nil {
				t.Fatalf("an auto rule waiting for a hold: %v", s)
			}
		}
	}
	if !found {
		t.Fatal("no rule for geo")
	}
	// a block says what its body is; one the tenant holds nothing of, nothing
	call(201, "POST", T+"/blocks", `{"body_hash":"`+b.BodyHash+`","reason":"CVE"}`)
	call(201, "POST", T+"/blocks", `{"body_hash":"`+strings.Repeat("f", 64)+`","reason":"ahead"}`)
	one := call(200, "GET", T+"/blocks/"+b.BodyHash, "").json(t)
	if bs := one["builds"].([]any); len(bs) != 1 || bs[0].(map[string]any)["source"] != "tenant" || bs[0].(map[string]any)["name"] != "tiles" ||
		bs[0].(map[string]any)["version"] != "1.0" || bs[0].(map[string]any)["platform"] != "linux_amd64" || bs[0].(map[string]any)["slot"] != "v2.0.0" {
		t.Fatalf("a block's builds: %v", one)
	}
	for _, x := range call(200, "GET", T+"/blocks", "").json(t)["blocks"].([]any) {
		x := x.(map[string]any)
		if n := len(x["builds"].([]any)); x["body_hash"] == b.BodyHash && n != 1 || x["body_hash"] != b.BodyHash && n != 0 {
			t.Fatalf("the blocks: %v", x)
		}
	}
}
