package app

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

// Spec 0020 phase 4a: serve applies the bootstrap before its listeners bind; a refused bootstrap
// stops it with nothing bound.
func TestServeBootstrapBeforeBind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfgPath := filepath.Join(dir, "kista.yaml")
	body := "profile: dev\nstore: { kind: sqlite, sqlite: { path: " + filepath.Join(dir, "k.db") + " } }\n" +
		"serve: { public_url: 'http://" + addr + "', listeners: [ { addr: '" + addr + "', scheme: http } ] }\n" +
		"bootstrap: { tenant: { name: acme, display_name: Acme } }\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a tenant of that name made through the API, with another display name
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewServices(cfg, st, authz.ServerAdmin{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Tenants.CreateTenant(ctx, authz.OSActor(), "acme", "Other", ""); err != nil {
		t.Fatal(err)
	}
	st.Close()
	err = Serve(ctx, cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "bootstrap") || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("serve: %v", err)
	}
	l, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("serve bound before its bootstrap: %v", err)
	}
	l.Close()
}
