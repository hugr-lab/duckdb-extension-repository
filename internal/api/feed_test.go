package api_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

const review = `{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest",
"capabilities":[],"network":[],"files":[],"processes":[],"licences":["MIT"],"findings":[],"text":"# Fine"}`

// subscriber is a second kista (its own store) with a feed client reaching a TLS server.
type subscriber struct {
	*env
	feeds *attachment.Feeds
	token string // the credential's token file
}

func newSubscriber(t *testing.T, srv *httptest.Server, token string) *subscriber {
	t.Helper()
	en := newEnv(t)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	eg, err := egress.New(egress.Config{CAFile: ca, Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(port)}}}})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "src", Tenants: []string{"acme"}, Prefixes: []string{srv.URL + "/"},
		Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	att := &attachment.Service{Store: en.st, Blob: en.blob, Authz: authz.ServerAdmin{}}
	return &subscriber{env: en, token: tokenFile, feeds: &attachment.Feeds{Store: en.st, Authz: authz.ServerAdmin{}, Attachments: att,
		Fetch: eg, Credentials: creds, Log: slog.New(slog.DiscardHandler)}}
}

// run asks for a feed's run and runs it.
func (s *subscriber) run(t *testing.T, name string) attachment.RunResult {
	t.Helper()
	f, err := s.feeds.Sync(ctx, admin, "acme", name)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.feeds.RunFeed(ctx, f.ID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (s *subscriber) docs(t *testing.T, body string) []store.Attachment {
	t.Helper()
	tn, _ := s.st.GetTenant(ctx, "acme")
	all, err := s.st.BodyAttachments(ctx, tn.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func tresorBody(t *testing.T, call func(path string) map[string]any) string {
	t.Helper()
	for _, x := range call("/api/v1/tenants/acme/channels/prod/extensions/tresor")["releases"].([]any) {
		if r := x.(map[string]any); r["version"] == "1.0" {
			return r["body_hash"].(string)
		}
	}
	t.Fatal("no tresor 1.0")
	return ""
}

// The feed between two kista servers (spec 0011 phase 1b): a source's feedable documents reach a
// subscriber that pinned its key, for the bodies it sent and holds; withdrawals and re-issues follow;
// the source answers only in the channels a feed grant covers, only documents offered on purpose.
func TestFeedBetweenServers(t *testing.T) {
	m := newMgmt(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.h.ServeHTTP(w, serve.WithHTTPS(r)) }))
	t.Cleanup(srv.Close)
	feedURL := srv.URL + "/api/v1/tenants/acme"
	m.att.PublicURL = srv.URL
	if err := signer.GenerateKeyFile(filepath.Join(m.dir, "att.pem")); err != nil {
		t.Fatal(err)
	}
	k, err := m.keySvc.AddAttestation(ctx, admin, "acme", "file:att.pem", true)
	if err != nil {
		t.Fatal(err)
	}
	for sub, ch := range map[string]string{"feeder": "prod", "stager": "staging"} {
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+sub, []string{"feed"}, tenants.GrantOn{Channel: ch}); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(sub string) string {
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	body := tresorBody(t, func(p string) map[string]any { return m.call(t, "GET", p, "", "").json(t) })

	// the source: a review offered to readers, an SBOM kept to itself
	rv, err := m.att.Attach(ctx, admin, "acme", "tresor", attachment.AttachInput{BodyHash: body, Kind: store.KindReview,
		MediaType: attachment.Review, Title: "Enterest review", Content: []byte(review)})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := m.att.Attach(ctx, admin, "acme", "tresor", attachment.AttachInput{BodyHash: body, Kind: store.KindSBOM,
		MediaType: attachment.CycloneDX, Title: "SBOM", Content: []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	readers, yes, no := store.VisibleReaders, true, false
	if _, err := m.att.SetFlags(ctx, admin, "acme", "tresor", rv.ID, &readers, &yes, 0); err != nil {
		t.Fatal(err)
	}

	// the source's routes: the attestation keys are public; the feed answers by channel scope
	var pub struct {
		Keys []struct{ Fingerprint, State string } `json:"keys"`
	}
	if err := json.Unmarshal(m.call(t, "GET", "/api/v1/tenants/acme/attestation-keys", "", "").body, &pub); err != nil ||
		len(pub.Keys) != 1 || pub.Keys[0].Fingerprint != k.Fingerprint {
		t.Fatalf("attestation keys: %+v %v", pub, err)
	}
	ask := `{"bodies":[{"body_hash":"` + body + `","after_seq":0}]}`
	statements := func(token string) (int, int) {
		r := m.call(t, "POST", "/api/v1/tenants/acme/feed", token, ask)
		if r.status != 200 {
			return r.status, 0
		}
		return r.status, len(r.json(t)["statements"].([]any))
	}
	for who, want := range map[string][2]int{"feeder": {200, 1}, "stager": {200, 0}, "ivan": {404, 0}, "": {401, 0}} {
		token := ""
		if who != "" {
			token = tok(who)
		}
		if s, n := statements(token); s != want[0] || n != want[1] {
			t.Errorf("%s: %d, %d statements", who, s, n)
		}
	}
	for id, want := range map[string]int{rv.ID: 200, sb.ID: 404, store.NewID(): 404} {
		if r := m.call(t, "GET", "/api/v1/tenants/acme/feed/documents/"+id, tok("feeder"), ""); r.status != want {
			t.Errorf("document %s: %d", id, r.status)
		}
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/feed/documents/"+rv.ID, tok("stager"), ""); r.status != 404 {
		t.Errorf("a document outside the grant's channels: %d", r.status)
	}

	// the subscriber: its feed brings the review, readers-visible, signed by the pinned key
	sub := newSubscriber(t, srv, tok("feeder"))
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "enterest", URL: feedURL, Credential: "src",
		Keys: []string{k.Fingerprint}, Readers: true, Offer: store.OfferChannels, Channels: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	if res := sub.run(t, "enterest"); res.Error != "" || res.Counts["received"] != 1 {
		t.Fatalf("first run: %+v", res)
	}
	docs := sub.docs(t, body)
	if len(docs) != 1 || docs[0].Origin != store.AttachedByFeed || docs[0].Visibility != store.VisibleReaders || docs[0].Issuer != feedURL ||
		docs[0].IssuerID != rv.ID || docs[0].KeyFP != k.Fingerprint || docs[0].Title != "Enterest review" || !strings.Contains(docs[0].Summary, `"approved"`) {
		t.Fatalf("received: %+v", docs)
	}
	// a second run brings nothing new
	if res := sub.run(t, "enterest"); res.Error != "" || res.Counts["received"] != 0 {
		t.Fatalf("second run: %+v", res)
	}
	// withdrawn at the source, then offered again, then to administrators only
	if _, err := m.att.SetFlags(ctx, admin, "acme", "tresor", rv.ID, nil, &no, 0); err != nil {
		t.Fatal(err)
	}
	if res := sub.run(t, "enterest"); res.Counts["withdrawn"] != 1 {
		t.Fatalf("withdrawal: %+v", res)
	}
	if d := sub.docs(t, body); d[0].WithdrawnAt.IsZero() || len(attachment.Readable(d, map[string]bool{body: true})) != 0 {
		t.Fatalf("not withdrawn: %+v", d)
	}
	if _, err := m.att.SetFlags(ctx, admin, "acme", "tresor", rv.ID, nil, &yes, 0); err != nil {
		t.Fatal(err)
	}
	if res := sub.run(t, "enterest"); res.Counts["updated"] != 1 {
		t.Fatalf("offered again: %+v", res)
	}
	if d := sub.docs(t, body); !d[0].WithdrawnAt.IsZero() || len(attachment.Readable(d, map[string]bool{body: true})) != 1 {
		t.Fatalf("not offered again: %+v", d)
	}
	admins := store.VisibleAdministrators
	if _, err := m.att.SetFlags(ctx, admin, "acme", "tresor", rv.ID, &admins, nil, 0); err != nil {
		t.Fatal(err)
	}
	if res := sub.run(t, "enterest"); res.Counts["updated"] != 1 {
		t.Fatalf("its audience narrowed: %+v", res)
	}
	d := sub.docs(t, body)
	if d[0].Visibility != store.VisibleAdministrators {
		t.Fatalf("still readers-visible: %+v", d)
	}
	// the subscriber's administrators cannot show readers what the source offers to administrators only
	if _, err := sub.feeds.Attachments.SetFlags(ctx, admin, "acme", "tresor", d[0].ID, &readers, nil, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("shown to readers against its audience: %v", err)
	}
	// a second feed of the same source stores nothing twice; one pinning another key verifies nothing
	other := extfile.Fingerprint(&mustKey(t).PublicKey)
	for name, fp := range map[string]string{"again": k.Fingerprint, "wrong-key": other} {
		if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: name, URL: feedURL, Credential: "src", Keys: []string{fp},
			Offer: store.OfferChannels, Channels: []string{"prod"}}); err != nil {
			t.Fatal(err)
		}
	}
	if res := sub.run(t, "again"); res.Error != "" || res.Counts["received"] != 0 || len(sub.docs(t, body)) != 1 {
		t.Fatalf("a second feed: %+v", res)
	}
	if res := sub.run(t, "wrong-key"); res.Error == "" {
		t.Fatalf("an unpinned key: %+v", res)
	}
	if f, _ := sub.feeds.Get(ctx, admin, "acme", "wrong-key"); f.Status != store.FeedFailing {
		t.Fatalf("status: %s", f.Status)
	}
	// the credential refused: unauthorized
	if err := os.WriteFile(sub.token, []byte("not-a-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub.run(t, "enterest")
	if f, _ := sub.feeds.Get(ctx, admin, "acme", "enterest"); f.Status != store.FeedUnauthorized {
		t.Fatalf("status: %s", f.Status)
	}
	// egress: a feed at a port egress does not reach is refused
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "elsewhere", URL: "https://127.0.0.1:1/api/v1/tenants/acme",
		Credential: "src", Keys: []string{other}}); err == nil {
		t.Fatal("an unreachable port accepted")
	}
	// removing a feed with its documents: another feed of the source keeps them, then they go
	if err := sub.feeds.Remove(ctx, admin, "acme", "enterest", true, 0); err != nil || len(sub.docs(t, body)) != 1 ||
		sub.docs(t, body)[0].AttachedBy != mustFeed(t, sub, "again").ID {
		t.Fatalf("remove: %v %+v", err, sub.docs(t, body))
	}
	if err := sub.feeds.Remove(ctx, admin, "acme", "wrong-key", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := sub.feeds.Remove(ctx, admin, "acme", "again", true, 0); err != nil || len(sub.docs(t, body)) != 0 {
		t.Fatalf("remove: %v %d", err, len(sub.docs(t, body)))
	}
}

// keySigner signs with a private key (a fake source's attestation key).
type keySigner struct{ k *rsa.PrivateKey }

func (s keySigner) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.k, crypto.SHA256, h[:])
}
func (s keySigner) Public() *rsa.PublicKey { return &s.k.PublicKey }
func (s keySigner) ID() string             { return "test" }

// A source the subscriber cannot trust beyond its key (spec 0011): unsigned, wrongly signed,
// altered, re-encoded, unrequested and misattributed statements are refused, and a document whose
// content does not match its statement too; an unresolved supersedes is kept.
func TestFeedRefusals(t *testing.T) {
	key, stranger := mustKey(t), mustKey(t)
	fp := extfile.Fingerprint(&key.PublicKey)
	var feedURL string
	content := []byte(review)
	var wire []attachment.WireStatement
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/attestation-keys"):
			der := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustDER(t, &key.PublicKey)})
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"fingerprint": fp, "public_key": string(der)}}})
		case strings.HasSuffix(r.URL.Path, "/feed"):
			_ = json.NewEncoder(w).Encode(attachment.FeedAnswer{Statements: wire, Bodies: []attachment.HighWater{}})
		case strings.Contains(r.URL.Path, "/feed/documents/"):
			if strings.HasSuffix(r.URL.Path, "/bad-content") {
				_, _ = w.Write([]byte(strings.Replace(review, "approved", "rejected", 1)))
				return
			}
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	feedURL = srv.URL + "/api/v1/tenants/src"
	sub := newSubscriber(t, srv, "T")
	body := tresorBody(t, func(p string) map[string]any { return getJSON(t, sub.env, p) })
	unsent := strings.Repeat("e", 64)
	seq := int64(0)
	stmt := func(mod func(*attachment.Statement)) attachment.Statement {
		seq++
		s := attachment.Statement{Issuer: feedURL, Key: fp, IssuerID: "doc-" + strconv.FormatInt(seq, 10), Seq: seq, BodyHash: body,
			Kind: store.KindReview, MediaType: attachment.Review, ContentHash: blob.ContentHash(content), Size: int64(len(content)),
			Title: "Review", Summary: json.RawMessage(`{"verdict":"approved"}`), IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant",
			Audience: "readers"}
		if mod != nil {
			mod(&s)
		}
		return s
	}
	sign := func(s attachment.Statement, k *rsa.PrivateKey) (string, []byte, []byte) {
		b, err := s.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := attachment.Sign(ctx, keySigner{k}, b)
		if err != nil {
			t.Fatal(err)
		}
		return s.IssuerID, b, sig
	}
	add := func(b, sig []byte) {
		wire = append(wire, attachment.WireStatement{Statement: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(sig)})
	}
	_, b, sig := sign(stmt(nil), key)
	add(b, sig) // stored
	_, b, sig = sign(stmt(func(s *attachment.Statement) { s.Supersedes = "never-seen" }), key)
	add(b, sig) // stored, its link unresolved
	_, b, sig = sign(stmt(func(s *attachment.Statement) { s.IssuerID = "bad-content" }), key)
	add(b, sig) // content that does not match
	_, b, sig = sign(stmt(nil), key)
	add([]byte(strings.Replace(string(b), `"Review"`, `"Approved by us"`, 1)), sig) // altered after signing
	_, b, _ = sign(stmt(nil), key)
	add(b, make([]byte, 256)) // unsigned
	_, b, sig = sign(stmt(nil), stranger)
	add(b, sig) // another key
	_, b, sig = sign(stmt(func(s *attachment.Statement) { s.BodyHash = unsent }), key)
	add(b, sig) // a body not sent
	_, b, sig = sign(stmt(func(s *attachment.Statement) { s.Issuer = srv.URL + "/api/v1/tenants/other" }), key)
	add(b, sig) // another issuer
	s := stmt(nil)
	pretty, _ := json.MarshalIndent(s, "", " ")
	sig, _ = attachment.Sign(ctx, keySigner{key}, pretty)
	add(pretty, sig) // signed, but not canonical
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "src", URL: feedURL, Credential: "src", Keys: []string{fp},
		Offer: store.OfferChannels, Channels: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	res := sub.run(t, "src")
	reasons := map[string]int{}
	for _, r := range res.Refused {
		reasons[r.Reason]++
	}
	if res.Error != "" || res.Counts["received"] != 2 || reasons["signature"] != 4 || reasons["unrequested"] != 1 || reasons["issuer"] != 1 ||
		reasons["content"] != 1 {
		t.Fatalf("run: %+v %v", res, reasons)
	}
	docs := sub.docs(t, body)
	if len(docs) != 2 || docs[1].Supersedes != "never-seen" || len(attachment.Heads(docs)) != 2 {
		t.Fatalf("stored: %+v", docs)
	}
	// the first statement that does not verify holds the body's cursor: what follows it, verified
	// or not, comes again at the next run (once its key is pinned)
	cur, _ := sub.st.FeedCursors(ctx, mustFeed(t, sub, "src").ID)
	if cur[body] != 3 {
		t.Fatalf("cursor: %v", cur)
	}
	if res := sub.run(t, "src"); res.Counts["received"] != 0 || res.Counts["refused"] != 6 {
		t.Fatalf("again: %+v", res)
	}
}

