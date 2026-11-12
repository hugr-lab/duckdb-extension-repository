package app

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
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
	if _, err := ten.CreateTenant(ctx, admin, "acme", "", "", ""); err != nil {
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

// Spec 0044: the server issuers are the configuration's own: their verifier's client is not
// narrowed by egress.hosts, so it fetches from a host the list leaves out, while the tenants'
// client refuses the same URL.
func TestServerAuthEgressHosts(t *testing.T) {
	ctx := context.Background()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[]}`)
	}))
	defer idp.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	port := uint16(idp.Listener.Addr().(*net.TCPAddr).Port)
	cfg := hostsConfig(t, []string{"listed.example"}) // 127.0.0.1 is not on it
	cfg.Egress.CAFile = ca
	cfg.Serve.PublicURL = "https://kista.example"
	cfg.Auth.ServerIssuers = []config.ServerIssuer{{Name: "ops", URL: idp.URL, RequiredClaims: map[string]string{"tid": "t1"}}}
	cfg.Auth.ServerEgressAllow = []config.EgressAllow{{CIDR: "127.0.0.1/32", Ports: []uint16{port}}}
	srv, err := ServerAuth(ctx, cfg, st)
	if err != nil || srv == nil || len(srv.Issuers) != 1 {
		t.Fatalf("ServerAuth: %v %v", srv, err)
	}
	if b, _, err := srv.Verifier.Fetch.Get(ctx, idp.URL+"/jwks"); err != nil || string(b) != `{"keys":[]}` {
		t.Fatalf("the server issuers' fetch from a host off the list: %q %v", b, err)
	}
	eg, err := Egress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eg.Get(ctx, idp.URL+"/jwks"); !errors.Is(err, egress.ErrHostNotAllowed) {
		t.Fatalf("the tenants' client: %v", err)
	}
}
