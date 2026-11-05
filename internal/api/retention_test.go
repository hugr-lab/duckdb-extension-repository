package api_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: the retention routes' rights: a tenant's rule is its administrators' (its auditors read
// it), a channel's rule, plan and pins its administrators' (the tenant's auditors read them), the
// line act its administrators'; anyone else 404, and an auditor writes nothing.
func TestRetentionRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	const C = T + "/channels/prod/retention"
	ta, carl, eve, ivan := m.toks["tenant admin"], m.toks["channel adm"], m.toks["ext admin"], m.toks["install"]
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|aud","verbs":["audit"]}`); r.status != 201 {
		t.Fatalf("an auditor's grant: %d %s", r.status, r.body)
	}
	aud := m.keys.sign(t, idpURL, map[string]any{"sub": "aud", "aud": publicURL + "/acme"})
	for _, tc := range []struct {
		method, path, body string
		ok, no             []string
	}{
		{"GET", T + "/retention", "", []string{ta, aud}, []string{carl, eve, ivan}},
		{"PUT", T + "/retention", `{"keep":3,"grace":"720h"}`, []string{ta}, []string{aud, carl, eve, ivan}},
		{"GET", C, "", []string{carl, ta, aud}, []string{eve, ivan}},
		{"PUT", C, `{"keep":5}`, []string{carl, ta}, []string{aud, eve, ivan}},
		{"GET", C + "/plan", "", []string{carl, ta, aud}, []string{eve, ivan}},
		{"GET", C + "/pins", "", []string{carl, ta, aud}, []string{eve, ivan}},
		{"POST", C + "/pins", `{"name":"tresor","version":"1.0","reason":"LTS"}`, []string{carl}, []string{aud, eve, ivan}},
		{"POST", C + "/lines", `{"duckdb_version":"v1.9.0","name":"nothing"}`, []string{carl, ta}, []string{aud, eve, ivan}},
	} {
		for _, tok := range tc.no {
			if r := m.call(t, tc.method, tc.path, tok, tc.body); r.status != 404 {
				t.Errorf("%s %s by a caller without the right: %d", tc.method, tc.path, r.status)
			}
		}
		for _, tok := range tc.ok {
			if r := m.call(t, tc.method, tc.path, tok, tc.body); r.status/100 != 2 {
				t.Errorf("%s %s: %d %s", tc.method, tc.path, r.status, r.body)
			}
		}
	}
	// a passthrough channel takes no rule, pin or line act
	must := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.ten.CreateChannel(ctx, admin, "acme", "core", store.ChannelPassthrough, ""))
	for _, x := range [][2]string{{"PUT", ""}, {"POST", "/pins"}, {"POST", "/lines"}} {
		body := map[string]string{"": `{"keep":1}`, "/pins": `{"name":"json","version":"1","reason":"x"}`, "/lines": `{"duckdb_version":"v2.0.0"}`}[x[1]]
		if r := m.call(t, x[0], T+"/channels/core/retention"+x[1], ta, body); r.status != 400 {
			t.Errorf("%s %s on a passthrough channel: %d", x[0], x[1], r.status)
		}
	}
}

