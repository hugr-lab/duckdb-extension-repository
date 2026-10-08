package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// kista serve refuses to start when a tenant holds a server audience (assigned before the server
// audience was configured).
func TestServerAuthAudienceConflict(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(string) bool { return true }}
	if _, err := ten.CreateTenant(ctx, admin, "acme", "", ""); err != nil {
		t.Fatal(err)
	}
	adm := &tenants.AuthAdmin{Store: st, Authz: authz.ServerAdmin{}, PublicURL: "https://kista.example"}
	if err := adm.AddAudience(ctx, admin, "acme", "api://kista"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Serve.PublicURL = "https://kista.example"
	cfg.Auth.ServerIssuers = []config.ServerIssuer{{Name: "ops", URL: "https://login.example", RequiredClaims: map[string]string{"tid": "t1"}}}
	cfg.Auth.ServerAudiences = []string{"api://other"}
	srv, err := ServerAuth(ctx, cfg, st)
	if err != nil || srv == nil || len(srv.Issuers) != 1 {
		t.Fatalf("no conflict: %v %v", srv, err)
	}
	cfg.Auth.ServerAudiences = []string{"api://kista"}
	if _, err := ServerAuth(ctx, cfg, st); err == nil || !strings.Contains(err.Error(), "api://kista") {
		t.Fatalf("conflict: %v", err)
	}
	// without server issuers there is no server identity
	cfg.Auth.ServerIssuers = nil
	if srv, err := ServerAuth(ctx, cfg, st); err != nil || srv != nil {
		t.Fatalf("no issuers: %v %v", srv, err)
	}
}