func mustDER(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustFeed(t *testing.T, s *subscriber, name string) store.Feed {
	t.Helper()
	f, err := s.feeds.Get(ctx, admin, "acme", name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func getJSON(t *testing.T, en *env, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	en.serve.ServeHTTP(rec, serve.WithHTTPS(req))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %d %v", path, rec.Code, err)
	}
	return out
}

// The feed's management routes over HTTP (spec 0011 phase 1b): who may call them, If-Match, 404
// uniformity; attestation keys added by a server administrator, read and moved by the tenant's.
func TestFeedRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom, carl, root := m.toks["tenant admin"], m.toks["channel adm"], m.toks["server admin"]
	// attestation keys
	if err := signer.GenerateKeyFile(filepath.Join(m.dir, "att.pem")); err != nil {
		t.Fatal(err)
	}
	add := `{"signer_ref":"file:att.pem","active":true}`
	if r := m.call(t, "POST", T+"/attestation-keys", tom, add); r.status != 404 {
		t.Fatalf("a tenant administrator registers a signer: %d", r.status)
	}
	r := m.call(t, "POST", T+"/attestation-keys", root, add)
	if r.status != 201 {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	loc := strings.TrimPrefix(r.header.Get("Location"), "")
	got := m.call(t, "GET", loc, tom, "")
	if got.status != 200 || got.header.Get("ETag") == "" {
		t.Fatalf("read: %d %v", got.status, got.header)
	}
	if r := m.call(t, "GET", loc, carl, ""); r.status != 404 {
		t.Fatalf("a channel administrator reads it: %d", r.status)
	}
	if r := m.call(t, "POST", loc+"/retire", tom, "", "If-Match", got.header.Get("ETag")); r.status != 409 {
		t.Fatalf("the active key retired: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", loc+"/retire?force=true", tom, "", "If-Match", "*"); r.status != 404 {
		t.Fatalf("force by a tenant administrator: %d", r.status)
	}
	// a document offered over the API
	body := tresorBody(t, func(p string) map[string]any { return m.call(t, "GET", p, "", "").json(t) })
	d, err := m.att.Attach(ctx, admin, "acme", "tresor", attachment.AttachInput{BodyHash: body, Kind: store.KindReview,
		MediaType: attachment.Review, Title: "R", Content: []byte(review)})
	if err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "POST", T+"/extensions/tresor/attachments/"+d.ID+"/feedable", tom, "", "If-Match", versionTag(d.Version)); r.status != 200 ||
		r.json(t)["feedable"] != true {
		t.Fatalf("feedable: %d %s", r.status, r.body)
	}
	// feeds
	feed := `{"name":"src","url":"https://source.example/api/v1/tenants/x","credential":"src","keys":["sha256:` + strings.Repeat("a", 64) + `"]}`
	for who, want := range map[string]int{"": 401, "channel adm": 404, "install": 404} {
		if r := m.call(t, "POST", T+"/feeds", m.toks[who], feed); r.status != want {
			t.Errorf("%q adds a feed: %d", who, r.status)
		}
	}
	r = m.call(t, "POST", T+"/feeds", tom, feed)
	if r.status != 201 || r.header.Get("Location") != T+"/feeds/src" {
		t.Fatalf("add a feed: %d %s", r.status, r.body)
	}
	etag := r.header.Get("ETag")
	if r := m.call(t, "POST", T+"/feeds", tom, strings.Replace(feed, `"src","url"`, `"other","url"`, 1)+"x"); r.status != 400 {
		t.Fatalf("a bad body: %d", r.status)
	}
	if l := m.call(t, "GET", T+"/feeds", tom, "").json(t)["feeds"].([]any); len(l) != 1 || l[0].(map[string]any)["status"] != "new" {
		t.Fatalf("list: %v", l)
	}
	if r := m.call(t, "POST", T+"/feeds/src", tom, `{"readers":true}`); r.status != 428 {
		t.Fatalf("a change without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/feeds/src", tom, `{"readers":true}`, "If-Match", `"v9"`); r.status != 412 {
		t.Fatalf("a stale change: %d", r.status)
	}
	r = m.call(t, "POST", T+"/feeds/src", tom, `{"readers":true}`, "If-Match", etag)
	if r.status != 200 || r.json(t)["readers"] != true {
		t.Fatalf("change: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/feeds/src/sync", tom, ""); r.status != 202 {
		t.Fatalf("sync: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/feeds/src?documents=maybe", tom, "", "If-Match", "*"); r.status != 400 {
		t.Fatalf("documents=maybe: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/feeds/src?documents=true", tom, "", "If-Match", "*"); r.status != 204 {
		t.Fatalf("remove: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/feeds/src", tom, ""); r.status != 404 {
		t.Fatalf("removed: %d", r.status)
	}
	// a bundle-mode feed (spec 0014): never synced, its mode never changed
	air := `{"name":"air","url":"https://source.example/api/v1/tenants/x","mode":"bundle","keys":["sha256:` + strings.Repeat("a", 64) + `"]}`
	r = m.call(t, "POST", T+"/feeds", tom, air)
	if r.status != 201 || r.json(t)["mode"] != "bundle" || r.json(t)["export"] != false || r.json(t)["run_requested"] != false {
		t.Fatalf("a bundle-mode feed: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/feeds/air/sync", tom, ""); r.status != 409 {
		t.Fatalf("sync of a bundle-mode feed: %d", r.status)
	}
	for _, ch := range []string{`{"mode":"online"}`, `{"export":true}`, `{"credential":"src"}`} {
		if r := m.call(t, "POST", T+"/feeds/air", tom, ch, "If-Match", "*"); r.status != 400 {
			t.Errorf("%s: %d %s", ch, r.status, r.body)
		}
	}
}

func versionTag(v int64) string { return `"v` + strconv.FormatInt(v, 10) + `"` }
