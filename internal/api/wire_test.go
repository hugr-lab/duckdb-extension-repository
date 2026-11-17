package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// The wire fixtures (spec 0046): testdata/wire/1 holds what kista 0.2 writes on each wire format
// another kista reads, as its real producers wrote it once: the serve handler's .well-known,
// -/names and -/revocations, the API's attestation keys, feed answer and feed document, the feed
// client's request, a plan as kista admin bundle plan writes it, and the bundle an export made for
// that plan (files, a licence, a revocation, statements). They are never edited or regenerated.
//
// Every run of this test
//   - makes the same things again with this binary's producers and fails if one no longer writes a
//     member a fixture has (a producer may add members, never drop one);
//   - reads the fixtures with this binary's readers: a mirror's run over the dash routes and the
//     .well-known file, a feed's run over the feed's answers, and an import of the bundle with its
//     licence, its revocation and its statements.
//
// KISTA_WRITE_WIRE=1 writes the files that are missing, once, when the directory is first made.
const (
	wireDir     = "../../testdata/wire/1"
	wireSource  = publicURL + "/acme/prod"           // the source channel's prefix
	wireFeedURL = publicURL + "/api/v1/tenants/acme" // the source tenant's feed
	wirePath    = "/acme/prod"
	wireAPI     = "/api/v1/tenants/acme"
)

var wireFiles = []string{"well-known.json", "names.json", "revocations.json", "attestation-keys.json", "feed-request.json",
	"feed-answer.json", "feed-document.json", "plan.json", "bundle.kbundle"}

// wireNet is the network between two kistas, without one: live, it calls the source kista's own
// handler and records what crossed; replaying, it answers with the fixtures' bytes.
type wireNet struct {
	h      http.Handler
	replay map[string][]byte // "GET <path>" or "POST <path>": the answer's bytes
	sent   map[string][]byte // "<path>": the first request body sent there
	got    map[string][]byte // "GET <path>": the last answer received
}

func (n *wireNet) do(method, raw string, hdr http.Header, body []byte) (int, http.Header, []byte) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, nil, nil
	}
	key := method + " " + u.Path
	if method == http.MethodPost {
		if _, ok := n.sent[u.Path]; !ok {
			n.sent[u.Path] = bytes.Clone(body)
		}
	}
	if n.h == nil {
		b, ok := n.replay[key]
		if !ok {
			return http.StatusNotFound, http.Header{}, nil
		}
		return http.StatusOK, http.Header{}, b
	}
	req := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(body))
	req.Host = u.Host
	for k, v := range hdr {
		req.Header[k] = v
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	n.h.ServeHTTP(rec, serve.WithHTTPS(req))
	if rec.Code == http.StatusOK {
		n.got[key] = bytes.Clone(rec.Body.Bytes())
	}
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

func wireErr(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return egress.ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return &egress.AuthError{Status: status, Host: "kista.example"}
	}
	return &egress.StatusError{Status: status, Host: "kista.example"}
}

func (n *wireNet) Get(_ context.Context, raw string) ([]byte, http.Header, error) {
	status, hdr, body := n.do(http.MethodGet, raw, nil, nil)
	return body, hdr, wireErr(status)
}

func (n *wireNet) PostJSON(_ context.Context, raw string, hdr http.Header, body []byte, _ int64) ([]byte, error) {
	status, _, ans := n.do(http.MethodPost, raw, hdr, body)
	return ans, wireErr(status)
}

func (n *wireNet) Download(_ context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error) {
	hdr := o.Header.Clone()
	if hdr == nil {
		hdr = http.Header{}
	}
	if o.ETag != "" {
		hdr.Set("If-None-Match", o.ETag)
	}
	status, ans, body := n.do(http.MethodGet, raw, hdr, nil)
	if status == http.StatusNotModified {
		return egress.Download{NotModified: true, ETag: o.ETag}, nil
	}
	if err := wireErr(status); err != nil {
		return egress.Download{}, err
	}
	if o.MaxBytes > 0 && int64(len(body)) > o.MaxBytes {
		return egress.Download{}, egress.ErrTooLarge
	}
	_, err := w.Write(body)
	return egress.Download{ETag: ans.Get("ETag"), Size: int64(len(body))}, err
}

func (n *wireNet) PortAllowed(uint16) bool { return true }

