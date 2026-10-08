// Package storetest opens test stores on every available engine: SQLite always, PostgreSQL and SQL
// Server when KISTA_TEST_POSTGRES / KISTA_TEST_SQLSERVER name a server (a DSN without a password;
// the password comes from KISTA_TEST_POSTGRES_PASSWORD / KISTA_TEST_SQLSERVER_PASSWORD). With
// KISTA_TEST_REQUIRE_DBS=1 a missing server fails instead of skipping.
//
// Each package run creates one database per server and drops it at exit (Cleanup); tests empty the
// tables between them through Reset.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Engine is one available engine. Tests using it must not call t.Parallel: they share one database
// per package run and empty it through Reset.
type Engine struct {
	Name string
	// Open opens a migrated, empty store; the test's cleanup closes it.
	Open func(t testing.TB) *store.Store
	// Fresh opens n stores (separate connection pools, like replicas) on one brand-new, unmigrated
	// database, for migration tests.
	Fresh func(t testing.TB, n int) []*store.Store
}

type server struct {
	once   sync.Once
	err    error
	dsn    string // the per-run database
	admin  string // the server's admin database DSN
	pwEnv  string
	open   func(ctx context.Context, dsn string, login store.Login) (*store.Store, error)
	create func(name string) string
	drop   func(name string) string
	dbs    []string
	mu     sync.Mutex
}

var (
	pg = &server{
		pwEnv: "KISTA_TEST_POSTGRES_PASSWORD",
		open: func(ctx context.Context, dsn string, l store.Login) (*store.Store, error) {
			return store.OpenPostgres(ctx, dsn, l, 4)
		},
		create: func(n string) string { return "CREATE DATABASE " + n },
		drop:   func(n string) string { return "DROP DATABASE IF EXISTS " + n + " WITH (FORCE)" },
	}
	ms = &server{
		pwEnv: "KISTA_TEST_SQLSERVER_PASSWORD",
		open: func(ctx context.Context, dsn string, l store.Login) (*store.Store, error) {
			return store.OpenSQLServer(ctx, dsn, l, 4)
		},
		create: func(n string) string { return "CREATE DATABASE " + n },
		drop: func(n string) string {
			return "IF DB_ID('" + n + "') IS NOT NULL BEGIN ALTER DATABASE " + n +
				" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + n + " END"
		},
	}
)

func randomName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "kista_test_" + hex.EncodeToString(b)
}

// withDB returns the DSN with the database replaced.
func withDB(kind, dsn, db string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	if kind == "postgres" {
		u.Path = "/" + db
	} else {
		q := u.Query()
		q.Set("database", db)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (s *server) login() store.Login { return store.PasswordLogin{Env: s.pwEnv} }

// adminDB opens the server's admin database with database/sql directly.
func (s *server) exec(kind, q string) error {
	st, err := s.open(context.Background(), s.admin, s.login())
	if err != nil {
		return err
	}
	defer st.Close()
	return store.ExecRaw(context.Background(), st, q)
}

// newDB creates a database and returns its DSN.
func (s *server) newDB(kind string) (string, error) {
	name := randomName()
	if err := s.exec(kind, s.create(name)); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.dbs = append(s.dbs, name)
	s.mu.Unlock()
	return withDB(kind, s.admin, name), nil
}

func (s *server) engine(t testing.TB, kind, env string) (Engine, bool) {
	dsn := os.Getenv(env)
	if dsn == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_DBS") == "1" {
			t.Fatalf("%s is not set and KISTA_TEST_REQUIRE_DBS=1", env)
		}
		return Engine{}, false
	}
	s.once.Do(func() {
		s.admin = dsn
		s.dsn, s.err = s.newDB(kind)
		if s.err != nil {
			return
		}
		st, err := s.open(context.Background(), s.dsn, s.login())
		if err != nil {
			s.err = err
			return
		}
		defer st.Close()
		s.err = st.Migrate(context.Background())
	})
	if s.err != nil {
		t.Fatalf("%s: %v", kind, s.err)
	}
	open := func(t testing.TB) *store.Store {
		t.Helper()
		st, err := s.open(context.Background(), s.dsn, s.login())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := Reset(context.Background(), st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	fresh := func(t testing.TB, n int) []*store.Store {
		t.Helper()
		dsn, err := s.newDB(kind)
		if err != nil {
			t.Fatal(err)
		}
		var out []*store.Store
		for i := 0; i < n; i++ {
			st, err := s.open(context.Background(), dsn, s.login())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			out = append(out, st)
		}
		return out
	}
	return Engine{Name: kind, Open: open, Fresh: fresh}, true
}

// Engines returns every available engine. SQLite is always there.
func Engines(t testing.TB) []Engine {
	t.Helper()
	sqliteOpen := func(t testing.TB, path string) *store.Store {
		t.Helper()
		st, err := store.OpenSQLite(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	out := []Engine{{
		Name: "sqlite",
		Open: func(t testing.TB) *store.Store {
			st := sqliteOpen(t, filepath.Join(t.TempDir(), "kista.db"))
			if err := st.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			return st
		},
		Fresh: func(t testing.TB, n int) []*store.Store {
			path := filepath.Join(t.TempDir(), "kista.db")
			var out []*store.Store
			for i := 0; i < n; i++ {
				out = append(out, sqliteOpen(t, path))
			}
			return out
		},
	}}
	if e, ok := pg.engine(t, "postgres", "KISTA_TEST_POSTGRES"); ok {
		out = append(out, e)
	}
	if e, ok := ms.engine(t, "sqlserver", "KISTA_TEST_SQLSERVER"); ok {
		out = append(out, e)
	}
	return out
}

// Cleanup drops the databases created by this package run; call it from TestMain.
func Cleanup() {
	for _, s := range []*server{pg, ms} {
		for _, db := range s.dbs {
			kind := "postgres"
			if s == ms {
				kind = "sqlserver"
			}
			if err := s.exec(kind, s.drop(db)); err != nil {
				fmt.Fprintf(os.Stderr, "storetest: dropping %s: %v\n", db, err)
			}
		}
	}
}

// Reset empties every table, children first.
func Reset(ctx context.Context, st *store.Store) error {
	if err := store.ExecRaw(ctx, st, "UPDATE channels SET serving_key_id = NULL"); err != nil {
		return err
	}
	for _, table := range []string{"release_signatures", "releases", "builds", "key_events", "channel_keys",
		"key_fingerprints", "channel_duckdb_versions", "duckdb_version_c_apis", "channels", "duckdb_versions",
		"tenants", "blobs", "storage_domains", "deployment", "leases"} {
		if err := store.ExecRaw(ctx, st, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return nil
}
