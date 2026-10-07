package e2e

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/vault"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi/vaulttest"
)

// Spec 0004: a channel whose active key is a Vault / OpenBao Transit key. DuckDB reads the
// channel's .well-known and installs and loads an extension signed through Transit.
func TestVaultSignedChannel(t *testing.T) {
	b := needBuild(t)
	servers := vaulttest.Servers(t)
	if len(servers) == 0 {
		t.Skip("KISTA_TEST_VAULT / KISTA_TEST_OPENBAO are not set")
	}
	for _, srv := range servers {
		t.Run(srv.Name, func(t *testing.T) {
			ctx := context.Background()
			admin := authz.Actor{Kind: authz.ActorOS, ID: "e2e"}
			tr := srv.NewTransit(t)

			api, err := vaultapi.New(vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthTokenFile,
				TokenFile: tr.TokenFile, AllowHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			reg := keysource.NewRegistry(signer.Resolver{})
			if err := reg.Add("bao", vault.New(api, tr.Mount), keysource.Options{Allow: []string{"ext-"}}); err != nil {
				t.Fatal(err)
			}
			st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}}
			ks := &keys.Service{Store: st, Signers: reg, Authz: authz.ServerAdmin{}}
			if _, err := ten.CreateTenant(ctx, admin, "acme", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := ten.CreateChannel(ctx, admin, "acme", "prod", store.ChannelSigned); err != nil {
				t.Fatal(err)
			}
			k, err := ks.Add(ctx, admin, "acme", "prod", "bao:"+tr.Key+":v1", true)
			if err != nil {
				t.Fatal(err)
			}
			sg, err := ks.OpenSigner(ctx, k)
			if err != nil {
				t.Fatal(err)
			}
			r := newRepo(t, b)
			r.add("loadable_extension_demo", keyFrom(t, sg), "")
			doc, _, err := ks.WellKnown(ctx, admin, "acme", "prod")
			if err != nil {
				t.Fatal(err)
			}
			r.write(".well-known/duckdb-extension-repo.json", doc)
			res := newSession(t, b, nil).exec(
				createRepo("r", r.dir),
				"INSTALL loadable_extension_demo FROM r",
				"LOAD loadable_extension_demo FROM r",
				"SELECT test_alias_hello()",
				"SELECT unnest(key_fingerprints) FROM duckdb_extension_repositories() WHERE repository_name = 'r'",
			)
			mustOK(t, res)
			if got := res[4].Rows[0][0]; got != k.Fingerprint {
				t.Fatalf("DuckDB pinned %v, the store has %s", got, k.Fingerprint)
			}
		})
	}
}
