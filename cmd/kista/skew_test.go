package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0046, version skew: what this kista does with a bundle of an exporter newer than it, and the
// wire fixtures every later release must still read.

// skew, set by the skew test only, makes the bundle fixtures stand in for a newer exporter: the
// plan's and the manifest's JSON rewritten beyond what this binary writes, statements a newer
// source signed, and a bundle key to sign with (kista bundle sign checks what it signs as this
// release does, and a newer exporter signs with its own).
var skew skewHooks

type skewHooks struct {
	key            ed25519.PrivateKey
	plan, manifest func(map[string]any)
	body, doc      string // the feed's body and document
	statements     []skewStatement
}

// skewStatement is a statement of a newer source: mod changes its known members, raw its JSON, note
// is the exporting kista's.
type skewStatement struct {
	mod  func(*attachment.Statement)
	raw  func(map[string]any)
	note string
}

func (skewHooks) rewrite(t *testing.T, data []byte, fn func(map[string]any)) []byte {
	t.Helper()
	if fn == nil {
		return data
	}
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// newerToken is a licence in a later format: v 2, a member and a header this binary does not know.
func newerToken(ext string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"kista-license+jwt","kid":"sha256:`+strings.Repeat("0", 64)+`","crit":["seats"]}`)) + "." +
		enc.EncodeToString([]byte(`{"v":2,"iss":"https://issuer.example","jti":"lic-9","sub":"acme","ext":"`+ext+`","seats":{"max":5}}`)) + "." +
		enc.EncodeToString(make([]byte, 256))
}

// One bundle of files and one of statements from a stand-in newer exporter, through the CLI on
// SQLite: an extra manifest member is ignored; a higher format is refused whole, saying which side
// to upgrade; a source of an unknown type is left out; a cell of an unknown outcome is that cell's
// problem; an unknown revocation state is a yank; a v 2 licence is refused by itself; a statement of
// an unknown kind and one of v 2 wait, one with an extra member is stored. The online cases (the
// feed, the dash routes' api, the licence relay) are in their packages' tests.
func TestSkew(t *testing.T) {
	t.Cleanup(func() { skew = skewHooks{} })
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fp := bundle.Fingerprint(pub)
	later := func(m map[string]any) { // dated after the site's upstream and feed are created
		at := time.Now().UTC().Add(time.Minute).Truncate(time.Second).Format(time.RFC3339)
		m["created_at"] = at
		m["plan"].(map[string]any)["created_at"] = at
	}

	// the files: a newer exporter's manifest
	var body string
	skew = skewHooks{key: priv,
		plan: func(p map[string]any) { p["licenses"] = true },
		manifest: func(m map[string]any) {
			later(m)
			m["exporter"].(map[string]any)["build"] = map[string]any{"commit": "abc", "dirty": false} // an extra member
			m["attestations"] = []any{map[string]any{"kind": "slsa"}}
			src := m["sources"].([]any)
			src[0].(map[string]any)["mirror_of"] = "https://origin.example.org"
			// a source of a package type this kista does not know, before the repository's (canonical order)
			// (canonical order); nothing of it is read, whatever shape its members took there
			m["sources"] = append([]any{map[string]any{"kind": "adbc-registry", "package_type": "adbc_driver",
				"prefix": "https://drivers.example.org/adbc", "well_known": "sources/9/index.json", "licenses": map[string]any{"by": "name"},
				"cells": []any{map[string]any{"driver": "postgresql", "arch": "amd64", "outcome": 7}}}}, src...)
		}}
	plan, signed := bundleFixture(t, dir, fp, func(m *bundle.Manifest) {
		src := &m.Sources[0]
		body = src.Cells[0].BodyHash
		// a cell in an outcome of a later release
		src.Cells = append(src.Cells, bundle.Cell{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "y", Outcome: "quarantined",
			Detail: "held for review"})
		m.Format = bundle.ManifestFormatV2
		src.Revocations = []bundle.Revocation{{Name: "x", Version: "1.0", Platform: "linux_amd64", BodyHash: body, State: "recalled", Reason: "CVE-9"}}
		m.Plan.Licenses = true
		src.Licenses = []bundle.License{{Name: "x", Token: newerToken("x")}}
	})
	var p bundle.Plan
	data, _ := os.ReadFile(plan)
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}

	// the same exporter writing a higher format
	d3 := t.TempDir()
	skew.manifest = func(m map[string]any) {
		m["format"] = "kista-bundle/v3"
		m["sources"] = map[string]any{"by_name": "another shape altogether"}
	}
	_, v3 := bundleFixture(t, d3, fp, nil)

	// the statements: a newer source's, numbered 4 to 6 after the fixture's three
	fdir := t.TempDir()
	skew = skewHooks{key: priv, body: body,
		doc: `{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest",` +
			`"capabilities":[],"network":[],"files":[],"processes":[],"licences":[],"findings":[],"text":"ok","confidence":"high"}`,
		manifest: func(m map[string]any) {
			later(m)
			m["id"] = "0192f0c4-1b2a-7c3d-8e4f-0123456789ac"
			m["relayed"] = true
		},
		statements: []skewStatement{
			{mod: func(s *attachment.Statement) { s.Kind, s.MediaType = "report", "application/vnd.kista.report.v1+json" }}, // 4: an unknown kind
			{raw: func(m map[string]any) { m["envelope"] = map[string]any{"version": 3, "parts": []any{"summary"}} }},       // 5: an extra member
			{raw: func(m map[string]any) { m["v"] = 2; m["size"] = map[string]any{"bytes": 10} }},                           // 6: a newer format
		}}
	_, fsigned, afp := feedFixture(t, fdir, fp, 0, "")
	skew = skewHooks{}

	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"rotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: "+keys+" }\n"+
		"serve: { public_url: 'https://kista.example' }\n"+
		"upstreams: { bundle_keys: [{ fingerprint: '"+fp+"', tenants: [acme] }] }\n"), 0o600)
	admin := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	has := func(what, got string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("%s: no %q in\n%s", what, w, got)
			}
		}
	}
	admin(0, "migrate")
	admin(0, "tenant", "create", "acme")
	admin(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", coreFile(t, "json"))
	admin(0, "channel", "create", "acme/prod", "-kind", "signed")
	admin(0, "channel", "versions", "acme/prod", "-add", "v2.0.0")
	admin(0, "key", "add", "acme/prod", "-signer", "file:a.pem", "-active")
	admin(0, "upstream", "add", "acme", "air", "-kind", "repository", "-mode", "bundle", "-channel", "prod",
		"-platforms", "linux_amd64", "-extensions", "x,y", "-prefix", p.Sources[0].Prefix, "-keys", p.Sources[0].Keys[0], "-revocations")
	admin(0, "feed", "add", "acme", "reviews", "-url", "https://gw.example.org/api/v1/tenants/reviews", "-key", afp, "-mode", "bundle")

	// a higher format is refused whole, by every reader, saying which side to upgrade
	var out, errb bytes.Buffer
	if code := run([]string{"bundle", "inspect", v3}, &out, &errb); code == 0 {
		t.Errorf("inspect read a kista-bundle/v3 bundle:\n%s", out.String())
	}
	has("inspect of a higher format", out.String()+errb.String(), "kista-bundle/v3", "upgrade the importing kista")
	has("the import of a higher format", admin(1, "bundle", "import", "acme", v3), "kista-bundle/v3", "upgrade the importing kista")

	// the files
	out.Reset()
	errb.Reset()
	_ = run([]string{"bundle", "inspect", signed, "-plan", plan}, &out, &errb)
	has("inspect", out.String()+errb.String(), "package type adbc_driver, not known here", "quarantined")
	got := admin(0, "bundle", "import", "acme", signed)
	has("the import", got,
		"released 1", // x, whatever else the bundle held
		"source adbc-registry https://drivers.example.org/adbc was left out: package type adbc_driver is not known here",
		"the exporting kista's outcome quarantined is not known here", // y: that cell's problem
		"revoked 1",                            // the unknown state, taken as a yank and applied
		"licence x", `v "`, "format version 2", // the v 2 licence, refused by itself
	)
	has("the stored revocation", admin(0, "upstream", "revocations", "acme", "air"), "yanked", "recalled: CVE-9")
	has("upstream show", admin(0, "upstream", "show", "acme", "air"),
		"This kista is older than its source: 1 revocations in a state it does not know are taken as yanks.")

	// the statements
	got = admin(0, "bundle", "import", "acme", fsigned)
	// the fixture's two documents and the newer source's with an extra member (content travels by hash);
	// the unknown kind and the newer format wait
	has("the feed's import", got, "feed:reviews", "received 3", "waiting 2")
	if strings.Contains(got, "signature") || strings.Contains(got, "held ") {
		t.Errorf("a statement of a newer source held its body:\n%s", got)
	}
	has("feed show", admin(0, "feed", "show", "acme", "reviews"),
		"This kista is older than its source: documents wait for an upgrade (`newer`).", "status ok")
	// the same binary writes its next plan from the cursor: nothing waits at a lower level than its own
	next := filepath.Join(dir, "next.json")
	admin(0, "bundle", "plan", "acme", "-o", next)
	if pd, _ := os.ReadFile(next); !strings.Contains(string(pd), `"seq": 6`) {
		t.Errorf("the next plan does not ask from 6:\n%s", pd)
	}
}

