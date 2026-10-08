package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// TestAdminCLI drives kista admin end to end on SQLite.
func TestAdminCLI(t *testing.T) {
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
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"rotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: "+keys+" }\n"), 0o600)

	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, errw.String())
		}
		return out.String()
	}
	run(0, "migrate")
	run(0, "check")
	run(0, "tenant", "create", "acme", "-display-name", "Acme")
	run(1, "tenant", "create", "acme-cn", "-domain", "cn") // not configured
	if got := run(0, "tenant", "list"); !strings.Contains(got, "acme") || !strings.Contains(got, "default") || strings.Contains(got, "acme-cn") {
		t.Fatalf("tenant list: %q", got)
	}
	run(0, "version", "add", "eb0d9df48e", "-kind", "dev")
	run(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6")
	run(0, "version", "c-api", "v2.0.0", "-c-api", "v2.0.0")
	run(1, "version", "c-api", "v2.0.0", "-c-api", "v2.1.0") // a major is never changed
	if got := run(0, "version", "list"); !strings.Contains(got, "v1.5.6 v2.0.0") {
		t.Fatalf("version list: %q", got)
	}
	run(0, "channel", "create", "acme/prod", "-kind", "signed")
	if got := run(0, "channel", "versions", "acme/prod", "-add", "eb0d9df48e", "-add", "v2.0.0"); !strings.Contains(got, "v2.0.0") {
		t.Fatalf("versions %q", got)
	}
	a := strings.Fields(run(0, "key", "add", "acme/prod", "-signer", "file:a.pem", "-active"))
	b := strings.Fields(run(0, "key", "add", "acme/prod", "-signer", "file:b.pem"))
	var doc struct {
		SignatureKeys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal([]byte(run(0, "wellknown", "acme/prod")), &doc); err != nil || len(doc.SignatureKeys) != 2 {
		t.Fatalf("wellknown: %v %d", err, len(doc.SignatureKeys))
	}
	run(0, "key", "activate", "acme/prod", b[1])
	run(1, "key", "retire", "acme/prod", b[0]) // the active key
	run(0, "key", "retire", "acme/prod", a[0])
	if !strings.Contains(run(0, "key", "list", "acme/prod"), "retired") {
		t.Fatal("key list")
	}
	if n := strings.Count(run(0, "key", "events", "acme/prod"), "os:"); n != 5 {
		t.Fatalf("%d events", n)
	}
	if got := run(0, "key", "check", "-signer", "file:a.pem"); !strings.HasPrefix(got, "file:a.pem sha256:") {
		t.Fatalf("key check: %q", got)
	}
	run(1, "key", "check", "-signer", "nope:x")
	// releases: add from a file, list, change; key resign after an activation
	file := filepath.Join(dir, "tresor.duckdb_extension")
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(bytes.Repeat([]byte{1}, 5000), extfile.MetadataPrefix...), block[:]...)
	if err := os.WriteFile(file, append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	rid := strings.TrimSpace(run(0, "release", "add", "acme/prod", file, "-name", "tresor"))
	if again := strings.TrimSpace(run(0, "release", "add", "acme/prod", file, "-name", "tresor")); again != rid {
		t.Fatalf("re-add: %q vs %q", again, rid)
	}
	run(1, "release", "add", "acme/prod", file, "-name", "postgres") // an alias DuckDB never requests
	run(2, "release", "add", "acme/prod", file)                      // no -name
	if got := run(0, "release", "list", "acme/prod"); !strings.Contains(got, rid) || !strings.Contains(got, "public") {
		t.Fatalf("release list: %q", got)
	}
	run(0, "release", "private", "acme/prod", rid)
	run(0, "release", "current", "acme/prod", rid)
	run(0, "key", "resign", "acme/prod")
	run(0, "release", "yank", "acme/prod", rid)
	run(1, "release", "yank", "acme/prod", rid)
	run(0, "backup", filepath.Join(dir, "backup.db"))
	if got := run(0, "blob", "check"); !strings.Contains(got, "domain default ok") {
		t.Fatalf("blob check: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "kista-domain.json")); err != nil {
		t.Fatalf("blob check wrote no marker beside the dev database: %v", err)
	}
	run(1, "key", "add", "acme/prod", "-signer", "file:../escape.pem")
	run(2, "nonsense")
	run(2, "key", "add", "acme")
}