// Spec 0042: a PUT is the whole rule (a field left out unset; at a tenant "none" is leaving keep
// out, at a channel no retention there), answering the GET's body with an ETag; If-Match guards it;
// an unchanged rule writes no event; keep above its column, a fraction, a grace not of whole seconds
// and one above the ceiling are refused; in_force gives each field's value and where it comes from.
func TestRetentionRules(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme/retention"
	const C = "/api/v1/tenants/acme/channels/prod/retention"
	ta := m.toks["tenant admin"]
	put := func(path, body string, want int, hdr ...string) map[string]any {
		t.Helper()
		r := m.call(t, "PUT", path, ta, body, hdr...)
		if r.status != want {
			t.Fatalf("PUT %s %s: %d, want %d: %s", path, body, r.status, want, r.body)
		}
		if want != 200 {
			return nil
		}
		if r.header.Get("ETag") == "" {
			t.Fatalf("PUT %s: no ETag", path)
		}
		return r.json(t)
	}
	if got := put(T, `{"keep":3,"grace":"720h"}`, 200); got["keep"] != float64(3) || got["grace"] != "720h" {
		t.Fatalf("the tenant's: %v", got)
	}
	tag := m.call(t, "GET", T, ta, "").header.Get("ETag")
	put(T, `{"keep":3,"grace":"720h"}`, 200, "If-Match", tag) // unchanged
	if n := len(m.eventsOf(t, "retention.set")); n != 1 {
		t.Fatalf("an unchanged rule wrote: %d", n)
	}
	put(T, `{"keep":4}`, 412, "If-Match", `"r9.9"`)
	if got := put(T, `{"keep":"none","grace":"720h"}`, 200); got["keep"] != nil || got["grace"] != "720h" {
		t.Fatalf("none at a tenant is no keep: %v", got)
	}
	if got := put(T, `{"keep":3}`, 200); got["grace"] != nil {
		t.Fatalf("a field left out is unset: %v", got)
	}
	for _, b := range []string{`{"keep":2147483648}`, `{"keep":0}`, `{"keep":1.5}`, `{"keep":"3"}`, `{"grace":"1.5s"}`, `{"grace":"-1h"}`,
		`{"grace":"forever"}`, `{"grace":"2161h"}`, `{"other":1}`} {
		put(T, b, 400)
	}
	// the channel: keep from it, grace from the tenant (720h) once set
	put(T, `{"keep":3,"grace":"720h"}`, 200)
	got := put(C, `{"keep":5}`, 200)
	inf := got["in_force"].(map[string]any)
	if got["keep"] != float64(5) || got["grace"] != nil || inf["keep"].(map[string]any)["from"] != "channel" ||
		inf["grace"].(map[string]any)["from"] != "tenant" || inf["grace"].(map[string]any)["value"] != "720h" {
		t.Fatalf("the channel's: %v", got)
	}
	got = put(C, `{"keep":"none"}`, 200)
	if got["keep"] != "none" || got["in_force"].(map[string]any)["keep"].(map[string]any)["value"] != "none" {
		t.Fatalf("none at a channel: %v", got)
	}
	got = put(C, `{}`, 200)
	if got["keep"] != nil || got["in_force"].(map[string]any)["keep"].(map[string]any)["from"] != "tenant" {
		t.Fatalf("the tenant's entirely: %v", got)
	}
	put(T, `{}`, 200)
	got = m.call(t, "GET", C, ta, "").json(t)
	if missing := got["in_force"].(map[string]any)["missing"].([]any); len(missing) != 2 {
		t.Fatalf("nothing in force: %v", got)
	}
	if ev := m.eventsOf(t, "retention.set"); ev[0].Subject != "tenant:acme" || ev[1].Subject != "channel:prod" ||
		!strings.Contains(ev[1].Data, `"in_force"`) {
		t.Fatalf("retention.set: %+v", ev[:2])
	}
}