// wireKista is a kista downstream of the source: a gateway (it mirrors, subscribes, exports) or a
// reader of the fixtures (it mirrors, subscribes, imports).
type wireKista struct {
	t     *testing.T
	m     *mgmt
	ups   *upstream.Service
	feeds *attachment.Feeds
	lic   *licenses.Service
}

func newWireKista(t *testing.T, net *wireNet, token string) *wireKista {
	t.Helper()
	m := newMgmt(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "src", Tenants: []string{"*"}, Prefixes: []string{publicURL + "/"},
		Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.DiscardHandler)
	rel := &release.Service{Store: m.st, Blob: m.blob, Signers: m.env.keys, Authz: authz.ServerAdmin{}}
	att := &attachment.Service{Store: m.st, Blob: m.blob, Authz: authz.ServerAdmin{}}
	k := &wireKista{t: t, m: m}
	k.feeds = &attachment.Feeds{Store: m.st, Authz: authz.ServerAdmin{}, Attachments: att, Fetch: net, Credentials: creds, Log: quiet}
	k.lic = &licenses.Service{Store: m.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: net, Credentials: creds, Log: quiet}
	k.ups = &upstream.Service{Store: m.st, Releases: rel, Blob: m.blob, Fetch: net, Authz: authz.ServerAdmin{}, MaxBody: 1 << 20, MaxIngests: 4,
		TempDir: t.TempDir(), Log: quiet, Feeds: k.feeds, Licenses: k.lic, Credentials: creds}
	return k
}

// tenant makes a tenant with a signed channel prod serving DuckDB v2.0.0.
func (k *wireKista) tenant(name string) {
	k.t.Helper()
	ten := k.m.env.ten
	fail := func(err error) {
		k.t.Helper()
		if err != nil {
			k.t.Fatal(err)
		}
	}
	_, err := ten.CreateTenant(ctx, admin, name, "", "", "")
	fail(err)
	_, err = ten.CreateChannel(ctx, admin, name, "prod", store.ChannelSigned, "")
	fail(err)
	_, err = ten.SetChannelVersions(ctx, admin, name, "prod", []string{"v2.0.0"}, nil)
	fail(err)
	fail(signer.GenerateKeyFile(filepath.Join(k.m.dir, name+".pem")))
	_, err = k.m.env.keys.Add(ctx, admin, name, "prod", "file:"+name+".pem", true)
	fail(err)
}

func (k *wireKista) mirror(tenant string, keys []string, entries ...string) store.Upstream {
	k.t.Helper()
	sp := upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: wireSource, Channel: "prod", Mode: store.ModeMirror,
		Visibility: store.Public, Revocations: true, Keys: keys, Platforms: []string{"linux_amd64"}}
	for _, e := range entries {
		sp.Entries = append(sp.Entries, store.UpstreamEntry{Name: e})
	}
	u, err := k.ups.Add(ctx, admin, tenant, sp)
	if err != nil {
		k.t.Fatalf("the mirror: %v", err)
	}
	return u
}

func (k *wireKista) sync(tenant string) upstream.Result {
	k.t.Helper()
	u, err := k.ups.Sync(ctx, admin, tenant, "src", false)
	if err != nil {
		k.t.Fatal(err)
	}
	res, err := k.ups.RunUpstream(ctx, u.ID, func(context.Context) error { return nil })
	if err != nil {
		k.t.Fatalf("the mirror's run: %v", err)
	}
	return res
}

func (k *wireKista) feedRun(tenant string) attachment.RunResult {
	k.t.Helper()
	f, err := k.feeds.Sync(ctx, admin, tenant, "reviews")
	if err != nil {
		k.t.Fatal(err)
	}
	res, err := k.feeds.RunFeed(ctx, f.ID, func(context.Context) error { return nil })
	if err != nil {
		k.t.Fatalf("the feed's run: %v", err)
	}
	return res
}

// wellKnownKeys are the fingerprints of the keys a .well-known file lists.
func wellKnownKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var wk struct {
		Keys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal(data, &wk); err != nil || len(wk.Keys) == 0 {
		t.Fatalf("the .well-known file: %s %v", data, err)
	}
	var out []string
	for _, k := range wk.Keys {
		pub, err := extfile.ParsePublicKey(k)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, extfile.Fingerprint(pub))
	}
	return out
}

