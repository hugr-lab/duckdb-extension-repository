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
	"github.com/hugr-lab/duckdb-extension-repository/internal/telemetry"
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
	if d := last(); d.Version != Version || d.Schema == 0 || len(d.Digests) != 11 || len(d.Changed) != 0 {
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

// A sink is named in metrics only when metrics may name every tenant it takes.
func TestSinkNames(t *testing.T) {
	cfg := config.Default()
	cfg.Events.Sinks = []config.Sink{{Name: "acme-splunk", Tenants: []string{"acme"}}, {Name: "beta-siem", Tenants: []string{"beta"}},
		{Name: "all", Tenants: []string{"*"}}, {Name: "ops", Server: true}}
	counts := map[string]int64{"acme-splunk": 1, "beta-siem": 2, "all": 4, "ops": 8}
	got := sinkNames(cfg, telemetry.Tenants{Names: map[string]bool{"acme": true}}, counts)
	if len(got) != 3 || got["acme-splunk"] != 1 || got["ops"] != 8 || got["_other"] != 6 {
		t.Fatalf("named: %v", got)
	}
	if got := sinkNames(cfg, telemetry.Tenants{All: true}, counts); got["beta-siem"] != 2 || got["all"] != 4 {
		t.Fatalf("every tenant: %v", got)
	}
}
