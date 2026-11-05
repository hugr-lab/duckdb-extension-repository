package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Spec 0043: a name's index answers its administrators what the channel purged of it (the pipeline
// counts it as had), on the DuckDB version and platform of its slot, filtered as the rows are; an
// install holder and an anonymous caller get no purged member.
func TestIndexPurged(t *testing.T) {
	m := newMgmt(t)
	const E = "/api/v1/tenants/acme/channels/prod/extensions/tresor"
	ta := m.toks["tenant admin"]
	id := m.ids["tresor-1.0"]
	if r := m.call(t, "POST", E+"/releases/"+id+"/yank", ta, "", "If-Match", "*"); r.status != 200 {
		t.Fatalf("yank: %d %s", r.status, r.body)
	}
	before := m.call(t, "GET", E, ta, "").json(t)
	if p, _ := before["purged"].([]any); p == nil || len(p) != 0 {
		t.Fatalf("nothing purged yet: %v", before["purged"])
	}
	v := ""
	for _, x := range before["releases"].([]any) {
		if x := x.(map[string]any); x["state"] == "yanked" && x["version"] == "1.0" {
			ws, _ := x["would_serve"].([]any)
			if len(ws) > 0 {
				v, _ = ws[0].(string)
			}
		}
	}
	if v == "" {
		t.Fatalf("the yanked release's DuckDB version: %v", before["releases"])
	}
	if r := m.call(t, "DELETE", E+"/releases/"+id, ta, "", "If-Match", "*"); r.status != 204 {
		t.Fatalf("purge: %d %s", r.status, r.body)
	}
	want := `[{"duckdb_version":"` + v + `","platform":"linux_amd64","version":"1.0"}]` // as decoded and marshalled again
	for who, tok := range map[string]string{"tenant admin": ta, "ext admin": m.toks["ext admin"], "channel adm": m.toks["channel adm"]} {
		got, _ := json.Marshal(m.call(t, "GET", E, tok, "").json(t)["purged"])
		if string(got) != want {
			t.Errorf("%s: %s", who, got)
		}
	}
	got, _ := json.Marshal(m.call(t, "GET", E+"?duckdb_version="+v+"&platform=linux_amd64", ta, "").json(t)["purged"])
	if string(got) != want {
		t.Errorf("of the cell: %s", got)
	}
	got, _ = json.Marshal(m.call(t, "GET", E+"?duckdb_version="+v+"&platform=osx_arm64", ta, "").json(t)["purged"])
	if string(got) != `[]` {
		t.Errorf("of another cell: %s", got)
	}
	for who, tok := range map[string]string{"install": m.toks["install"], "anonymous": ""} {
		if r := m.call(t, "GET", E, tok, ""); r.status != 200 || strings.Contains(string(r.body), "purged") {
			t.Errorf("%s: %d %s", who, r.status, r.body)
		}
	}
}

