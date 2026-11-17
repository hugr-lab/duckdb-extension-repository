package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Spec 0046, version skew: what this kista does with a bundle of an exporter newer than it. (The
// wire fixtures every later release must still read are in internal/api/wire_test.go.)

// skew, set by the skew test only, makes the bundle fixtures stand in for a newer exporter: the
// plan's and the manifest's JSON rewritten beyond what this binary writes, statements a newer
// source signed, and a bundle key to sign with (kista bundle sign checks what it signs as this
// release does, and a newer exporter signs with its own).
var skew skewHooks

type skewHooks struct {
	key            ed25519.PrivateKey
	plan, manifest func(map[string]any)
	before, after  func(*bundle.Writer) // entries before the fixture's own, and after them
	body, doc      string               // the feed's body and document
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
			// a source of a package type this kista does not know, before the repository's (canonical
			// order); nothing of it is read, whatever shape its members took there
			m["sources"] = append([]any{map[string]any{"kind": "adbc-registry", "package_type": "adbc_driver",
				"prefix": "https://drivers.example.org/adbc", "well_known": "sources/9/index.json", "licenses": map[string]any{"by": "name"},
				"cells": []any{map[string]any{"driver": "postgresql", "arch": "amd64", "outcome": 7}}}}, src...)
		},
		// that source's own entries, real ones: its index before the known source's .well-known
		// file (it sorts first), its file after the known source's
		before: func(w *bundle.Writer) {
			idx := []byte(`{"drivers":["postgresql"]}`)
			w.Entry("sources/9/index.json", int64(len(idx)), bytes.NewReader(idx))
		},
		after: func(w *bundle.Writer) {
			w.Entry("files/"+strings.Repeat("f", 64)+".gz", 4096, bytes.NewReader(make([]byte, 4096)))
		}}
	files := func(m *bundle.Manifest) {
		src := &m.Sources[0]
		body = src.Cells[0].BodyHash
		// a cell in an outcome of a later release
		src.Cells = append(src.Cells, bundle.Cell{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "y", Outcome: "quarantined",
			Detail: "held for review"})
		m.Format = bundle.ManifestFormatV2
		src.Revocations = []bundle.Revocation{{Name: "x", Version: "1.0", Platform: "linux_amd64", BodyHash: body, State: "recalled", Reason: "CVE-9"}}
		m.Plan.Licenses = true
		src.Licenses = []bundle.License{{Name: "x", Token: newerToken("x")}}
	}
	plan, signed := bundleFixture(t, dir, fp, files)
	var p bundle.Plan
	data, _ := os.ReadFile(plan)
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}

	// kista bundle sign, run by this kista on the same bundle: the licence in a newer format is
	// skipped by itself, with a line, and the bundle is signed
	sdir := t.TempDir()
	var out, errb bytes.Buffer
	keyFile := filepath.Join(sdir, "bundle-key.pem")
	if code := run([]string{"bundle", "keygen", "-o", keyFile}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	sfp := strings.TrimSpace(out.String())
	withKey := skew
	skew.key = nil
	splan, unsigned := bundleFixture(t, sdir, sfp, files)
	skew = withKey
	out.Reset()
	if code := run([]string{"bundle", "sign", "-key", keyFile, "-plan", splan, "-in", unsigned, "-o", filepath.Join(sdir, "s.kbundle")}, &out, &errb); code != 0 {
		t.Fatalf("sign refused a bundle for one licence in a newer format: %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String()+errb.String(), "the licence of x is in a format newer than this kista reads") {
		t.Errorf("sign does not name the licence it skipped:\n%s%s", out.String(), errb.String())
	}

	// the same exporter writing a higher format
	d3 := t.TempDir()
	skew.before, skew.after = nil, nil
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

	// a higher format is refused whole, by every reader, saying which side to upgrade: the reader
	out.Reset()
	errb.Reset()
	if code := run([]string{"bundle", "inspect", v3}, &out, &errb); code == 0 {
		t.Errorf("inspect read a kista-bundle/v3 bundle:\n%s", out.String())
	}
	has("inspect of a higher format", out.String()+errb.String(), "kista-bundle/v3", "this kista is older than the bundle's writer; upgrade this kista")
	has("the import of a higher format", admin(1, "bundle", "import", "acme", v3), "kista-bundle/v3", "this kista is older than the bundle's writer; upgrade this kista")

	// the files
	out.Reset()
	errb.Reset()
	// a source left out is inspect's problem to report (exit 1): an import takes the rest
	if code := run([]string{"bundle", "inspect", signed, "-plan", plan}, &out, &errb); code != 1 {
		t.Errorf("inspect of a bundle with a source left out: exit %d\n%s%s", code, out.String(), errb.String())
	}
	has("inspect", out.String()+errb.String(), "package type adbc_driver, not known here", "quarantined",
		"the licence of x is in a format newer than this kista reads")
	if strings.Contains(out.String()+errb.String(), "read through") && strings.Contains(out.String()+errb.String(), "over ") {
		t.Errorf("inspect capped the left-out source's entries:\n%s%s", out.String(), errb.String())
	}
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

	// the statements: inspect reads the bundle whole, counts what would wait, and asks for no
	// document of a statement it cannot read
	out.Reset()
	errb.Reset()
	if code := run([]string{"bundle", "inspect", fsigned}, &out, &errb); code != 0 {
		t.Errorf("inspect of the statements: exit %d\n%s%s", code, out.String(), errb.String())
	}
	has("inspect of the statements", out.String(), "statements 6", "2 statements are of a newer source than this kista reads")
	got = admin(0, "bundle", "import", "acme", fsigned)
	// the fixture's two documents and the newer source's with an extra member (content travels by hash);
	// the unknown kind and the newer format wait
	has("the feed's import", got, "feed:reviews", "received 3", "waiting 2")
	if strings.Contains(got, "signature") || strings.Contains(got, "held ") {
		t.Errorf("a statement of a newer source held its body:\n%s", got)
	}
	// one body: its row shows its last reason (the unknown kind came before the newer format)
	show := admin(0, "feed", "show", "acme", "reviews")
	has("feed show", show, "This kista is older than its source: documents wait for an upgrade (`newer`).", "status ok")
	if strings.Contains(show, "The exporting kista is older") {
		t.Errorf("feed show blames the exporter for what this kista cannot read:\n%s", show)
	}
	// the same binary writes its next plan from the cursor: nothing waits at a lower level than its own
	next := filepath.Join(dir, "next.json")
	admin(0, "bundle", "plan", "acme", "-o", next)
	var np bundle.Plan
	pd, _ := os.ReadFile(next)
	if err := bundle.Decode(pd, &np); err != nil || len(np.Feeds) != 1 || len(np.Feeds[0].Bodies) != 1 || np.Feeds[0].Bodies[0].BodyHash != body ||
		np.Feeds[0].Bodies[0].Seq != 6 {
		t.Errorf("the next plan does not ask the body from 6: %v\n%s", err, pd)
	}
}
