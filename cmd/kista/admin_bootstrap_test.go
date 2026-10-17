package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Spec 0020 phase 4a: kista admin bootstrap plans, applies once, refuses a changed section until it
// is accepted, and re-arms.
func TestAdminBootstrap(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(idpKey.N.Bytes()), "e": "AQAB"}}})
	}))
	defer idp.Close()
	caFile := filepath.Join(dir, "idp-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	idpPort := idp.Listener.Addr().(*net.TCPAddr).Port
	cfg := filepath.Join(dir, "kista.yaml")
	write := func(display string) {
		t.Helper()
		body := "profile: dev\nstore: { kind: sqlite, sqlite: { path: " + filepath.Join(dir, "k.db") + " } }\n" +
			"signers: { file_dir: " + keys + " }\nserve: { public_url: 'https://kista.example' }\n" +
			"egress: { ca_file: " + caFile + ", allow: [{ cidr: 127.0.0.1/32, ports: [" + strconv.Itoa(idpPort) + "] }] }\n" +
			"bootstrap:\n" +
			"  versions: [ { version: v1.4.1, kind: release, c_apis: [v1.2.0] } ]\n" +
			"  tenant: { name: acme, display_name: " + display + " }\n" +
			"  channels: [ { name: prod, kind: signed, versions: [v1.4.1], key: 'file:a.pem' } ]\n" +
			"  issuer: { name: idp, url: '" + idp.URL + "', jwks_uri: '" + idp.URL + "/keys', required_claims: { tid: t1 } }\n" +
			"  grants: [ { principal: 'subject:idp|alice', verbs: [admin] } ]\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	write("Acme")
	run(0, "migrate")
	run(2, "bootstrap", "-accept", "-rearm")
	if got := run(0, "bootstrap", "-dry-run"); !strings.Contains(got, "create   tenant") || !strings.Contains(got, "create   key") {
		t.Fatalf("dry run: %s", got)
	}
	if got := run(0, "tenant", "list"); strings.Contains(got, "acme") {
		t.Fatalf("a dry run made the tenant: %s", got)
	}
	if got := run(0, "bootstrap"); !strings.Contains(got, "admin bootstrap acme by os:") {
		t.Fatalf("apply: %s", got)
	}
	if got := run(0, "tenant", "list"); !strings.Contains(got, "acme") {
		t.Fatalf("no tenant: %s", got)
	}
	if got := run(0, "bootstrap"); !strings.Contains(got, "applied already") {
		t.Fatalf("again: %s", got)
	}
	write("Acme Corp")
	if got := run(1, "bootstrap"); !strings.Contains(got, "changed after it was applied") {
		t.Fatalf("changed: %s", got)
	}
	run(0, "bootstrap", "-accept")
	run(0, "bootstrap")
	run(0, "bootstrap", "-rearm")
	// re-armed, the tenant differs from the section: refused
	if got := run(1, "bootstrap", "-adopt"); !strings.Contains(got, "refused") {
		t.Fatalf("re-armed: %s", got)
	}
}