// wireDir holds the bytes release 0.2 writes for each wire format, signed (spec 0046). They are
// never edited: every later release reads them. KISTA_WRITE_WIRE=1 writes the files that are
// missing, once, when the directory is first made.
const wireDir = "../../testdata/wire/1"

func TestWireFixtures(t *testing.T) {
	if os.Getenv("KISTA_WRITE_WIRE") == "1" {
		writeWire(t)
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(wireDir, name))
		if err != nil {
			t.Fatalf("a wire fixture is missing (they are committed, never regenerated): %v", err)
		}
		return b
	}
	cli := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 0 {
			t.Fatalf("kista %v: exit %d\n%s%s", args, code, out.String(), errb.String())
		}
		return out.String()
	}

	// statements, as the feed route answers them, with the keys file and the document they name
	keys, err := attachment.ParseKeysFile(read("attestation-keys.json"))
	if err != nil || len(keys) != 1 {
		t.Fatalf("the keys file: %v", err)
	}
	var ans attachment.FeedAnswer
	if err := json.Unmarshal(read("feed-answer.json"), &ans); err != nil || len(ans.Statements) != 2 || len(ans.Bodies) != 1 || ans.More {
		t.Fatalf("the feed's answer: %+v %v", ans, err)
	}
	doc := read("document.json")
	for i, w := range ans.Statements {
		stmt, err1 := base64.StdEncoding.DecodeString(w.Statement)
		sig, err2 := base64.StdEncoding.DecodeString(w.Signature)
		if err1 != nil || err2 != nil {
			t.Fatalf("statement %d: its encoding", i)
		}
		st, err := attachment.Verify(stmt, sig, keys)
		if err != nil || st.Major() != 1 || st.Seq != int64(i+1) || st.BodyHash != ans.Bodies[0].BodyHash {
			t.Fatalf("statement %d: %+v %v", i, st, err)
		}
		switch i {
		case 0:
			if st.Withdrawn || st.Kind != store.KindReview || st.MediaType != attachment.Review || st.ContentHash != blob.ContentHash(doc) ||
				st.Size != int64(len(doc)) || st.Audience != store.VisibleReaders || st.Origin != "tenant" {
				t.Fatalf("the offer: %+v", st)
			}
			if _, err := attachment.SummarizeReceived(st.Kind, st.MediaType, doc); err != nil {
				t.Fatalf("its document: %v", err)
			}
		case 1:
			if !st.Withdrawn || st.Kind != "" {
				t.Fatalf("the withdrawal: %+v", st)
			}
		}
	}

	// the dash routes' answers
	var names struct {
		API   *int      `json:"api"`
		Names *[]string `json:"names"`
		Next  string    `json:"next"`
	}
	if err := json.Unmarshal(read("names.json"), &names); err != nil || names.API == nil || *names.API != 1 || names.Names == nil || len(*names.Names) != 2 {
		t.Fatalf("-/names: %+v %v", names, err)
	}
	var revs struct {
		API  *int                  `json:"api"`
		Rows *[]release.RevokedRow `json:"rows"`
		Next string                `json:"next"`
	}
	if err := json.Unmarshal(read("revocations.json"), &revs); err != nil || revs.API == nil || *revs.API != 1 || revs.Rows == nil || len(*revs.Rows) != 3 {
		t.Fatalf("-/revocations: %+v %v", revs, err)
	}
	for _, r := range *revs.Rows {
		if store.RevocationRank(r.State) == 0 || release.ValidName(r.Name) != nil {
			t.Fatalf("a revoked row: %+v", r)
		}
	}

	// a licence, with the key it verifies under
	var lic struct {
		Token     string `json:"token"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(read("license.json"), &lic); err != nil {
		t.Fatal(err)
	}
	lk, err := license.ParseKeys([]string{lic.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := license.Parse(lic.Token)
	if err != nil || tok.Extension != "x" || tok.HolderID != "acme" {
		t.Fatalf("the licence: %+v %v", tok, err)
	}
	if err := tok.Verify(lk); err != nil {
		t.Fatalf("the licence's signature: %v", err)
	}

	// the plans and the signed bundles: the files with a licence and a revocation, and the statements
	for _, name := range []string{"source", "feed"} {
		var p bundle.Plan
		if err := bundle.Decode(read(name+"-plan.json"), &p); err != nil || p.Validate() != nil || p.Format != bundle.PlanFormat {
			t.Fatalf("%s-plan.json: %v %v", name, err, p.Validate())
		}
		f, err := os.Open(filepath.Join(wireDir, name+".kbundle"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := bundle.Open(f, time.Now(), func(fp string) bool { return len(p.BundleKeys) == 1 && fp == p.BundleKeys[0] })
		if err != nil || !b.Signed {
			t.Fatalf("%s.kbundle: %v", name, err)
		}
		for {
			it, err := b.Next()
			if err == io.EOF {
				break
			}
			if err != nil || it.Damaged != "" {
				t.Fatalf("%s.kbundle: %v %s", name, err, it.Damaged)
			}
			if it.R != nil {
				if _, err := io.Copy(io.Discard, it.R); err != nil {
					t.Fatalf("%s.kbundle: %v", name, err)
				}
			}
		}
		if missing := b.Missing(); len(missing) != 0 {
			t.Fatalf("%s.kbundle misses %v", name, missing)
		}
		_ = f.Close()
		m := b.Manifest
		switch name {
		case "source":
			if m.Format != bundle.ManifestFormatV2 || len(m.Sources) != 1 || len(m.Sources[0].Revocations) != 1 || len(m.Sources[0].Licenses) != 1 ||
				m.Sources[0].Cells[0].Outcome != bundle.Fetched {
				t.Fatalf("source.kbundle's manifest: %+v", m)
			}
		case "feed":
			if m.Format != bundle.ManifestFormat || len(m.Feeds) != 1 || len(m.Feeds[0].Entries) != 3 {
				t.Fatalf("feed.kbundle's manifest: %+v", m)
			}
		}
		got := cli("bundle", "inspect", filepath.Join(wireDir, name+".kbundle"), "-plan", filepath.Join(wireDir, name+"-plan.json"))
		if !strings.Contains(got, "consistent with the plan's pins") {
			t.Errorf("inspect %s.kbundle:\n%s", name, got)
		}
	}
}

// writeWire writes the wire fixtures that are missing, with keys made for the occasion and thrown
// away: the fixtures carry the public halves only.
func writeWire(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { skew = skewHooks{} })
	if err := os.MkdirAll(wireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		p := filepath.Join(wireDir, name)
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists: a wire fixture is never rewritten", p)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rk := func() *rsa.PrivateKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	mustJSON := func(v any) []byte {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}

	// statements
	ak := rk()
	afp := extfile.Fingerprint(&ak.PublicKey)
	pemStr, _ := extfile.MarshalPublicKeyPEM(&ak.PublicKey)
	write("attestation-keys.json", mustJSON(map[string]any{"keys": []any{map[string]any{"fingerprint": afp, "public_key": pemStr,
		"state": "active"}}}))
	doc := []byte(`{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-11-16T12:00:00Z","analyser":"Enterest",` +
		`"capabilities":["network"],"network":["api.example.org:443"],"files":[],"processes":[],"licences":["MIT"],` +
		`"findings":[{"severity":"low","title":"Telemetry","detail":"Sends a version check at load."}],"text":"# Review\n\nFine."}` + "\n")
	write("document.json", doc)
	const issuer = "https://kista.example.org/api/v1/tenants/acme"
	body := strings.Repeat("ab", 32)
	var ans attachment.FeedAnswer
	for seq, st := range []attachment.Statement{
		{Issuer: issuer, Key: afp, IssuerID: "0192f0c4-1b2a-7c3d-8e4f-0123456789d1", Seq: 1, BodyHash: body, Kind: store.KindReview,
			MediaType: attachment.Review, ContentHash: blob.ContentHash(doc), Size: int64(len(doc)), Title: "Enterest review",
			Summary: json.RawMessage(`{"findings":1,"verdict":"approved"}`), IssuedAt: "2026-11-16T12:00:00Z", ValidUntil: "2027-11-16T12:00:00Z",
			Origin: "tenant", Audience: store.VisibleReaders},
		{Issuer: issuer, Key: afp, IssuerID: "0192f0c4-1b2a-7c3d-8e4f-0123456789d0", Seq: 2, BodyHash: body, IssuedAt: "2026-11-16T12:05:00Z",
			Withdrawn: true},
	} {
		b, err := st.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := attachment.Sign(context.Background(), rsaSigner{ak}, b)
		if err != nil {
			t.Fatal(err)
		}
		ans.Statements = append(ans.Statements, attachment.WireStatement{Statement: base64.StdEncoding.EncodeToString(b),
			Signature: base64.StdEncoding.EncodeToString(sig)})
		ans.Bodies = []attachment.HighWater{{BodyHash: body, Seq: int64(seq + 1)}}
	}
	write("feed-answer.json", mustJSON(ans))

	// the dash routes
	write("names.json", mustJSON(serve.NamesAnswer{API: 1, Names: []string{"acl", "tresor"}}))
	row := func(v, state, reason string) release.RevokedRow {
		return release.RevokedRow{Name: "tresor", Version: v, Platform: "linux_amd64", BodyHash: body, State: state, Reason: reason}
	}
	write("revocations.json", mustJSON(serve.RevocationsAnswer{API: 1, Rows: []release.RevokedRow{row("1.0", store.RevokeWithdrawn, "CVE-2026-0001"),
		row("1.1", store.RevokeBlocked, "a bad build"), row("1.2", store.RevokeYanked, "")}}))

	// a licence
	lk := rk()
	lpem, _ := extfile.MarshalPublicKeyPEM(&lk.PublicKey)
	token := vectors.Sign(lk, vectors.Header(lk), `{"v":1,"iss":"https://issuer.example","jti":"lic-1","sub":"acme","holder":"Acme","ext":"x",`+
		`"plan":"pro","iat":1794830400,"exp":4102444800}`)
	write("license.json", mustJSON(map[string]string{"token": token, "public_key": lpem}))

	// the bundles, signed with a bundle key of their own
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fp := bundle.Fingerprint(pub)
	move := func(from, to string) {
		t.Helper()
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		write(to, b)
	}
	dir := t.TempDir()
	skew = skewHooks{key: priv, plan: func(p map[string]any) { p["licenses"] = true; p["accepts_revocations"] = true }}
	plan, signed := bundleFixture(t, dir, fp, func(m *bundle.Manifest) {
		src := &m.Sources[0]
		m.Format = bundle.ManifestFormatV2
		src.Revocations = []bundle.Revocation{{Name: "x", Version: "0.9", Platform: "linux_amd64", BodyHash: body, State: store.RevokeWithdrawn,
			Reason: "CVE-2026-0001"}}
		m.Plan.Licenses = true
		src.Licenses = []bundle.License{{Name: "x", Token: token}}
	})
	move(plan, "source-plan.json")
	move(signed, "source.kbundle")
	fdir := t.TempDir()
	skew = skewHooks{key: priv}
	fplan, fsigned, _ := feedFixture(t, fdir, fp, 0, "")
	move(fplan, "feed-plan.json")
	move(fsigned, "feed.kbundle")
	skew = skewHooks{}
}