// wireProduce plays the source and its gateway with this binary and answers what crossed the wire:
// the fixtures' files, as they would be written today.
func wireProduce(t *testing.T) map[string][]byte {
	t.Helper()
	fail := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// the source: tenant acme at publicURL, serving an extension that declares a licence key
	src := newMgmt(t)
	pemKey, err := extfile.MarshalPublicKeyPEM(&src.licKey.PublicKey)
	fail("the licence key", err)
	block, err := extfile.EncodeLicenseKeys(pemKey)
	fail("the licence key block", err)
	add := func(version string) store.Release {
		t.Helper()
		r, _, err := src.env.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(keyed(t, version, block, "wire_duckdb_cpp_init")),
			release.AddOptions{Name: "wire"})
		fail("wire "+version, err)
		return r
	}
	first := add("1.0")
	fail("the attestation key file", signer.GenerateKeyFile(filepath.Join(src.dir, "att.pem")))
	ak, err := src.keySvc.AddAttestation(ctx, admin, "acme", "file:att.pem", true)
	fail("the attestation key", err)
	_, err = src.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|feeder", []string{"feed"}, tenants.GrantOn{Channel: "prod"})
	fail("the feed grant", err)
	token := src.keys.sign(t, idpURL, map[string]any{"sub": "feeder", "aud": publicURL + "/acme"})

	// the gateway: it mirrors the source's channel, applies its revocations, subscribes to its feed
	// for export, and holds a licence of the extension
	net := &wireNet{h: src.h, sent: map[string][]byte{}, got: map[string][]byte{}}
	gw := newWireKista(t, net, token)
	gw.tenant("gw")
	wk, _, err := net.Get(ctx, wireSource+"/.well-known/duckdb-extension-repo.json")
	fail("the .well-known file", err)
	keys := wellKnownKeys(t, wk)
	gw.mirror("gw", keys, "wire")
	if res := gw.sync("gw"); res.Counts[store.CellReleased] != 1 || res.Error != "" {
		t.Fatalf("the gateway's first run: %+v", res)
	}
	// the source releases 1.1 and yanks 1.0: the gateway takes the new one and applies the revocation
	add("1.1")
	_, err = src.env.rel.Apply(ctx, admin, "acme", "prod", "", first.ID, release.Yank, 0)
	fail("the yank", err)
	if res := gw.sync("gw"); res.Counts[store.CellReleased] != 1 || res.Counts["revoked"] != 1 || res.Error != "" {
		t.Fatalf("the gateway's second run: %+v", res)
	}
	body := ""
	for _, x := range src.call(t, "GET", wireAPI+"/channels/prod/extensions/wire", "", "").json(t)["releases"].([]any) {
		if r := x.(map[string]any); r["version"] == "1.1" {
			body = r["body_hash"].(string)
		}
	}
	if body == "" {
		t.Fatal("no wire 1.1 at the source")
	}
	// the source's documents: a review offered to readers, and notes offered and withdrawn
	readers, yes, no := store.VisibleReaders, true, false
	rv, err := src.att.Attach(ctx, admin, "acme", pkgtype.Of("wire"), attachment.AttachInput{BodyHash: body, Kind: store.KindReview,
		MediaType: attachment.Review, Title: "Review of wire 1.1", Content: []byte(review)})
	fail("the review", err)
	_, err = src.att.SetFlags(ctx, admin, "acme", pkgtype.Of("wire"), rv.ID, &readers, &yes, 0)
	fail("offering the review", err)
	notes, err := src.att.Attach(ctx, admin, "acme", pkgtype.Of("wire"), attachment.AttachInput{BodyHash: body, Kind: store.KindNotes,
		MediaType: attachment.Markdown, Title: "Notes", Content: []byte("# Notes\n")})
	fail("the notes", err)
	_, err = src.att.SetFlags(ctx, admin, "acme", pkgtype.Of("wire"), notes.ID, nil, &yes, 0)
	fail("offering the notes", err)
	_, err = src.att.SetFlags(ctx, admin, "acme", pkgtype.Of("wire"), notes.ID, nil, &no, 0)
	fail("withdrawing the notes", err)
	_, err = gw.feeds.Add(ctx, admin, "gw", attachment.FeedSpec{Name: "reviews", URL: wireFeedURL, Credential: "src",
		Keys: []string{ak.Fingerprint}, Readers: true, Export: true})
	fail("the gateway's feed", err)
	if res := gw.feedRun("gw"); res.Counts["received"] != 1 || res.Counts["kept"] != 1 || res.Error != "" {
		t.Fatalf("the gateway's feed run: %+v", res)
	}
	// a licence, as its issuer signs it (kista issues none): the gateway holds it for the mirror's entry
	now := time.Now().Unix()
	lic := vectors.Sign(src.licKey, vectors.Header(src.licKey), `{"v":1,"iss":"https://issuer.example","jti":"lic-wire-1","sub":"acme",`+
		`"holder":"Acme","ext":"wire","plan":"pro","iat":`+itoa(now-60)+`,"exp":4102444800}`)
	_, err = gw.lic.Import(ctx, admin, "gw", "", "upstream:src", lic, licenses.Create)
	fail("the gateway's licence", err)

	// the dash routes and the .well-known file, as anyone reads them
	for _, p := range []string{"/-/names", "/-/revocations"} {
		_, _, err := net.Get(ctx, wireSource+p)
		fail(p, err)
	}

	// the site: a tenant with a bundle-mode upstream and feed, its plan, and the gateway's export
	gw.tenant("site")
	_, err = gw.ups.Add(ctx, admin, "site", upstream.Spec{Name: "air", Kind: store.UpstreamRepo, Prefix: wireSource, Channel: "prod",
		Mode: store.ModeBundle, Visibility: store.Public, Revocations: true, Keys: keys, Platforms: []string{"linux_amd64"},
		Entries: []store.UpstreamEntry{{Name: "wire"}}})
	fail("the site's upstream", err)
	_, err = gw.feeds.Add(ctx, admin, "site", attachment.FeedSpec{Name: "reviews", URL: wireFeedURL, Keys: []string{ak.Fingerprint},
		Readers: true, Mode: store.FeedBundle})
	fail("the site's feed", err)
	keyPEM, fp, err := bundle.GenerateKey()
	fail("the bundle key", err)
	priv, err := bundle.ParsePrivateKey(keyPEM)
	fail("the bundle key", err)
	plan, err := gw.ups.Plan(ctx, admin, "site", upstream.PlanTargets{Licenses: true}, []string{fp})
	fail("the plan", err)
	planData, err := json.MarshalIndent(plan, "", "  ") // as kista admin bundle plan writes it
	fail("the plan", err)
	planData = append(planData, '\n')
	var out bytes.Buffer
	res, err := gw.ups.Export(ctx, admin, "gw", &out, upstream.ExportOptions{Plan: planData, Key: priv, Licenses: true,
		PublicURL: "https://gw.example", Version: "kista v0.2.0"})
	fail("the export", err)
	if res.Failed() || len(res.Sources) != 1 || res.Sources[0].Counts[bundle.Fetched] != 1 || len(res.Sources[0].Licenses) != 1 ||
		res.Sources[0].Counts["revocations"] != 1 || len(res.Feeds) != 1 || res.Feeds[0].Counts["statements"] != 2 ||
		res.Feeds[0].Counts["documents"] != 1 {
		t.Fatalf("the export: %+v %+v", res.Sources, res.Feeds)
	}
	doc := ""
	for k := range net.got {
		if strings.HasPrefix(k, "GET "+wireAPI+"/feed/documents/") {
			doc = k
		}
	}
	return map[string][]byte{
		"well-known.json":       wk,
		"names.json":            net.got["GET "+wirePath+"/-/names"],
		"revocations.json":      net.got["GET "+wirePath+"/-/revocations"],
		"attestation-keys.json": net.got["GET "+wireAPI+"/attestation-keys"],
		"feed-request.json":     net.sent[wireAPI+"/feed"],
		"feed-answer.json":      net.got["POST "+wireAPI+"/feed"],
		"feed-document.json":    net.got[doc],
		"plan.json":             planData,
		"bundle.kbundle":        out.Bytes(),
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// members lists a JSON document's member paths ("a.b[].c"); a string that is itself a base64 JSON
// object (a statement) is walked too.
func members(t *testing.T, data []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("not JSON: %v\n%.200s", err, data)
	}
	set := map[string]bool{}
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				p := at + "." + k
				set[p] = true
				walk(e, p)
			}
		case []any:
			for _, e := range x {
				walk(e, at+"[]")
			}
		case string:
			if raw, err := base64.StdEncoding.DecodeString(x); err == nil && len(raw) > 1 && raw[0] == '{' {
				var inner any
				if json.Unmarshal(raw, &inner) == nil {
					walk(inner, at+"{}")
				}
			}
		}
	}
	walk(v, "")
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// bundleParts reads a bundle whole: its manifest, and its entries' bytes by what they are.
type bundleParts struct {
	manifest  []byte
	wellKnown []byte
	files     map[string][]byte // by cell name: the gzip entry
	keys      []byte
	batches   [][]byte
	docs      int
	m         *bundle.Manifest
}

