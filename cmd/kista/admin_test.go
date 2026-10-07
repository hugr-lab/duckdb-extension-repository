package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if !strings.Contains(run(0, "tenant", "list"), "acme") {
		t.Fatal("tenant list")
	}
	run(0, "version", "add", "eb0d9df48e", "-kind", "dev")
	run(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.2.0")
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
	run(0, "backup", filepath.Join(dir, "backup.db"))
	run(1, "key", "add", "acme/prod", "-signer", "file:../escape.pem")
	run(2, "nonsense")
	run(2, "key", "add", "acme")
}
