package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestServerStart(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	last := func() (d struct {
		Version string            `json:"version"`
		Schema  int               `json:"schema"`
		Digests map[string]string `json:"digests"`
		Changed []string          `json:"changed"`
	}) {
		t.Helper()
		e, err := st.LastEvent(ctx, audit.ServerTenant, "server.start")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if err := ServerStart(ctx, cfg, st); err != nil {
		t.Fatal(err)
	}
	if d := last(); d.Version != Version || d.Schema == 0 || len(d.Digests) != 9 || len(d.Changed) != 0 {
		t.Fatalf("the first start: %+v", d)
	}
	cfg.Egress.Allow = []config.EgressAllow{{CIDR: "10.0.0.0/8"}}
	cfg.Serve.Resign = true
	if err := ServerStart(ctx, cfg, st); err != nil {
		t.Fatal(err)
	}
	if d := last(); !slices.Equal(d.Changed, []string{"egress", "serve"}) {
		t.Fatalf("changed: %+v", d)
	}
}

func TestHeadersFile(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "h")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	h, err := headersFile(write("# the collector's token\nauthorization: Bearer s3cret\n\nX-Scope-OrgID: acme\n"))
	if err != nil || h.Get("Authorization") != "Bearer s3cret" || h.Get("X-Scope-Orgid") != "acme" {
		t.Fatalf("headers: %v %v", h, err)
	}
	for _, bad := range []string{"no colon\n", "Content-Type: text/plain\n", "Bad Name: x\n"} {
		if _, err := headersFile(write(bad)); err == nil || strings.Contains(err.Error(), "text/plain") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := headersFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file")
	}
}