func readBundle(t *testing.T, data []byte, pinned []string) bundleParts {
	t.Helper()
	b, err := bundle.Open(bytes.NewReader(data), time.Now(), func(fp string) bool { return pinned == nil || slices.Contains(pinned, fp) })
	if err != nil || !b.Signed {
		t.Fatalf("the bundle: %v", err)
	}
	p := bundleParts{manifest: b.ManifestBytes, files: map[string][]byte{}, m: b.Manifest}
	byFile := b.Manifest.CellsByFile()
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil || it.Damaged != "" {
			t.Fatalf("the bundle: %v %s", err, it.Damaged)
		}
		switch {
		case it.WellKnown != nil:
			p.wellKnown = it.Data
		case it.R != nil:
			raw, err := io.ReadAll(it.R)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range byFile[it.File] {
				p.files[ref.Cell.Name] = raw
			}
		case it.Feed != nil:
			fe, _ := bundle.ParseFeedEntry(it.Feed.Entries[it.Entry].Path)
			switch {
			case fe.Keys:
				p.keys = it.Data
			case fe.Batch > 0:
				p.batches = append(p.batches, it.Data)
			default:
				p.docs++
			}
		}
	}
	if missing := b.Missing(); len(missing) != 0 {
		t.Fatalf("the bundle misses %v", missing)
	}
	return p
}

