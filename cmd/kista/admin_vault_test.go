package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi/vaulttest"
)

// kista admin key check through a configured Vault / OpenBao source: config → app → registry → vault.
func TestAdminKeyCheckVault(t *testing.T) {
	servers := vaulttest.Servers(t)
	if len(servers) == 0 {
		t.Skip("KISTA_TEST_VAULT / KISTA_TEST_OPENBAO are not set")
	}
	for _, srv := range servers {
		t.Run(srv.Name, func(t *testing.T) {
			tr := srv.NewTransit(t)
			dir := t.TempDir()
			cfg := filepath.Join(dir, "kista.yaml")
			os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
				"signers:\n  sources:\n    - name: bao\n      kind: vault\n      allow: [\"ext-\"]\n"+
				"      vault: { address: \""+srv.Address+"\", mount: "+tr.Mount+", auth: { kind: token_file, token_file: "+tr.TokenFile+" } }\n"), 0o600)
			var out, errw bytes.Buffer
			if code := admin([]string{"-config", cfg, "key", "check", "-signer", "bao:" + tr.Key + ":v1"}, nil, &out, &errw); code != 0 {
				t.Fatalf("exit %d: %s", code, errw.String())
			}
			if !strings.HasPrefix(out.String(), "bao:"+tr.Key+":v1 sha256:") {
				t.Fatalf("output %q", out.String())
			}
			out.Reset()
			if code := admin([]string{"-config", cfg, "key", "check", "-signer", "bao:other:v1"}, nil, &out, &errw); code != 1 {
				t.Fatalf("a key outside the allow list: exit %d", code)
			}
		})
	}
}