func (m *mgmt) eventsOf(t *testing.T, kind string) []store.Event {
	t.Helper()
	tn, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := m.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: kind, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// Spec 0042: the plan lists what the next pass retires (with its lines, bytes and why), the retired
// releases past their grace (those a pin keeps marked) and those in it, paged by (name, version);
// shared_by names a body's holders among the caller's channels only, the others counted. Pins answer
// 201, then 200 for the same pin.
func TestRetentionPlanAndPins(t *testing.T) {
	m := newMgmt(t)
	const C = "/api/v1/tenants/acme/channels/prod/retention"
	ta, carl := m.toks["tenant admin"], m.toks["channel adm"]
	ids := map[string]string{}
	for i, v := range []string{"1.0", "2.0", "3.0"} {
		r, _, err := m.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(ext(t, uint64(40+i), cpp(v))), release.AddOptions{Unchecked: true, Name: "geo"})
		if err != nil {
			t.Fatal(err)
		}
		ids[v] = r.ID
	}
	// geo 1.0's body is staging's too
	if _, _, err := m.rel.Add(ctx, admin, "acme", "staging", bytes.NewReader(ext(t, 40, cpp("1.0"))), release.AddOptions{Unchecked: true, Name: "geo"}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "PUT", C, carl, `{"keep":1,"grace":"0s"}`); r.status != 200 {
		t.Fatalf("the rule: %d %s", r.status, r.body)
	}
	plan := func(tok, q string) map[string]any {
		t.Helper()
		r := m.call(t, "GET", C+"/plan"+q, tok, "")
		if r.status != 200 {
			t.Fatalf("plan: %d %s", r.status, r.body)
		}
		return r.json(t)
	}
	retire := func(p map[string]any) []map[string]any {
		var out []map[string]any
		for _, x := range p["retire"].([]any) {
			if x := x.(map[string]any); x["name"] == "geo" {
				out = append(out, x)
			}
		}
		return out
	}
	p := retire(plan(ta, ""))
	if len(p) != 2 || p[0]["version"] != "1.0" || p[1]["version"] != "2.0" || p[0]["lines"].([]any)[0] != "v2.0.0" ||
		p[0]["bytes"].(float64) <= 0 || p[0]["kept"].(map[string]any)["v2.0.0"].([]any)[0] != "3.0" {
		t.Fatalf("the plan's retire: %v", p)
	}
	rel := p[0]["releases"].([]any)[0].(map[string]any)
	if rel["shared"] != true || len(rel["shared_by"].([]any)) != 1 || rel["shared_by"].([]any)[0] != "staging" || rel["others"] != float64(0) {
		t.Fatalf("a tenant administrator's shared_by: %v", rel)
	}
	rel = retire(plan(carl, ""))[0]["releases"].([]any)[0].(map[string]any)
	if rel["shared"] != true || len(rel["shared_by"].([]any)) != 0 || rel["others"] != float64(1) {
		t.Fatalf("a channel administrator's shared_by: %v", rel)
	}
	if rel := p[1]["releases"].([]any)[0].(map[string]any); rel["shared"] != false {
		t.Fatalf("2.0's body is prod's alone: %v", rel)
	}
	// paging by (name, version)
	page := plan(ta, "?limit=1")
	if len(page["retire"].([]any)) != 1 || page["next"] == nil {
		t.Fatalf("a page: %v", page)
	}
	// the pass, then the plan's purge (grace 0: past it at once) and a pin keeping one
	if _, err := m.apiRel.RetainChannel(ctx, m.channelID(t, "prod"), nil); err != nil {
		t.Fatal(err)
	}
	r := m.call(t, "POST", C+"/pins", carl, `{"name":"geo","version":"1.0","reason":"LTS nodes"}`)
	if r.status != 201 || r.json(t)["duckdb_version"] != nil || r.json(t)["reason"] != "LTS nodes" || r.header.Get("Location") == "" {
		t.Fatalf("a pin: %d %s", r.status, r.body)
	}
	pinID := r.json(t)["id"].(string)
	if r := m.call(t, "POST", C+"/pins", carl, `{"name":"geo","version":"1.0","reason":"again"}`); r.status != 200 || r.json(t)["id"] != pinID {
		t.Fatalf("the same pin: %d %s", r.status, r.body)
	}
	for _, b := range []string{`{"name":"geo","version":"9.9","reason":"x"}`, `{"name":"geo","version":"1.0","duckdb_version":"v1.9.0","reason":"x"}`,
		`{"name":"geo","version":"1.0","reason":""}`} {
		if r := m.call(t, "POST", C+"/pins", carl, b); r.status != 400 {
			t.Errorf("a pin %s: %d", b, r.status)
		}
	}
	got := plan(ta, "")
	purge := map[string]bool{}
	for _, x := range got["purge"].([]any) {
		x := x.(map[string]any)
		purge[x["version"].(string)] = x["pinned"].(bool)
	}
	if len(purge) != 2 || !purge["1.0"] || purge["2.0"] || len(got["retire"].([]any)) != 0 {
		t.Fatalf("the plan's purge: %v", got)
	}
	pins := m.call(t, "GET", C+"/pins", carl, "").json(t)["pins"].([]any)
	if len(pins) != 1 || pins[0].(map[string]any)["pinned_by"] == "" {
		t.Fatalf("pins: %v", pins)
	}
	if r := m.call(t, "DELETE", "/api/v1/tenants/acme/channels/staging/retention/pins/"+pinID, ta, ""); r.status != 404 {
		t.Fatalf("another channel's pin: %d", r.status)
	}
	if r := m.call(t, "DELETE", C+"/pins/"+pinID, carl, ""); r.status != 204 {
		t.Fatalf("unpin: %d", r.status)
	}
	// the line act answers what it retired and kept
	r = m.call(t, "POST", C+"/lines", carl, `{"duckdb_version":"v2.0.0","name":"geo"}`)
	var line struct {
		Retired []map[string]any `json:"retired"`
		Kept    []map[string]any `json:"kept"`
	}
	if err := json.Unmarshal(r.body, &line); r.status != 200 || err != nil || len(line.Retired) != 1 || line.Retired[0]["id"] != ids["3.0"] {
		t.Fatalf("the line act: %d %s", r.status, r.body)
	}
}

func (m *mgmt) channelID(t *testing.T, name string) string {
	t.Helper()
	c, err := m.st.GetChannel(ctx, "acme", name)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}