// Spec 0043: the channels' order. A tenant administrator sets it (If-Match optional, 412 when stale)
// and its auditors and channels' administrators read it; a channel's administrator gets 404 on PUT;
// install holders, extension administrators, distribution admins and other tenants' callers get 404;
// a refused order is 400; a PUT that changes nothing writes no event; channel.order carries before and
// after. The public index is unchanged.
func TestChannelOrderRoutes(t *testing.T) {
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
	call(201, "POST", T+"/channels", ta, `{"name":"nightly","kind":"signed"}`)
	call(201, "POST", T+"/channels", ta, `{"name":"core","kind":"passthrough"}`)
	call(201, "POST", T+"/grants", ta, `{"principal":"subject:corp|aud","verbs":["audit"]}`)
	aud := m.keys.sign(t, idpURL, map[string]any{"sub": "aud", "aud": publicURL + "/acme"})

	empty := call(200, "GET", T+"/channel-order", ta, "")
	if string(empty.body) != `{"order":[]}` || empty.header.Get("ETag") == "" {
		t.Fatalf("no order: %s %v", empty.body, empty.header)
	}
	// set, with the empty order's ETag; a stale one is then 412
	chain := `{"order":[{"channel":"staging","next":"prod"},{"channel":"nightly","next":"staging"}]}`
	r := call(200, "PUT", T+"/channel-order", ta, chain, "If-Match", empty.header.Get("ETag"))
	const want = `{"order":[{"channel":"nightly","next":"staging"},{"channel":"staging","next":"prod"}]}`
	if string(r.body) != want || r.header.Get("ETag") == empty.header.Get("ETag") {
		t.Fatalf("the chain: %s %v", r.body, r.header)
	}
	tag := r.header.Get("ETag")
	call(412, "PUT", T+"/channel-order", ta, `{"order":[]}`, "If-Match", empty.header.Get("ETag"))
	// the same order again, without If-Match: no event
	call(200, "PUT", T+"/channel-order", ta, chain)
	// the readers: the tenant's administrators, its auditors, a channel's administrator; the ETag the PUT answered
	for who, tok := range map[string]string{"tenant admin": ta, "auditor": aud, "channel adm": carl, "server admin": m.toks["server admin"]} {
		if r := call(200, "GET", T+"/channel-order", tok, ""); string(r.body) != want || r.header.Get("ETag") != tag {
			t.Fatalf("%s reads %s %v", who, r.body, r.header)
		}
	}
	call(304, "GET", T+"/channel-order", ta, "", "If-None-Match", tag)
	call(200, "GET", T+"/channel-order?type=duckdb_extension", ta, "")
	call(400, "GET", T+"/channel-order?type=nope", ta, "")
	// nobody else reads it, and only the tenant's administrators write it
	for who, tok := range map[string]string{"install": m.toks["install"], "ext admin": m.toks["ext admin"], "no grants": m.toks["no grants"],
		"dist admin": m.toks["server dist admin"]} {
		if r := m.call(t, "GET", T+"/channel-order", tok, ""); r.status != 404 {
			t.Errorf("%s reads: %d %s", who, r.status, r.body)
		}
	}
	for who, tok := range map[string]string{"channel adm": carl, "auditor": aud, "install": m.toks["install"], "dist admin": m.toks["server dist admin"]} {
		if r := m.call(t, "PUT", T+"/channel-order", tok, `{"order":[]}`); r.status != 404 {
			t.Errorf("%s writes: %d %s", who, r.status, r.body)
		}
	}
	// without a token of the tenant: 401
	for _, tok := range []string{"", m.toks["other admin"]} {
		if r := m.call(t, "GET", T+"/channel-order", tok, ""); r.status != 401 {
			t.Errorf("a caller without a token of acme: %d", r.status)
		}
	}
	// the checks: 400 naming the problem, the order unchanged
	for body, says := range map[string]string{
		`{"order":[{"channel":"prod","next":"staging"},{"channel":"staging","next":"prod"}]}`: "cycle",
		`{"order":[{"channel":"prod","next":"prod"}]}`:                                        "its own next",
		`{"order":[{"channel":"nightly","next":"prod"},{"channel":"staging","next":"prod"}]}`: "twice as a next",
		`{"order":[{"channel":"core","next":"prod"}]}`:                                        "passthrough",
		`{"order":[{"channel":"nope","next":"prod"}]}`:                                        "not a channel",
		`{"order":"nightly"}`: "",
	} {
		r := call(400, "PUT", T+"/channel-order", ta, body)
		if !strings.Contains(string(r.body), says) {
			t.Errorf("%s: %s", body, r.body)
		}
	}
	call(400, "PUT", T+"/channel-order?type=nope", ta, `{"order":[]}`)
	if r := call(200, "GET", T+"/channel-order", ta, ""); string(r.body) != want {
		t.Fatalf("refused orders changed it: %s", r.body)
	}
	// cleared
	if r := call(200, "PUT", T+"/channel-order", ta, `{"order":[]}`, "If-Match", "*"); string(r.body) != `{"order":[]}` {
		t.Fatalf("cleared: %s", r.body)
	}
	// the public index is unchanged: the channels' list says nothing of the order
	if r := call(200, "GET", T+"/channels", "", ""); strings.Contains(string(r.body), "next") {
		t.Fatalf("the public channels' list: %s", r.body)
	}

	// the events: one per change, subject tenant:acme, before and after
	evs := call(200, "GET", T+"/events?kind=channel.order", ta, "").json(t)["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("two changes, two events: %v", evs)
	}
	last, first := evs[0].(map[string]any), evs[1].(map[string]any)
	b, _ := json.Marshal(first["data"])
	if first["subject"] != "tenant:acme" || string(b) != `{"after":[{"channel":"nightly","next":"staging"},{"channel":"staging","next":"prod"}],"before":[]}` {
		t.Fatalf("the first event: %v %s", first, b)
	}
	if b, _ := json.Marshal(last["data"]); string(b) != `{"after":[],"before":[{"channel":"nightly","next":"staging"},{"channel":"staging","next":"prod"}]}` {
		t.Fatalf("the clearing event: %s", b)
	}
}
