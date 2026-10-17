package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
)

// Spec 0012: kista admin license, against the vectors' test-only key in licenses.keys.
func TestAdminLicense(t *testing.T) {
	dir := t.TempDir()
	k, err := vectors.LoadKey("../../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := extfile.MarshalPublicKeyPEM(&k.PublicKey)
	keyFile := filepath.Join(dir, "lic.pem")
	if err := os.WriteFile(keyFile, []byte(pubPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	indented := "    " + strings.ReplaceAll(strings.TrimSpace(pubPEM), "\n", "\n      ")
	cfg := filepath.Join(dir, "kista.yaml")
	if err := os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"licenses:\n  keys:\n  - |\n  "+indented+"\n"), 0o600); err != nil {
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
	if got := run(0, "", "license", "keys"); strings.TrimSpace(got) != vectors.Kid(&k.PublicKey) {
		t.Fatalf("keys: %q", got)
	}
	lic := filepath.Join(dir, "tresor.lic")
	_ = os.WriteFile(lic, []byte(sign("pro")+"\n"), 0o600)
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
	if got := run(0, "", "license", "verify", lic, "-key", keyFile); !strings.Contains(got, `"plan": "pro"`) || !strings.Contains(got, "status: valid") {
		t.Fatalf("verify: %q", got)
	}
	run(1, "", "license", "verify", filepath.Join(dir, "nope"))
	run(0, "", "license", "remove", "acme", "tresor")
	run(1, "", "license", "show", "acme", "tresor")
}