// wireMembers are a fixture's member paths, by part for the bundle.
func wireMembers(t *testing.T, name string, data []byte) map[string][]string {
	t.Helper()
	if name != "bundle.kbundle" {
		return map[string][]string{name: members(t, data)}
	}
	p := readBundle(t, data, nil)
	out := map[string][]string{"bundle manifest": members(t, p.manifest), "bundle .well-known": members(t, p.wellKnown),
		"bundle keys file": members(t, p.keys)}
	for _, b := range p.batches {
		out["bundle batch"] = append(out["bundle batch"], members(t, b)...)
	}
	return out
}

func TestWireFixtures(t *testing.T) {
	// the producer check has teeth: a member dropped anywhere, in a statement too, is a path gone
	inner := base64.StdEncoding.EncodeToString([]byte(`{"seq":1,"summary":{"x":1}}`))
	if got := members(t, []byte(`{"a":{"b":[{"c":1}]},"statement":"`+inner+`"}`)); !slices.Equal(got,
		[]string{".a", ".a.b", ".a.b[].c", ".statement", ".statement{}.seq", ".statement{}.summary", ".statement{}.summary.x"}) {
		t.Fatalf("member paths: %v", got)
	}
	fresh := wireProduce(t)
	for _, name := range wireFiles {
		if len(fresh[name]) == 0 {
			t.Fatalf("the producers wrote no %s", name)
		}
	}
	if os.Getenv("KISTA_WRITE_WIRE") == "1" {
		if err := os.MkdirAll(wireDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range wireFiles {
			p := filepath.Join(wireDir, name)
			if _, err := os.Stat(p); err == nil {
				continue // a wire fixture is never rewritten
			}
			if err := os.WriteFile(p, fresh[name], 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	fix := map[string][]byte{}
	for _, name := range wireFiles {
		b, err := os.ReadFile(filepath.Join(wireDir, name))
		if err != nil {
			t.Fatalf("a wire fixture is missing (they are committed, never regenerated): %v", err)
		}
		fix[name] = b
	}

	// the producers: what this binary writes today has every member the fixtures have
	for _, name := range wireFiles {
		was, is := wireMembers(t, name, fix[name]), wireMembers(t, name, fresh[name])
		for part, paths := range was {
			for _, p := range paths {
				if !slices.Contains(is[part], p) {
					t.Errorf("%s: this release's producer no longer writes %s", part, p)
				}
			}
		}
	}

	// the readers: a kista of this binary downstream of the fixtures
	var plan bundle.Plan
	if err := bundle.Decode(fix["plan.json"], &plan); err != nil || plan.Validate() != nil || len(plan.Sources) != 1 || len(plan.Feeds) != 1 ||
		!plan.Licenses || !plan.AcceptsRevocations {
		t.Fatalf("plan.json: %v %v %+v", err, plan.Validate(), plan)
	}
	parts := readBundle(t, fix["bundle.kbundle"], plan.BundleKeys)
	if parts.m.Format != bundle.ManifestFormatV2 || len(parts.m.Sources) != 1 || len(parts.m.Sources[0].Licenses) != 1 ||
		len(parts.m.Sources[0].Revocations) != 1 || len(parts.m.Feeds) != 1 || parts.docs != 1 || len(parts.files["wire"]) == 0 {
		t.Fatalf("bundle.kbundle's manifest: %+v", parts.m)
	}
	var ans attachment.FeedAnswer
	if err := json.Unmarshal(fix["feed-answer.json"], &ans); err != nil || len(ans.Statements) != 2 {
		t.Fatalf("feed-answer.json: %v", err)
	}
	stmt, _ := base64.StdEncoding.DecodeString(ans.Statements[0].Statement)
	var offer attachment.Statement
	if err := json.Unmarshal(stmt, &offer); err != nil || offer.IssuerID == "" || offer.Issuer != wireFeedURL {
		t.Fatalf("the feed's first statement: %s %v", stmt, err)
	}
	net := &wireNet{sent: map[string][]byte{}, got: map[string][]byte{}, replay: map[string][]byte{
		"GET " + wirePath + "/.well-known/duckdb-extension-repo.json":      fix["well-known.json"],
		"GET " + wirePath + "/-/names":                                     fix["names.json"],
		"GET " + wirePath + "/-/revocations":                               fix["revocations.json"],
		"GET " + wireAPI + "/attestation-keys":                             fix["attestation-keys.json"],
		"POST " + wireAPI + "/feed":                                        fix["feed-answer.json"],
		"GET " + wireAPI + "/feed/documents/" + offer.IssuerID:             fix["feed-document.json"],
		"GET " + wirePath + "/v2.0.0/linux_amd64/wire.duckdb_extension.gz": parts.files["wire"], // the file the bundle carries
	}}
	r := newWireKista(t, net, "T")
	keys := wellKnownKeys(t, fix["well-known.json"])
	revoked := parts.m.Sources[0].Revocations[0]
	// the revoked build is one a downstream held before the source revoked it (a revoked release is
	// served and exported no more): each upstream here is given it, as its record only
	hold := func(tenant string, u store.Upstream) {
		t.Helper()
		tn, err := r.m.st.GetTenant(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		b := store.Build{TenantID: tn.ID, Source: store.UpstreamSource(u.ID), Name: revoked.Name, PackageVersion: revoked.Version,
			Platform: revoked.Platform, ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: revoked.BodyHash, Origin: store.OriginUpstream,
			CreatedBy: "test"}
		if err := r.m.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) }); err != nil {
			t.Fatal(err)
		}
	}
	stored := func(tenant, name string) []store.UpstreamRevocation {
		t.Helper()
		rs, err := r.ups.RevocationsOf(ctx, admin, tenant, name)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}

	// online: a mirror reads the .well-known file, -/names ("*") and -/revocations
	r.tenant("gw")
	u := r.mirror("gw", keys, "*")
	hold("gw", u)
	res := r.sync("gw")
	var names struct {
		Names []string `json:"names"`
	}
	_ = json.Unmarshal(fix["names.json"], &names)
	got, err := r.ups.Get(ctx, admin, "gw", "src")
	if err != nil || res.Error != "" || !got.RemoteKista || res.Counts[store.CellReleased] != 1 || !slices.Contains(got.Discovered, "wire") ||
		!slices.Contains(names.Names, "wire") || res.Counts["revocations_unknown"] != 0 {
		t.Fatalf("the mirror's run over the fixtures: %+v, discovered %v, names.json %v, %v", res, got.Discovered, names.Names, err)
	}
	for _, p := range res.Problems {
		if p.Outcome == "revocations" || p.Outcome == "names" {
			t.Fatalf("the mirror could not read a dash route's fixture: %+v", p)
		}
	}
	if rs := stored("gw", "src"); len(rs) != 1 || rs[0].PackageVersion != revoked.Version || rs[0].State != store.RevokeYanked {
		t.Fatalf("revocations.json at the mirror: %+v", rs)
	}

	// online: a feed reads the keys file, the feed's answer and the document
	if _, err := r.feeds.Add(ctx, admin, "gw", attachment.FeedSpec{Name: "reviews", URL: wireFeedURL, Credential: "src", Keys: plan.Feeds[0].Keys,
		Readers: true}); err != nil {
		t.Fatal(err)
	}
	if fr := r.feedRun("gw"); fr.Error != "" || fr.Counts["received"] != 1 || fr.Counts["refused"] != 0 || fr.Counts["waiting"] != 0 {
		t.Fatalf("the feed's run over the fixtures: %+v", fr)
	}
	// and the request it sends has every member the fixture's has
	for _, p := range members(t, fix["feed-request.json"]) {
		if !slices.Contains(members(t, net.sent[wireAPI+"/feed"]), p) {
			t.Errorf("feed-request.json: this release's feed client no longer sends %s", p)
		}
	}

	// offline: an import of the bundle, into an upstream and a feed made before the plan was
	r.tenant("site")
	clock := r.m.st.Now
	r.m.st.Now = func() time.Time { return plan.CreatedAt.Add(-time.Hour) }
	air, err := r.ups.Add(ctx, admin, "site", upstream.Spec{Name: "air", Kind: store.UpstreamRepo, Prefix: plan.Sources[0].Prefix, Channel: "prod",
		Mode: store.ModeBundle, Visibility: store.Public, Revocations: true, Keys: plan.Sources[0].Keys, Platforms: []string{"linux_amd64"},
		Entries: []store.UpstreamEntry{{Name: "wire"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.feeds.Add(ctx, admin, "site", attachment.FeedSpec{Name: "reviews", URL: plan.Feeds[0].URL, Keys: plan.Feeds[0].Keys,
		Readers: true, Mode: store.FeedBundle}); err != nil {
		t.Fatal(err)
	}
	r.m.st.Now = clock
	hold("site", air)
	imp, err := r.ups.Import(ctx, admin, "site", bytes.NewReader(fix["bundle.kbundle"]), upstream.ImportOptions{BundleKeys: plan.BundleKeys, Holder: "test"})
	if err != nil || imp.Problem != "" || len(imp.Targets) != 2 || len(imp.LeftOut) != 0 {
		t.Fatalf("the import: %+v %v", imp, err)
	}
	for _, tg := range imp.Targets {
		switch {
		case tg.Status != upstream.TargetImported:
			t.Fatalf("a target: %+v", tg)
		case tg.Upstream == "air" && (tg.Result.Counts[store.CellReleased] != 1 || len(tg.Result.Problems) != 0):
			t.Fatalf("the upstream's import: %+v", tg.Result)
		case tg.Feed == "reviews" && (tg.FeedRun == nil || tg.FeedRun.Counts["received"] != 1 || tg.FeedRun.Counts["refused"] != 0 ||
			tg.FeedRun.Counts["waiting"] != 0 || tg.FeedRun.Error != ""):
			t.Fatalf("the feed's import: %+v", tg.FeedRun)
		}
	}
	if imp.Licenses == nil || imp.Licenses.Counts["stored"] != 1 || len(imp.Licenses.Refused) != 0 {
		t.Fatalf("the bundle's licence: %+v", imp.Licenses)
	}
	if rs := stored("site", "air"); len(rs) != 1 || rs[0].State != revoked.State || rs[0].PackageVersion != revoked.Version {
		t.Fatalf("the bundle's revocation: %+v", rs)
	}
	site, _ := r.m.st.GetTenant(ctx, "site")
	docs, err := r.m.st.BodyAttachments(ctx, site.ID, offer.BodyHash)
	if err != nil || len(docs) != 1 || docs[0].Title != offer.Title || docs[0].Kind != store.KindReview || docs[0].Visibility != store.VisibleReaders ||
		docs[0].Issuer != wireFeedURL {
		t.Fatalf("the bundle's document: %+v %v", docs, err)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}
