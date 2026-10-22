package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0012: kista admin license, with the vectors' test-only key declared by a build of the name
// (spec 0021).
func TestAdminLicense(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	k, err := vectors.LoadKey("../../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := extfile.MarshalPublicKeyPEM(&k.PublicKey)
	keyFile := filepath.Join(dir, "lic.pem")
	if err := os.WriteFile(keyFile, []byte(pubPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "k.db")
	cfg := filepath.Join(dir, "kista.yaml")
	if err := os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+db+" } }\nsigners: { file_dir: "+keys+" }\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	run := func(want int, in string, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		old := stdin
		stdin = strings.NewReader(in)
		defer func() { stdin = old }()
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, errw.String())
		}
		return out.String()
	}
	now := time.Now().Unix()
	sign := func(plan string) string {
		return vectors.Sign(k, vectors.Header(k), fmt.Sprintf(
			`{"v":1,"iss":"https://issuer.example","jti":"lic-%s","sub":"acme","ext":"tresor","plan":%q,"limits":{"sources":5},"iat":%d,"exp":%d}`,
			plan, plan, now-60, now+86400))
	}
	run(0, "", "migrate")
	run(0, "", "tenant", "create", "acme")
	run(0, "", "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", coreFile(t, "json"))
	run(0, "", "channel", "create", "acme/prod", "-kind", "signed")
	run(0, "", "channel", "versions", "acme/prod", "-add", "v2.0.0")
	run(0, "", "key", "add", "acme/prod", "-signer", "file:a.pem", "-active")
	file := filepath.Join(dir, "tresor.duckdb_extension")
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(bytes.Repeat([]byte{1}, 5000), extfile.MetadataPrefix...), block[:]...)
	if err := os.WriteFile(file, append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	run(0, "", "release", "add", "acme/prod", file, "-name", "tresor", "-unchecked")
	lic := filepath.Join(dir, "tresor.lic")
	_ = os.WriteFile(lic, []byte(sign("pro")+"\n"), 0o600)
	run(0, "", "publisher", "add", "acme", "ci")
	run(0, "", "publisher", "license-keys", "acme", "ci", "on")
	if got := run(0, "", "publisher", "list", "acme"); !strings.Contains(got, "LICENSE KEYS") || len(strings.Fields(strings.Split(got, "\n")[1])) < 2 || strings.Fields(strings.Split(got, "\n")[1])[1] != "yes" {
		t.Fatalf("publisher list: %q", got)
	}
	run(2, "", "publisher", "license-keys", "acme", "ci", "maybe")
	// a marked publisher's keys are the server administrator's: the CLI is one (spec 0021, 0023)
	keyID := strings.TrimSpace(run(0, publisherKeyLine, "publisher", "key", "add", "acme", "ci", "-"))
	run(0, "", "publisher", "license-keys", "acme", "ci", "off")
	run(1, "", "publisher", "license-keys", "acme", "ci", "on") // not while it holds a key
	run(0, "", "publisher", "key", "remove", "acme", "ci", keyID)
	run(1, "", "license", "import", "acme", lic) // no live build of tresor declares a key
	if got := run(0, "", "license", "keys", "acme"); strings.Contains(got, "tresor") {
		t.Fatalf("keys before any were recorded: %q", got)
	}
	// a trusted intake of a body declaring the key, as a marked publisher's publication records it
	st, err := store.OpenSQLite(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	tn, err := st.GetTenant(context.Background(), "acme")
	if err == nil {
		_, err = storetest.LicenseKey(context.Background(), st, tn.ID, "tresor", store.KeySourcePublisher, "ci", &k.PublicKey)
	}
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := run(0, "", "license", "keys", "acme", "tresor"); !strings.Contains(got, vectors.Kid(&k.PublicKey)) ||
		!strings.Contains(got, "publisher:ci") {
		t.Fatalf("keys: %q", got)
	}
	run(0, "", "license", "import", "acme", lic)
	run(1, "", "license", "import", "acme", "-", "-replace") // nothing on stdin
	run(1, sign("max"), "license", "import", "acme", "-")    // a licence is stored: -replace replaces it
	run(0, sign("max"), "license", "import", "acme", "-", "-replace")
	if got := run(0, "", "license", "list", "acme"); !strings.Contains(got, "tresor") || !strings.Contains(got, "max") {
		t.Fatalf("list: %q", got)
	}
	if got := run(0, "", "license", "show", "acme", "tresor"); !strings.Contains(got, "status: valid") || !strings.Contains(got, "limit: sources = 5") {
		t.Fatalf("show: %q", got)
	}
	if got := run(0, "", "license", "token", "acme", "tresor"); strings.TrimSpace(got) != sign("max") {
		t.Fatalf("token: %q", got)
	}
	run(0, "", "license", "public", "acme", "tresor")

	// spec 0028: the expected holder, set and cleared by the CLI, and enforced on its import
	stderrOf := func(want int, in string, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		old := stdin
		stdin = strings.NewReader(in)
		defer func() { stdin = old }()
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, errw.String())
		}
		return errw.String()
	}
	if got := stderrOf(1, "", "tenant", "holder", "acme", "org-1"); !strings.Contains(got, `tresor (lic-max, holder "acme")`) {
		t.Fatalf("the holder over another holder's licence: %q", got)
	}
	run(2, "", "tenant", "holder", "acme")
	run(2, "", "tenant", "holder", "acme", "acme", "-clear")
	// an empty id (an unset variable) is refused, never a clear; -clear with none set changes nothing
	if got := stderrOf(1, "", "tenant", "holder", "acme", ""); !strings.Contains(got, "-clear clears it") {
		t.Fatalf("an empty holder id: %q", got)
	}
	run(0, "", "tenant", "holder", "acme", "-clear")
	if got := stderrOf(1, "", "tenant", "holder", "nope", "org-1"); !strings.Contains(got, "not found") {
		t.Fatalf("a missing tenant: %q", got)
	}
	run(0, "", "tenant", "holder", "acme", "acme")
	if got := run(0, "", "tenant", "show", "acme"); !strings.Contains(got, "holder id:") || !strings.Contains(got, " acme\n") {
		t.Fatalf("show with a holder: %q", got)
	}
	other := vectors.Sign(k, vectors.Header(k), fmt.Sprintf(
		`{"v":1,"iss":"https://issuer.example","jti":"lic-other","sub":"org-2","ext":"tresor","plan":"other","iat":%d,"exp":%d}`, now-60, now+86400))
	if got := stderrOf(1, other, "license", "import", "acme", "-", "-replace"); !strings.Contains(got, "holder_mismatch") {
		t.Fatalf("another holder's import: %q", got)
	}
	run(0, "", "tenant", "holder", "acme", "-clear")
	if got := run(0, "", "tenant", "show", "acme"); strings.Contains(got, "holder id") {
		t.Fatalf("show after the clear: %q", got)
	}
	run(0, "", "tenant", "create", "beta", "-holder-id", "org-b")
	if got := run(0, "", "tenant", "show", "beta"); !strings.Contains(got, "org-b") {
		t.Fatalf("show beta: %q", got)
	}
	run(1, "", "tenant", "create", "gamma", "-holder-id", strings.Repeat("a", 129))

	if got := run(0, "", "license", "verify", lic, "-key", keyFile); !strings.Contains(got, `"plan": "pro"`) || !strings.Contains(got, "status: valid") {
		t.Fatalf("verify: %q", got)
	}
	run(1, "", "license", "verify", filepath.Join(dir, "nope"), "-key", keyFile)
	run(1, "", "license", "verify", lic) // no -key
	run(0, "", "license", "remove", "acme", "tresor")
	run(1, "", "license", "show", "acme", "tresor")
}
