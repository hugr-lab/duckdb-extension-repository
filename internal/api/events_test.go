package api_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0010 phase 1a: events, read with audit on the tenant or by server administrators.
func TestEvents(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	// a change through the API writes its event with the request's facts
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|aud","verbs":["audit"]}`, "X-Request-Id", "spoofed"); r.status != 201 {
		t.Fatalf("grant audit: %d %s", r.status, r.body)
	}
	for name, body := range map[string]string{
		"audit on a channel":       `{"principal":"subject:corp|x","verbs":["audit"],"channel":"prod"}`,
		"audit on an extension":    `{"principal":"subject:corp|x","verbs":["audit"],"extension":"acl"}`,
		"audit issuer-wide":        `{"principal":"issuer:corp","verbs":["audit"]}`,
		"audit for a publisher ci": `{"principal":"publisher:nope","verbs":["audit"]}`,
	} {
		if r := m.call(t, "POST", T+"/grants", ta, body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	aud := m.keys.sign(t, idpURL, map[string]any{"sub": "aud", "aud": publicURL + "/acme", "name": "Ann Auditor"})
	// who reads
	for who, want := range map[string]int{"anonymous": 401, "install": 404, "channel adm": 404, "ext admin": 404, "tenant admin": 200,
		"server admin": 200} {
		if r := m.call(t, "GET", T+"/events", m.toks[who], ""); r.status != want {
			t.Errorf("%s: %d", who, r.status)
		}
	}
	r := m.call(t, "GET", T+"/events?kind=grant.add", aud, "")
	if r.status != 200 {
		t.Fatalf("an auditor: %d %s", r.status, r.body)
	}
	evs := r.json(t)["events"].([]any) // newest first: the fixture's grants (the CLI's) come after
	e := evs[0].(map[string]any)
	since := url.QueryEscape(e["at"].(string))
	if !strings.HasSuffix(e["actor"].(string), "|tom") || !strings.HasPrefix(e["subject"].(string), "grant:") || e["request"] == "spoofed" ||
		e["outcome"] != "ok" || e["client"] == nil ||
		e["data"].(map[string]any)["principal"] != "subject:corp|aud" {
		t.Errorf("the event: %v", e)
	}
	if g := m.call(t, "GET", T+"/events/"+e["id"].(string), aud, ""); g.status != 200 || g.json(t)["kind"] != "grant.add" {
		t.Errorf("one event: %d %s", g.status, g.body)
	}
	if g := m.call(t, "GET", T+"/events/01a00000-0000-7000-8000-000000000000", aud, ""); g.status != 404 {
		t.Errorf("a missing event: %d", g.status)
	}
	// another tenant's administrator never reads acme's events
	if g := m.call(t, "GET", "/api/v1/tenants/other/events?kind=grant.add", m.toks["other admin"], ""); g.status != 200 ||
		strings.Contains(string(g.body), "corp|aud") || !strings.Contains(string(g.body), "corp|olga") {
		t.Errorf("other's events: %d %s", g.status, g.body)
	}
	// the auditor's own name shows on its changes: it has none; paging
	m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|p1","verbs":["install"]}`)
	m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|p2","verbs":["install"]}`)
	first := m.call(t, "GET", T+"/events?kind=grant.add&limit=1&order=asc&since="+since, aud, "").json(t)
	next, _ := first["next"].(string)
	if len(first["events"].([]any)) != 1 || next == "" {
		t.Fatalf("a first page: %v", first)
	}
	second := m.call(t, "GET", T+"/events?kind=grant.add&limit=5&order=asc&since="+since+"&cursor="+url.QueryEscape(next), aud, "").json(t)
	if n := len(second["events"].([]any)); n != 2 {
		t.Fatalf("the next page: %v", second)
	}
	for q, want := range map[string]int{"subject=grant:&since=" + since: 3, "outcome=refused": 0, "kind=grant.add&since=" + since: 3,
		"until=2000-01-01T00:00:00Z": 0} {
		if g := m.call(t, "GET", T+"/events?"+q, aud, ""); g.status != 200 || len(g.json(t)["events"].([]any)) != want {
			t.Errorf("%s: %d %s", q, g.status, g.body)
		}
	}
	for _, q := range []string{"since=yesterday", "order=random", "cursor=bad"} {
		if g := m.call(t, "GET", T+"/events?"+q, aud, ""); g.status != 400 {
			t.Errorf("%s: %d", q, g.status)
		}
	}
	// a token's display name reaches the events of its changes
	tomNamed := m.keys.sign(t, idpURL, map[string]any{"sub": "tom", "aud": publicURL + "/acme", "preferred_username": "tom.k"})
	m.call(t, "POST", T+"/grants", tomNamed, `{"principal":"subject:corp|p3","verbs":["install"]}`)
	if g := m.call(t, "GET", T+"/events?kind=grant.add&limit=1", aud, "").json(t)["events"].([]any); g[0].(map[string]any)["actor_name"] != "tom.k" {
		t.Errorf("the actor's name: %v", g)
	}
	// whoami says who reads the log
	if w := m.call(t, "GET", T+"/whoami", aud, "").json(t); w["holds_audit"] != true || w["holds_admin"] != false {
		t.Errorf("an auditor's whoami: %v", w)
	}
	if w := m.call(t, "GET", T+"/whoami", m.toks["install"], "").json(t); w["holds_audit"] != false {
		t.Errorf("an installer's whoami: %v", w)
	}
	// server events: server administrators only; the catalogue: public
	if g := m.call(t, "GET", "/api/v1/events", m.toks["server admin"], ""); g.status != 200 {
		t.Errorf("server events: %d", g.status)
	}
	if g := m.call(t, "GET", "/api/v1/events", ta, ""); g.status != 404 {
		t.Errorf("server events for a tenant admin: %d", g.status)
	}
	srv := m.call(t, "GET", "/api/v1/events?kind=tenant.create", m.toks["server admin"], "").json(t)["events"].([]any)
	if len(srv) == 0 {
		t.Fatal("no tenant.create among the server's events")
	}
	id := srv[0].(map[string]any)["id"].(string)
	if g := m.call(t, "GET", "/api/v1/events/"+id, m.toks["server admin"], ""); g.status != 200 || g.json(t)["kind"] != "tenant.create" {
		t.Errorf("a server event: %d %s", g.status, g.body)
	}
	if g := m.call(t, "GET", T+"/events/"+id, aud, ""); g.status != 404 {
		t.Errorf("a server event through a tenant's route: %d", g.status)
	}
	// a server administrator's change on the tenant shows as "server" to the tenant's readers
	m.call(t, "POST", "/api/v1/tenants/acme/audiences", m.toks["server admin"], `{"audience":"api://acme-x"}`)
	ev := m.call(t, "GET", T+"/events?kind=audience.add", aud, "").json(t)["events"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["actor"] != "server" || ev[0].(map[string]any)["client"] != nil {
		t.Errorf("a server administrator's event for a tenant reader: %v", ev)
	}
	if ev := m.call(t, "GET", T+"/events?kind=audience.add", m.toks["server admin"], "").json(t)["events"].([]any); ev[0].(map[string]any)["actor"] == "server" {
		t.Errorf("a server administrator sees the actor: %v", ev)
	}
	if g := m.call(t, "GET", "/api/v1/event-kinds", "", ""); g.status != 200 || !strings.Contains(string(g.body), `"release.promote"`) {
		t.Errorf("the catalogue: %d", g.status)
	}
}

// Spec 0010 phase 1b: refusals are events, recorded from the request's final answer.
func TestRefusalEvents(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "GET", T+"/events", "", ""); r.status != 401 {
		t.Fatalf("anonymous: %d", r.status)
	}
	if r := m.call(t, "GET", T+"/events", "not-a-token", ""); r.status != 401 {
		t.Fatalf("a bad token: %d", r.status)
	}
	// noted before the tenant is looked up, still the tenant's; an unknown tenant's is nobody's
	if r := m.call(t, "GET", T+"/events", "", "", "Authorization", "Bearer "); r.status != 401 {
		t.Fatalf("a malformed credential: %d", r.status)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/nobody/events", "", "", "Authorization", "Bearer "); r.status != 401 {
		t.Fatalf("a malformed credential: %d", r.status)
	}
	if r := m.call(t, "GET", T+"/events", m.toks["install"], ""); r.status != 404 {
		t.Fatalf("install reads events: %d", r.status)
	}
	if r := m.call(t, "GET", "/api/v1/events", m.toks["tenant admin"], ""); r.status != 404 {
		t.Fatalf("a tenant token on a server route: %d", r.status)
	}
	m.events.Close(ctx) // the second failure, a second within the first, is a trailing count
	list := func(tenantID, kind string) []store.Event {
		evs, err := m.st.ListEvents(ctx, tenantID, store.EventFilter{Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	fails := list(acme.ID, "auth.failure")
	if len(fails) != 2 || !strings.Contains(fails[0].Data, `"reason":"malformed"`) {
		t.Fatalf("auth.failure, malformed (the trailing count of the invalid one's key): %+v", fails)
	}
	fails = fails[1:]
	if n := len(list(audit.ServerTenant, "auth.failure")); n != 0 {
		t.Fatalf("an unknown tenant's failure was recorded: %d", n)
	}
	if len(fails) != 1 || fails[0].Actor != "anonymous" || fails[0].Outcome != audit.Refused ||
		!strings.Contains(fails[0].Data, `"reason":"invalid"`) || !strings.Contains(fails[0].Data, `"route":"GET tenants/{t}/events"`) ||
		strings.Contains(fails[0].Data, "not-a-token") {
		t.Fatalf("auth.failure (a request without a token is none): %+v", fails)
	}
	refused := list(acme.ID, "authz.refused")
	if len(refused) != 1 || !strings.HasPrefix(refused[0].Actor, "principal:acme/") || !strings.Contains(refused[0].Data, `"status":404`) {
		t.Fatalf("authz.refused: %+v", refused)
	}
	if fails[0].Client == "" || fails[0].Request == fails[0].ID {
		t.Fatalf("the request's facts: %+v", fails[0])
	}
}

// Spec 0010 phase 2a: download statistics, read by scope.
func TestStatistics(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	acme, _ := m.st.GetTenant(ctx, "acme")
	chs, _ := m.st.ListChannels(ctx, "acme")
	ids := map[string]string{}
	for _, c := range chs {
		ids[c.Name] = c.ID
	}
	today := store.DayOf(m.st.Now())
	key := func(ch, name string, authd bool) store.DownloadKey {
		return store.DownloadKey{TenantID: acme.ID, ChannelID: ids[ch], Name: name, ExtVersion: "1.0", Platform: "linux_amd64",
			DuckDBVersion: "v2.0.0", Day: today, Authenticated: authd}
	}
	if _, err := m.st.AddDownloadCounts(ctx, map[store.DownloadKey]int64{key("prod", "tresor", true): 3, key("prod", "tresor", false): 2,
		key("prod", "acl", true): 5, key("staging", "acl", true): 7}); err != nil {
		t.Fatal(err)
	}
	if err := m.st.PutInstallers(ctx, today, map[store.InstallerKey]int64{{TenantID: acme.ID, ChannelID: ids["prod"], Name: "tresor",
		ExtVersion: "1.0", Platform: "linux_amd64"}: 2}); err != nil {
		t.Fatal(err)
	}
	total := func(who, q string) (int, int64) {
		r := m.call(t, "GET", T+"/stats/downloads?group=channel,day"+q, m.toks[who], "")
		if r.status != 200 {
			return r.status, 0
		}
		var n int64
		for _, row := range r.json(t)["rows"].([]any) {
			n += int64(row.(map[string]any)["count"].(float64))
		}
		return 200, n
	}
	for who, want := range map[string]int64{"tenant admin": 17, "server admin": 17, "channel adm": 10, "ext admin": 5} {
		if status, n := total(who, ""); status != 200 || n != want {
			t.Errorf("%s: %d %d, want %d", who, status, n, want)
		}
	}
	for who, want := range map[string]int{"anonymous": 401, "install": 404, "no grants": 404} {
		if status, _ := total(who, ""); status != want {
			t.Errorf("%s: %d, want %d", who, status, want)
		}
	}
	r := m.call(t, "GET", T+"/stats/downloads?group=version&extension=tresor", m.toks["tenant admin"], "").json(t)
	rows := r["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["installers"] != float64(2) || rows[0].(map[string]any)["authenticated"] != float64(3) {
		t.Fatalf("tresor by version: %v", r)
	}
	r = m.call(t, "GET", T+"/stats/downloads?group=duckdb_version", m.toks["tenant admin"], "").json(t)
	if row := r["rows"].([]any)[0].(map[string]any); row["installers"] != nil || row["duckdb_version"] != "v2.0.0" {
		t.Fatalf("installers are not by DuckDB version: %v", r)
	}
	for _, q := range []string{"?group=user", "?from=yesterday", "?from=2020-01-01&to=2026-01-01", "?from=2026-10-09&to=2026-10-01"} {
		if s := m.call(t, "GET", T+"/stats/downloads"+q, m.toks["tenant admin"], ""); s.status != 400 {
			t.Errorf("%s: %d", q, s.status)
		}
	}
	if s := m.call(t, "GET", T+"/stats/downloads?channel=nope", m.toks["tenant admin"], ""); s.status != 404 {
		t.Errorf("an unknown channel: %d", s.status)
	}
	// a version is an extension's: grouping by version never merges two extensions
	r = m.call(t, "GET", T+"/stats/downloads?group=version", m.toks["tenant admin"], "").json(t)
	if rows := r["rows"].([]any); len(rows) != 2 || rows[0].(map[string]any)["extension"] != "acl" {
		t.Fatalf("by version: %v", r)
	}
	// beyond a caller's scope a channel or an extension answers as missing, and is refused
	for _, q := range []string{"?channel=staging", "?extension=acl", "?channel=prod&extension=acl"} {
		if s := m.call(t, "GET", T+"/stats/downloads"+q, m.toks["ext admin"], ""); s.status != 404 {
			t.Errorf("ext admin %s: %d", q, s.status)
		}
	}
	if s := m.call(t, "GET", T+"/stats/downloads?channel=prod&extension=tresor", m.toks["ext admin"], ""); s.status != 200 {
		t.Errorf("ext admin on its extension: %d", s.status)
	}
	m.events.Close(ctx)
	if evs, _ := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "authz.refused", Limit: 100}); len(evs) < 3 {
		t.Errorf("refusals recorded: %d", len(evs))
	}
	rel := m.call(t, "GET", T+"/stats/releases", m.toks["ext admin"], "")
	rs := rel.json(t)["releases"].([]any)
	if rel.status != 200 || len(rs) != 1 || rs[0].(map[string]any)["last_7_days"] != float64(5) ||
		rs[0].(map[string]any)["last_download_day"] != today {
		t.Fatalf("releases for the extension's admin: %d %s", rel.status, rel.body)
	}
}
