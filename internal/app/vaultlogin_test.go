package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi/vaulttest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultlogin"
)

// pgAdmin is the store suite's PostgreSQL (KISTA_TEST_POSTGRES, a superuser) for setting up a
// database and a role of the test's own.
type pgAdmin struct {
	dsn string
	st  *store.Store
}

func newPGAdmin(t *testing.T) *pgAdmin {
	t.Helper()
	dsn := os.Getenv("KISTA_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("KISTA_TEST_POSTGRES is not set")
	}
	st, err := store.OpenPostgres(context.Background(), dsn, store.PasswordLogin{Env: "KISTA_TEST_POSTGRES_PASSWORD"}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &pgAdmin{dsn: dsn, st: st}
}

func (a *pgAdmin) exec(t *testing.T, q string) {
	t.Helper()
	if err := store.ExecRaw(context.Background(), a.st, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// roleDB makes a login role with a password and a database it owns; it returns the role and the
// database's DSN for it (no password: the login gives it).
func (a *pgAdmin) roleDB(t *testing.T, password string) (string, string) {
	t.Helper()
	role := "kista_vl_" + vaulttest.Random()
	a.exec(t, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, password))
	a.exec(t, fmt.Sprintf("CREATE DATABASE %s OWNER %s", role, role))
	t.Cleanup(func() {
		ctx := context.Background()
		_ = store.ExecRaw(ctx, a.st, "DROP DATABASE IF EXISTS "+role+" WITH (FORCE)")
		_ = store.ExecRaw(ctx, a.st, "DROP ROLE IF EXISTS "+role)
	})
	u, _ := url.Parse(a.dsn)
	u.User, u.Path = url.User(role), "/"+role
	return role, u.String()
}

// dropConnections ends the role's open connections, so the pool's next use opens a new one.
func (a *pgAdmin) dropConnections(t *testing.T, role string) {
	t.Helper()
	a.exec(t, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '"+role+"'")
}

// eventually runs a query until it succeeds: a connection ended under the pool, or a password
// rejected once, fails one attempt.
func eventually(t *testing.T, st *store.Store) {
	t.Helper()
	var err error
	for range 5 {
		if err = store.ExecRaw(context.Background(), st, "SELECT 1"); err == nil {
			return
		}
	}
	t.Fatalf("no query succeeded: %v", err)
}

func vaultLoginConfig(t *testing.T, srv vaulttest.Server, dsn, tokenFile, secret string) config.Config {
	t.Helper()
	body := fmt.Sprintf(`profile: dev
store:
  kind: postgres
  dsn: %q
  max_open_conns: 2
  login:
    kind: vault
    vault:
      address: %q
      auth: {kind: token_file, token_file: %q}
      %s
`, dsn, srv.Address, tokenFile, secret)
	p := filepath.Join(t.TempDir(), "kista.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The database password from a KV v2 secret (spec 0027), on Vault and OpenBao in dev mode and the
// store suite's PostgreSQL: the store opens and migrates, a rotation reaches new connections.
func TestVaultLoginKV(t *testing.T) {
	servers := vaulttest.Servers(t)
	if len(servers) == 0 {
		t.Skip("KISTA_TEST_VAULT / KISTA_TEST_OPENBAO are not set")
	}
	pg := newPGAdmin(t)
	ctx := context.Background()
	for _, srv := range servers {
		t.Run(srv.Name, func(t *testing.T) {
			role, dsn := pg.roleDB(t, "first-Password-1")
			mount := "kv" + vaulttest.Random()
			srv.Root(t, http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
			t.Cleanup(func() { srv.Root(t, http.MethodDelete, "sys/mounts/"+mount, nil) })
			srv.Root(t, http.MethodPost, mount+"/data/kista/db", map[string]any{"data": map[string]string{"password": "first-Password-1"}})
			policy := "kista-db-" + mount
			srv.Policy(t, policy, fmt.Sprintf("path %q { capabilities = [\"read\"] }\n", mount+"/data/kista/db"))
			tok := srv.WriteToken(t, policy)

			cfg := vaultLoginConfig(t, srv, dsn, tok, "kv: {mount: "+mount+", path: kista/db, field: password}\n      cache: 1m")
			st, err := OpenStore(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			l, ok := st.Login.(*vaultlogin.Login)
			if !ok {
				t.Fatalf("login %T", st.Login)
			}
			// a rotation: the database and Vault change; the open connection keeps working
			srv.Root(t, http.MethodPost, mount+"/data/kista/db", map[string]any{"data": map[string]string{"password": "second-Password-2"}})
			pg.exec(t, "ALTER ROLE "+role+" PASSWORD 'second-Password-2'")
			eventually(t, st)
			// a new connection: the cached password is rejected once, then Vault is read again
			pg.dropConnections(t, role)
			eventually(t, st)
			if c := l.Counts(); c[vaultlogin.OutcomeRejected] < 1 || c[vaultlogin.OutcomeVault] < 2 {
				t.Fatalf("counts %v", c)
			}
			// a policy that does not cover the path: a fixed error naming the status, at start
			other := vaultLoginConfig(t, srv, dsn, tok, "kv: {mount: "+mount+", path: kista/other, field: password}")
			if _, err := OpenStore(ctx, other); err == nil || !strings.Contains(err.Error(), "store: vault login: status 403") ||
				strings.Contains(err.Error(), "second-Password-2") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The database password from a database secrets engine static role (spec 0027): Vault rotates it;
// open connections keep working, new ones take the new password. KISTA_TEST_VAULT_POSTGRES is the
// store suite's PostgreSQL as Vault and OpenBao reach it (host:port).
func TestVaultLoginStaticRole(t *testing.T) {
	servers := vaulttest.Servers(t)
	if len(servers) == 0 {
		t.Skip("KISTA_TEST_VAULT / KISTA_TEST_OPENBAO are not set")
	}
	hostPort := os.Getenv("KISTA_TEST_VAULT_POSTGRES")
	if hostPort == "" {
		t.Skip("KISTA_TEST_VAULT_POSTGRES (the PostgreSQL as Vault reaches it) is not set")
	}
	pg := newPGAdmin(t)
	ctx := context.Background()
	admin, _ := url.Parse(pg.dsn)
	for _, srv := range servers {
		t.Run(srv.Name, func(t *testing.T) {
			role, dsn := pg.roleDB(t, "initial-Password-0")
			mount := "db" + vaulttest.Random()
			srv.Root(t, http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "database"})
			t.Cleanup(func() { srv.Root(t, http.MethodDelete, "sys/mounts/"+mount, nil) })
			srv.Root(t, http.MethodPost, mount+"/config/pg", map[string]any{
				"plugin_name":    "postgresql-database-plugin",
				"connection_url": "postgresql://{{username}}:{{password}}@" + hostPort + "/postgres?sslmode=disable",
				"username":       admin.User.Username(), "password": os.Getenv("KISTA_TEST_POSTGRES_PASSWORD"),
				"allowed_roles": []string{"kista"}, "verify_connection": true,
			})
			srv.Root(t, http.MethodPost, mount+"/static-roles/kista", map[string]any{"db_name": "pg", "username": role, "rotation_period": "1h"})
			policy := "kista-db-" + mount
			srv.Policy(t, policy, fmt.Sprintf("path %q { capabilities = [\"read\"] }\n", mount+"/static-creds/kista"))
			tok := srv.WriteToken(t, policy)

			st, err := OpenStore(ctx, vaultLoginConfig(t, srv, dsn, tok, "static_role: {mount: "+mount+", name: kista}"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			l := st.Login.(*vaultlogin.Login)
			if e := l.Expiry(); e <= 0 || e > time.Hour {
				t.Fatalf("expiry %s", e)
			}
			// Vault rotates the password: open connections keep working
			srv.Root(t, http.MethodPost, mount+"/rotate-role/kista", nil)
			if err := store.ExecRaw(ctx, st, "SELECT 1"); err != nil {
				t.Fatalf("an open connection after the rotation: %v", err)
			}
			// new connections take the new password, after one rejection of the cached one
			pg.dropConnections(t, role)
			eventually(t, st)
			if c := l.Counts(); c[vaultlogin.OutcomeRejected] < 1 || c[vaultlogin.OutcomeVault] < 2 {
				t.Fatalf("counts %v", c)
			}
			// a static role for another user than the DSN's is refused
			other, _ := url.Parse(dsn)
			other.User = url.User("someone_else")
			if _, err := OpenStore(ctx, vaultLoginConfig(t, srv, other.String(), tok, "static_role: {mount: "+mount+", name: kista}")); err == nil ||
				!strings.Contains(err.Error(), "another user than the DSN's") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
