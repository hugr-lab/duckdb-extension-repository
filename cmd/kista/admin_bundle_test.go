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
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
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
	admin(0, nil, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6")
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
