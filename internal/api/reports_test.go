package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0045 over the API: the kind report, the attachments list narrowed to a waiting version, a
// feed's via, and the relay's two routes between servers.

func reportDoc(issue int, bodies ...string) string {
	var listed []any
	for _, b := range bodies {
		listed = append(listed, map[string]any{"hash": b, "read": true})
	}
	b, _ := json.Marshal(map[string]any{"envelope": "kista.report.v1", "report": "rep-1", "issue": issue, "issued_at": "2026-11-16T10:00:00Z",
		"depth": "static", "subject": map[string]any{"bodies": listed}, "attention": []any{map[string]any{"kind": "network", "count": 2}},
		"kinds_evaluated": 4, "html": "<h1>Report</h1><script>alert(1)</script>"})
	return string(b)
}

func TestReportRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom := m.toks["tenant admin"]
	body := tresorBody(t, func(p string) map[string]any { return m.call(t, "GET", p, "", "").json(t) })
	attach := func(q, mt, doc string) resp {
		return m.call(t, "POST", T+"/extensions/tresor/attachments?"+q, tom, doc, "Content-Type", mt, "Kista-Title", "Report")
	}
	// the kind: attached with its media type, summarised by its floor; refused when its body list
	// leaves the body out, or with another kind's media type
	r := attach("body_hash="+body+"&kind=report", attachment.Report, reportDoc(1, body))
	if r.status != 201 {
		t.Fatalf("attach a report: %d %s", r.status, r.body)
	}
	first := r.json(t)
	sum := first["summary"].(map[string]any)
	if first["kind"] != "report" || first["media_type"] != attachment.Report || sum["issue"] != 1.0 || sum["read"] != true || sum["bodies"] != 1.0 ||
		sum["depth"] != "static" || sum["html"] != nil {
		t.Fatalf("attached: %v", first)
	}
	for name, r := range map[string]resp{
		"a report about another body": attach("body_hash="+body+"&kind=report", attachment.Report, reportDoc(1, strings.Repeat("e", 64))),
		"a report as a review":        attach("body_hash="+body+"&kind=report", attachment.Review, reportDoc(1, body)),
		"html of another type":        attach("body_hash="+body+"&kind=report", attachment.Report, strings.Replace(reportDoc(1, body), `"html":"`, `"html":["`, 1)),
	} {
		if r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// a correction supersedes it
	r = attach("body_hash="+body+"&kind=report&supersedes="+first["id"].(string), attachment.Report, reportDoc(2, body))
	if r.status != 201 {
		t.Fatalf("a correction: %d %s", r.status, r.body)
	}
	second := r.json(t)["id"].(string)

	// the document is downloaded as its own type, sandboxed: kista never serves text/html
	got := m.call(t, "GET", T+"/extensions/tresor/attachments/"+second+"/content", tom, "")
	if got.status != 200 || got.header.Get("Content-Type") != attachment.Report || got.header.Get("Content-Security-Policy") != "sandbox" ||
		got.header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(got.header.Get("Content-Disposition"), "attachment;") ||
		string(got.body) != reportDoc(2, body) {
		t.Fatalf("content: %d %v", got.status, got.header)
	}

	// the list: by kind, and narrowed to a source's version and to chain heads
	source := ""
	for _, e := range m.call(t, "GET", T+"/catalogue?name=tresor", tom, "").json(t)["entries"].([]any) {
		source = e.(map[string]any)["source"].(string)
	}
	if source == "" {
		t.Fatal("no catalogue entry of tresor")
	}
	ids := func(q string) []string {
		t.Helper()
		r := m.call(t, "GET", T+"/extensions/tresor/attachments?"+q, tom, "")
		if r.status != 200 {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		var out []string
		for _, a := range r.json(t)["attachments"].([]any) {
			out = append(out, a.(map[string]any)["id"].(string))
		}
		return out
	}
	src := url.QueryEscape(source)
	for q, want := range map[string]int{
		"kind=report":                                      2,
		"kind=report&head=true":                            1,
		"kind=report&head=false":                           2,
		"source=" + src + "&version=1.0":                   2,
		"source=" + src + "&version=1.0&head=true":         1,
		"source=" + src + "&version=9.9&head=true":         0,
		"version=1.0&head=true":                            1,
		"source=" + src + "&version=1.0&kind=sbom":         0,
		"source=" + src + "&version=1.0&body_hash=" + body: 2,
	} {
		if got := ids(q); len(got) != want {
			t.Errorf("?%s: %d documents, want %d", q, len(got), want)
		}
	}
	if got := ids("source=" + src + "&version=1.0&head=true"); len(got) != 1 || got[0] != second {
		t.Fatalf("the head: %v, want %s", got, second)
	}
	for _, q := range []string{"head=maybe", "source=upstream:nope", "source=elsewhere", "kind=nope", "version=" + strings.Repeat("9", 201)} {
		if r := m.call(t, "GET", T+"/extensions/tresor/attachments?"+q, tom, ""); r.status != 400 {
			t.Errorf("?%s: %d", q, r.status)
		}
	}
	// a caller who cannot read documents gets none of it
	if r := m.call(t, "GET", T+"/extensions/tresor/attachments?head=true", m.toks["install"], ""); r.status != 404 {
		t.Errorf("a reader lists documents: %d", r.status)
	}

	// a feed's via: set at creation, shown, never changed
	keys := `["sha256:` + strings.Repeat("a", 64) + `"]`
	feed := `{"name":"own","url":"https://issuer.example/api/v1/tenants/x","via":"https://source.example/api/v1/tenants/org","credential":"src","keys":` + keys + `,"kinds":["report"]}`
	r = m.call(t, "POST", T+"/feeds", tom, feed)
	if r.status != 201 || r.json(t)["via"] != "https://source.example/api/v1/tenants/org" || r.json(t)["url"] != "https://issuer.example/api/v1/tenants/x" {
		t.Fatalf("a via feed: %d %s", r.status, r.body)
	}
	if got := m.call(t, "GET", T+"/feeds/own", tom, "").json(t); got["via"] != "https://source.example/api/v1/tenants/org" {
		t.Fatalf("read back: %v", got)
	}
	for _, ch := range []string{`{"via":"https://source.example/api/v1/tenants/other"}`, `{"via":""}`} {
		if r := m.call(t, "POST", T+"/feeds/own", tom, ch, "If-Match", "*"); r.status != 400 {
			t.Errorf("a change of via %s: %d %s", ch, r.status, r.body)
		}
	}
	for name, bad := range map[string]string{
		"via the issuer's URL":       strings.Replace(feed, "https://source.example/api/v1/tenants/org", "https://issuer.example/api/v1/tenants/x", 1),
		"via not a feed URL":         strings.Replace(feed, "/api/v1/tenants/org", "/", 1),
		"via on a bundle-mode feed":  strings.Replace(strings.Replace(feed, `"credential":"src",`, "", 1), `"name":"own"`, `"name":"air","mode":"bundle"`, 1),
		"a credential not for via's": strings.Replace(feed, "https://source.example/api/v1/tenants/org", "https://relay.example/api/v1/tenants/org", 1),
	} {
		if r := m.call(t, "POST", T+"/feeds", tom, strings.Replace(bad, `"name":"own"`, `"name":"bad"`, 1)); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// a feed without via has no such member
	plain := `{"name":"plain","url":"https://source.example/api/v1/tenants/x","credential":"src","keys":` + keys + `}`
	if r := m.call(t, "POST", T+"/feeds", tom, plain); r.status != 201 || strings.Contains(string(r.body), `"via"`) {
		t.Fatalf("a feed without via: %d %s", r.status, r.body)
	}
}

// Three servers (spec 0045): an issuer's tenant offers a report; the relaying tenant A takes its
// feed with export; an own kista B takes A's feed for the issuer with a credential that holds feed
// on A's channel. B never contacts the issuer. With A suspended B is unauthorized and keeps what it
// holds.
func TestRelayBetweenServers(t *testing.T) {
	serveOf := func(m *mgmt, n *atomic.Int64) *httptest.Server {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			m.h.ServeHTTP(w, serve.WithHTTPS(r))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	feedGrant := func(m *mgmt, sub string) string {
		t.Helper()
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+sub, []string{"feed"}, tenants.GrantOn{Channel: "prod"}); err != nil {
			t.Fatal(err)
		}
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	const api = "/api/v1/tenants/acme"

	// the issuer: a report about tresor 1.0, offered to administrators
	issuer := newMgmt(t)
	var toIssuer, toRelay atomic.Int64
	srvI := serveOf(issuer, &toIssuer)
	issuer.att.PublicURL = srvI.URL
	if err := signer.GenerateKeyFile(filepath.Join(issuer.dir, "att.pem")); err != nil {
		t.Fatal(err)
	}
	key, err := issuer.keySvc.AddAttestation(ctx, admin, "acme", "file:att.pem", true)
	if err != nil {
		t.Fatal(err)
	}
	body := tresorBody(t, func(p string) map[string]any { return issuer.call(t, "GET", p, "", "").json(t) })
	offer := func(issue int, supersedes string) store.Attachment {
		t.Helper()
		d, err := issuer.att.Attach(ctx, admin, "acme", pkgtype.Of("tresor"), attachment.AttachInput{BodyHash: body, Kind: store.KindReport,
			MediaType: attachment.Report, Title: "Report", Content: []byte(reportDoc(issue, body)), Supersedes: supersedes})
		if err != nil {
			t.Fatal(err)
		}
		yes := true
		if _, err := issuer.att.SetFlags(ctx, admin, "acme", pkgtype.Of("tresor"), d.ID, nil, &yes, 0); err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := offer(1, "")
	issuerURL := srvI.URL + api

	// A: the relaying tenant, with an export feed of the issuer and a feed grant for the own kista
	relay := newMgmt(t)
	srvA := serveOf(relay, &toRelay)
	a := subscriberOn(t, relay.env, srvI, feedGrant(issuer, "relay"))
	if _, err := a.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "issuer", URL: issuerURL, Credential: "src", Keys: []string{key.Fingerprint},
		Offer: store.OfferChannels, Channels: []string{"prod"}, Export: true}); err != nil {
		t.Fatal(err)
	}
	if res := a.run(t, "issuer"); res.Error != "" || res.Counts["received"] != 1 {
		t.Fatalf("A's run: %+v", res)
	}
	own := feedGrant(relay, "own-kista")

	// the relay's routes, as the own kista's credential and as others
	ask := `{"issuer":"` + issuerURL + `","bodies":[{"body_hash":"` + body + `","after_seq":0}]}`
	r := relay.call(t, "POST", api+"/feed", own, ask)
	if r.status != 200 {
		t.Fatalf("the relay's answer: %d %s", r.status, r.body)
	}
	var ans attachment.FeedAnswer
	if err := json.Unmarshal(r.body, &ans); err != nil || len(ans.Statements) != 1 || len(ans.Keys) == 0 || ans.Statements[0].Note != "" || ans.More {
		t.Fatalf("the relay's answer: %s %v", r.body, err)
	}
	if ks, err := attachment.ParseKeysFile(ans.Keys); err != nil || ks[key.Fingerprint] == nil {
		t.Fatalf("the keys in the answer: %v %v", ks, err)
	}
	for name, c := range map[string]struct {
		token, ask string
		status     int
	}{
		"no grant":                    {relay.toks["install"], ask, 404},
		"no token":                    {"", ask, 401},
		"an issuer not handed on":     {own, strings.Replace(ask, issuerURL, "https://elsewhere.example/api/v1/tenants/x", 1), 200},
		"an issuer not in its form":   {own, strings.Replace(ask, issuerURL, issuerURL+"/", 1), 400},
		"an issuer of another scheme": {own, strings.Replace(ask, "https://", "http://", 1), 400},
	} {
		r := relay.call(t, "POST", api+"/feed", c.token, c.ask)
		if r.status != c.status || r.status == 200 && string(r.body) != `{"statements":[],"bodies":[],"more":false}`+"\n" && string(r.body) != `{"statements":[],"bodies":[],"more":false}` {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	q := "?issuer=" + url.QueryEscape(issuerURL)
	doc := relay.call(t, "GET", api+"/feed/documents/"+first.ID+q, own, "")
	if doc.status != 200 || string(doc.body) != reportDoc(1, body) || doc.header.Get("Content-Type") != attachment.Report ||
		doc.header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("the relayed document: %d %v", doc.status, doc.header)
	}
	for name, c := range map[string]struct {
		path, token string
		status      int
	}{
		"without the issuer (the tenant's own route)": {api + "/feed/documents/" + first.ID, own, 404},
		"another issuer":            {api + "/feed/documents/" + first.ID + "?issuer=" + url.QueryEscape("https://elsewhere.example/api/v1/tenants/x"), own, 404},
		"no such document":          {api + "/feed/documents/" + store.NewID() + q, own, 404},
		"no grant":                  {api + "/feed/documents/" + first.ID + q, relay.toks["install"], 404},
		"an issuer not in its form": {api + "/feed/documents/" + first.ID + q + "%2F", own, 400},
	} {
		if r := relay.call(t, "GET", c.path, c.token, ""); r.status != c.status {
			t.Errorf("a document, %s: %d %s", name, r.status, r.body)
		}
	}

	// B: the own kista, through A
	b := newSubscriber(t, srvA, own)
	if _, err := b.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "reports", URL: issuerURL, Via: srvA.URL + api, Credential: "src",
		Keys: []string{key.Fingerprint}, Offer: store.OfferChannels, Channels: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	seen := toIssuer.Load()
	if res := b.run(t, "reports"); res.Error != "" || res.Counts["received"] != 1 {
		t.Fatalf("B's run: %+v", res)
	}
	if toIssuer.Load() != seen || toRelay.Load() == 0 {
		t.Fatalf("B's run reached the issuer: %d requests (%d to the relay)", toIssuer.Load()-seen, toRelay.Load())
	}
	docs := b.docs(t, body)
	if len(docs) != 1 || docs[0].Kind != store.KindReport || docs[0].Issuer != issuerURL || docs[0].IssuerID != first.ID || docs[0].KeyFP != key.Fingerprint ||
		docs[0].Visibility != store.VisibleAdministrators || !strings.Contains(docs[0].Summary, `"issue":1`) {
		t.Fatalf("the report at B: %+v", docs)
	}
	// a correction follows the same way
	offer(2, first.ID)
	if res := a.run(t, "issuer"); res.Counts["received"] != 1 {
		t.Fatalf("A's run for the correction: %+v", res)
	}
	seen = toIssuer.Load()
	if res := b.run(t, "reports"); res.Error != "" || res.Counts["received"] != 1 || res.Counts["superseded"] != 1 {
		t.Fatalf("B's run for the correction: %+v", res)
	}
	if heads := attachment.Heads(b.docs(t, body)); len(heads) != 1 || !strings.Contains(heads[0].Summary, `"issue":2`) {
		t.Fatalf("the heads at B: %+v", heads)
	}
	if toIssuer.Load() != seen {
		t.Fatalf("B's run reached the issuer: %d requests", toIssuer.Load()-seen)
	}

	// stored bytes altered at A: the relayed answer ends below them, and A's feed says so
	tn, _ := relay.st.GetTenant(ctx, "acme")
	held, err := relay.st.BodyAttachments(ctx, tn.ID, body)
	if err != nil || len(held) != 2 {
		t.Fatalf("A's documents: %d %v", len(held), err)
	}
	if err := store.ExecRaw(ctx, relay.st, "UPDATE attachments SET statement = ? WHERE id = ?",
		strings.Replace(held[0].Statement, `"title":"Report"`, `"title":"Other"`, 1), held[0].ID); err != nil {
		t.Fatal(err)
	}
	if r := relay.call(t, "POST", api+"/feed", own, ask); r.status != 200 || len(r.json(t)["statements"].([]any)) != 0 {
		t.Fatalf("the relay's answer over altered bytes: %d %s", r.status, r.body)
	}
	if f := relay.call(t, "GET", api+"/feeds/issuer", relay.toks["tenant admin"], "").json(t); f["relay_stopped_at"] == nil || f["relay_stopped"] != nil {
		t.Fatalf("A's feed after an answer that ended early: %v", f)
	}
	if err := store.ExecRaw(ctx, relay.st, "UPDATE attachments SET statement = ? WHERE id = ?", held[0].Statement, held[0].ID); err != nil {
		t.Fatal(err)
	}

	// A suspended: its feed routes answer as a missing tenant; B is unauthorized and keeps its documents
	root := relay.toks["server admin"]
	etag := relay.call(t, "GET", api, root, "").header.Get("ETag")
	if r := relay.call(t, "POST", api+"/suspend", root, "", "If-Match", etag); r.status != 200 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	if r := relay.call(t, "POST", api+"/feed", own, ask); r.status != 404 {
		t.Fatalf("a suspended tenant's relay: %d", r.status)
	}
	if r := relay.call(t, "GET", api+"/feed/documents/"+first.ID+q, own, ""); r.status != 404 {
		t.Fatalf("a suspended tenant's relayed document: %d", r.status)
	}
	res := b.run(t, "reports")
	if f := mustFeed(t, b, "reports"); f.Status != store.FeedUnauthorized || res.Error == "" || len(b.docs(t, body)) != 2 {
		t.Fatalf("B while A is suspended: %s %+v", f.Status, res)
	}
}

// attachments.feed_answer_max bounds the tenant's own feed answers too: under the smallest bound a
// subscriber pages through its statements, and none is lost.
func TestOwnFeedAnswerMax(t *testing.T) {
	m := newMgmt(t)
	m.att.FeedAnswerMax = attachment.MinFeedAnswer
	if err := signer.GenerateKeyFile(filepath.Join(m.dir, "att.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.keySvc.AddAttestation(ctx, admin, "acme", "file:att.pem", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|feeder", []string{"feed"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	tok := m.keys.sign(t, idpURL, map[string]any{"sub": "feeder", "aud": publicURL + "/acme"})
	body := tresorBody(t, func(p string) map[string]any { return m.call(t, "GET", p, "", "").json(t) })
	const n = 160
	prev, yes := "", true
	for i := range n { // one chain: the heads limit does not count it
		d, err := m.att.Attach(ctx, admin, "acme", pkgtype.Of("tresor"), attachment.AttachInput{BodyHash: body, Kind: store.KindNotes,
			MediaType: attachment.Markdown, Title: strings.Repeat("t", 180), Content: []byte("# note " + strings.Repeat("n", i)), Supersedes: prev})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.att.SetFlags(ctx, admin, "acme", pkgtype.Of("tresor"), d.ID, nil, &yes, 0); err != nil {
			t.Fatal(err)
		}
		prev = d.ID
	}
	after, got, pages := int64(0), 0, 0
	for {
		r := m.call(t, "POST", "/api/v1/tenants/acme/feed", tok, `{"bodies":[{"body_hash":"`+body+`","after_seq":`+itoa(after)+`}]}`)
		var ans attachment.FeedAnswer
		if err := json.Unmarshal(r.body, &ans); err != nil || r.status != 200 || len(r.body) > attachment.MinFeedAnswer || len(ans.Keys) != 0 {
			t.Fatalf("page %d: %d, %d bytes, %v", pages, r.status, len(r.body), err)
		}
		got, pages = got+len(ans.Statements), pages+1
		if len(ans.Bodies) != 1 || ans.Bodies[0].Seq <= after {
			t.Fatalf("page %d moves nothing: %+v", pages, ans.Bodies)
		}
		after = ans.Bodies[0].Seq
		if !ans.More {
			break
		}
	}
	if got != n || pages < 2 {
		t.Fatalf("%d statements in %d pages, want %d in several", got, pages, n)
	}
}
