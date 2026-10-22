package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
)

// TestAdminBundleImport drives a bundle import from a file, standard input and the blob store's
// imports/ prefix, and the import records and reset, on SQLite.
func TestAdminBundleImport(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	bundleKey := filepath.Join(dir, "bundle-key.pem")
	if code := run([]string{"bundle", "keygen", "-o", bundleKey}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	fp := strings.TrimSpace(out.String())
	// the bundle's plan is dated after the upstream is created: an older plan is stale
	plan, unsigned := bundleFixture(t, dir, fp, func(m *bundle.Manifest) {
		m.Plan.CreatedAt = m.Plan.CreatedAt.Add(time.Minute)
		m.CreatedAt = m.CreatedAt.Add(time.Minute)
	})
	signed := filepath.Join(dir, "acme.kbundle")
	if code := run([]string{"bundle", "sign", "-key", bundleKey, "-plan", plan, "-in", unsigned, "-o", signed}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	var p bundle.Plan
	data, _ := os.ReadFile(plan)
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"rotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: "+keys+" }\n"+
		"serve: { public_url: 'https://kista.example' }\n"+
		"upstreams: { bundle_keys: [{ fingerprint: '"+fp+"', tenants: [acme] }] }\n"), 0o600)
	admin := func(want int, stdin []byte, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		cmd := append([]string{"-config", cfg}, args...)
		var code int
		if stdin != nil {
			code = adminWithInput(cmd, bytes.NewReader(stdin), &out, &errw)
		} else {
			code = admin(cmd, nil, &out, &errw)
		}
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	admin(0, nil, "migrate")
	admin(0, nil, "tenant", "create", "acme")
	admin(0, nil, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", coreFile(t, "json"))
	admin(0, nil, "channel", "create", "acme/prod", "-kind", "signed")
	admin(0, nil, "channel", "versions", "acme/prod", "-add", "v2.0.0")
	admin(0, nil, "key", "add", "acme/prod", "-signer", "file:a.pem", "-active")
	admin(0, nil, "upstream", "add", "acme", "air", "-kind", "repository", "-mode", "bundle", "-channel", "prod",
		"-platforms", "linux_amd64", "-extensions", "x", "-prefix", p.Sources[0].Prefix, "-keys", p.Sources[0].Keys[0])

	got := admin(0, nil, "bundle", "import", "acme", signed, "-dry-run")
	if !strings.Contains(got, "(dry run)") || !strings.Contains(got, "would_release 1") {
		t.Fatalf("dry run: %s", got)
	}
	got = admin(0, nil, "bundle", "import", "acme", signed)
	in, _ := os.ReadFile(signed)
	inputSHA := "input sha256 " + sha(in)
	if !strings.Contains(got, "air") || !strings.Contains(got, "released 1") || !strings.Contains(got, inputSHA) {
		t.Fatalf("import: %s", got)
	}
	if got := admin(0, in, "bundle", "import", "acme", "-"); !strings.Contains(got, "unchanged 1") || !strings.Contains(got, inputSHA) {
		t.Fatalf("from standard input: %s", got)
	}
	// the operator's upload under the domain's imports/ prefix; kista reads it and never removes it
	up := filepath.Join(dir, "blobs", "imports", "acme-1.kbundle")
	if err := os.MkdirAll(filepath.Dir(up), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(up, in, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := admin(0, nil, "bundle", "import", "acme", "blob:default/imports/acme-1.kbundle"); !strings.Contains(got, "unchanged 1") ||
		!strings.Contains(got, inputSHA) {
		t.Fatalf("from the blob store: %s", got)
	}
	if _, err := os.Stat(up); err != nil {
		t.Errorf("the upload is gone: %v", err)
	}
	admin(1, nil, "bundle", "import", "acme", "blob:default/imports/../k.db")
	admin(2, nil, "bundle", "import", "acme", "blob:default/streams/x") // not under imports/
	admin(1, nil, "bundle", "import", "other", signed)                  // no such tenant

	list := admin(0, nil, "bundle", "list", "acme")
	if strings.Count(list, "done") != 4 || !strings.Contains(list, "true") {
		t.Fatalf("list: %s", list)
	}
	id := strings.Fields(strings.Split(list, "\n")[1])[0]
	if got := admin(0, nil, "bundle", "show", "acme", id); !strings.Contains(got, fp) || !strings.Contains(got, `"status": "imported"`) {
		t.Fatalf("show: %s", got)
	}
	if got := admin(0, nil, "upstream", "show", "acme", "air"); !strings.Contains(got, "0192f0c4-1b2a-7c3d-8e4f-0123456789ab") {
		t.Errorf("upstream show: %s", got)
	}
	admin(0, nil, "bundle", "reset", "acme", "air")
	admin(1, nil, "bundle", "reset", "acme", "nope")
	admin(2, nil, "bundle", "import", "acme")
}

func adminWithInput(args []string, in io.Reader, stdout, stderr io.Writer) int {
	saved := stdin
	stdin = in
	defer func() { stdin = saved }()
	return admin(args, nil, stdout, stderr)
}

// TestAdminBundleExport exports a bundle from an own channel for a site's plan: signed as it is
// written, or unsigned and signed offline, then imported, on SQLite.
func TestAdminBundleExport(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.pem", "b.pem"} {
		if err := signer.GenerateKeyFile(filepath.Join(keys, k)); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	bundleKey := filepath.Join(dir, "bundle-key.pem")
	if code := run([]string{"bundle", "keygen", "-o", bundleKey}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	fp := strings.TrimSpace(out.String())
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"rotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: "+keys+" }\n"+
		"serve: { public_url: 'https://kista.example' }\n"+
		"upstreams: { bundle_keys: [{ fingerprint: '"+fp+"', tenants: [site] }] }\n"), 0o600)
	admin := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	admin(0, "migrate")
	admin(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", coreFile(t, "json"))
	for i, tn := range []string{"acme", "site"} {
		admin(0, "tenant", "create", tn)
		admin(0, "channel", "create", tn+"/prod", "-kind", "signed")
		admin(0, "channel", "versions", tn+"/prod", "-add", "v2.0.0")
		admin(0, "key", "add", tn+"/prod", "-signer", "file:"+[]string{"a.pem", "b.pem"}[i], "-active")
	}
	// acme publishes x in its own channel; the site plans that channel as a repository source
	block, _ := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	body := append(append(symtest.ELF("x_duckdb_cpp_init"), extfile.MetadataPrefix...), block[:]...)
	ext := filepath.Join(dir, "x.duckdb_extension")
	os.WriteFile(ext, append(body, make([]byte, extfile.SignatureSize)...), 0o600)
	admin(0, "release", "add", "acme/prod", ext, "-name", "x")
	prodFP := ""
	for _, f := range strings.Fields(admin(0, "key", "list", "acme/prod")) {
		if strings.HasPrefix(f, "sha256:") {
			prodFP = f
		}
	}
	admin(0, "upstream", "add", "site", "own", "-kind", "repository", "-mode", "bundle", "-channel", "prod",
		"-platforms", "linux_amd64", "-extensions", "x", "-prefix", "https://kista.example/acme/prod", "-keys", prodFP)
	plan := filepath.Join(dir, "plan.json")
	admin(0, "bundle", "plan", "site", "-o", plan)

	// unsigned, then signed offline: the site imports it
	unsigned, signed := filepath.Join(dir, "b.unsigned"), filepath.Join(dir, "b.kbundle")
	got := admin(0, "bundle", "export", "acme", "-plan", plan, "-unsigned", "-o", unsigned)
	if !strings.Contains(got, "unsigned") || !strings.Contains(got, "fetched 1") || !strings.Contains(got, "lists key "+prodFP) {
		t.Fatalf("export -unsigned: %s", got)
	}
	out.Reset()
	if code := run([]string{"bundle", "sign", "-key", bundleKey, "-plan", plan, "-in", unsigned, "-o", signed}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if got := admin(0, "bundle", "import", "site", signed); !strings.Contains(got, "released 1") {
		t.Fatalf("import: %s", got)
	}
	// signed as it is written: the same file, already held
	direct := filepath.Join(dir, "c.kbundle")
	got = admin(0, "bundle", "export", "acme", "-plan", plan, "-key", bundleKey, "-o", direct)
	data, _ := os.ReadFile(direct)
	if !strings.Contains(got, "signed with "+fp) || !strings.Contains(got, "sha256 "+sha(data)) {
		t.Fatalf("export -key: %s", got)
	}
	if got := admin(0, "bundle", "import", "site", direct); !strings.Contains(got, "unchanged 1") {
		t.Fatalf("import: %s", got)
	}
	// refusals leave no output
	other := filepath.Join(dir, "other-key.pem")
	if code := run([]string{"bundle", "keygen", "-o", other}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	admin(1, "bundle", "export", "acme", "-plan", plan, "-key", other, "-o", filepath.Join(dir, "d.kbundle"))
	admin(1, "bundle", "export", "acme", "-plan", plan, "-key", bundleKey, "-o", direct) // exists
	admin(2, "bundle", "export", "acme", "-plan", plan, "-o", filepath.Join(dir, "d.kbundle"))
	admin(2, "bundle", "export", "acme", "-plan", plan, "-key", bundleKey, "-unsigned", "-o", filepath.Join(dir, "d.kbundle"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "d.kbundle" || strings.HasPrefix(e.Name(), ".kista-bundle-") {
			t.Errorf("a refused export left %s", e.Name())
		}
	}
	// spec 0021: the plan asks for licences, and the exporter agrees, or is told it did not
	admin(2, "bundle", "plan", "site", "-license-holder", "acme-corp", "-o", filepath.Join(dir, "x.json"))
	lplan := filepath.Join(dir, "lplan.json")
	admin(0, "bundle", "plan", "site", "-licenses", "-license-holder", "acme-corp", "-o", lplan)
	if pd, _ := os.ReadFile(lplan); !strings.Contains(string(pd), `"licenses": true`) || !strings.Contains(string(pd), `"license_holder": "acme-corp"`) {
		t.Fatalf("the plan: %s", pd)
	}
	if got := admin(0, "bundle", "export", "acme", "-plan", lplan, "-unsigned", "-o", filepath.Join(dir, "l1.unsigned")); !strings.Contains(got, "export -licenses carries them") {
		t.Fatalf("export without -licenses: %s", got)
	}
	if got := admin(0, "bundle", "export", "acme", "-plan", plan, "-unsigned", "-licenses", "-o", filepath.Join(dir, "l2.unsigned")); !strings.Contains(got, "the plan asks for no licences") {
		t.Fatalf("export -licenses of a plan without: %s", got)
	}
	lsigned := filepath.Join(dir, "l3.kbundle")
	// acme holds no licence: the bundle carries none, and says why
	if got := admin(0, "bundle", "export", "acme", "-plan", lplan, "-key", bundleKey, "-licenses", "-o", lsigned); !strings.Contains(got,
		`no licence in force for the bundle's names of holder "acme-corp"`) {
		t.Fatalf("export -licenses: %s", got)
	}
	admin(2, "bundle", "export", "acme", "-plan", lplan, "-key", bundleKey, "-license-holder", "x", "-o", filepath.Join(dir, "l4.kbundle"))
	admin(2, "bundle", "import", "site", lsigned, "-licenses")
	out.Reset()
	if code := run([]string{"bundle", "inspect", lsigned, "-plan", lplan}, &out, &errb); code != 0 || !strings.Contains(out.String(), `the plan asks for licences, of holder "acme-corp"`) {
		t.Fatalf("inspect: %d %s", code, out.String())
	}

	// a source mirrored nowhere fails the export, which still writes the rest
	var p bundle.Plan
	pd, _ := os.ReadFile(plan)
	if err := json.Unmarshal(pd, &p); err != nil {
		t.Fatal(err)
	}
	p.Sources = append(p.Sources, bundle.PlanSource{Kind: "repository", Prefix: "https://nowhere.example/x", Keys: []string{prodFP},
		Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "x", Versions: []string{}}}})
	pd, _ = json.Marshal(p)
	os.WriteFile(plan, pd, 0o644)
	if got := admin(1, "bundle", "export", "acme", "-plan", plan, "-key", bundleKey, "-o", filepath.Join(dir, "e.kbundle")); !strings.Contains(got, "not mirrored here") {
		t.Fatalf("a failed source: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "e.kbundle")); err != nil {
		t.Errorf("the bundle of the other sources: %v", err)
	}
}

// Spec 0021: signing offline checks the bundle's licence request against the plan, and each
// licence's extension and holder.
func TestWithinPlanLicenses(t *testing.T) {
	key, err := vectors.LoadKey("../../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	tok := func(ext, sub string) string {
		return vectors.Sign(key, vectors.Header(key), `{"v":1,"iss":"https://issuer.example","jti":"l","sub":"`+sub+`","ext":"`+ext+
			`","plan":"pro","iat":1,"exp":4102444800}`)
	}
	plan := &bundle.Plan{Licenses: true, LicenseHolder: "acme"}
	m := &bundle.Manifest{Plan: bundle.PlanRef{Licenses: true, LicenseHolder: "acme"}, Sources: []bundle.Source{{Kind: "repository",
		Prefix: "https://src.example/acme/prod", Licenses: []bundle.License{{Name: "x", Token: tok("x", "acme")}}}}}
	plan.Sources = []bundle.PlanSource{{Kind: "repository", Prefix: "https://src.example/acme/prod"}}
	if err := withinPlan(m, plan); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(){
		"another request": func() { m.Plan.LicenseHolder = "" },
		"another holder":  func() { m.Sources[0].Licenses[0].Token = tok("x", "other") },
		"another name":    func() { m.Sources[0].Licenses[0].Token = tok("y", "acme") },
		"not a licence":   func() { m.Sources[0].Licenses[0].Token = "a.b.c" },
	} {
		m.Plan.LicenseHolder, m.Sources[0].Licenses[0].Token = "acme", tok("x", "acme")
		change()
		if err := withinPlan(m, plan); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
