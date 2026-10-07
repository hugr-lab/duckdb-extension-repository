package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

var ctx = context.Background()

// each runs f as a subtest on every available engine.
func each(t *testing.T, f func(t *testing.T, e storetest.Engine)) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) { f(t, e) })
	}
}

// fixture creates a tenant and a signed channel.
func fixture(t *testing.T, s *store.Store) (store.Tenant, store.Channel) {
	t.Helper()
	tn := store.Tenant{Name: "acme", DisplayName: "Acme"}
	ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
	err := s.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		return tx.CreateChannel(ctx, &ch)
	})
	if err != nil {
		t.Fatal(err)
	}
	return tn, ch
}

func addKey(t *testing.T, s *store.Store, ch store.Channel, fp, state string) store.Key {
	t.Helper()
	k := store.Key{TenantID: ch.TenantID, ChannelID: ch.ID, Fingerprint: fp, SignerRef: "file:" + fp,
		PublicKey: []byte{1, 2, 3}, State: state}
	if err := s.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
		return tx.InsertKey(ctx, &k, "os:1:test", false)
	}); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTenantsAndChannels(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)

		got, err := s.GetTenant(ctx, "acme")
		if err != nil || got.ID != tn.ID || got.State != store.TenantActive || got.DisplayName != "Acme" {
			t.Fatalf("GetTenant: %+v %v", got, err)
		}
		if !got.CreatedAt.Equal(tn.CreatedAt) {
			t.Fatalf("created_at round trip: %v vs %v", got.CreatedAt, tn.CreatedAt)
		}
		c, err := s.GetChannel(ctx, "acme", "prod")
		if err != nil || c.ID != ch.ID || c.Kind != store.ChannelSigned {
			t.Fatalf("GetChannel: %+v %v", c, err)
		}
		if _, err := s.GetChannel(ctx, "acme", "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing channel: %v", err)
		}

		dup := func(f func(tx *store.Tx) error) error { return s.InTx(ctx, "", f) }
		if err := dup(func(tx *store.Tx) error { return tx.CreateTenant(ctx, &store.Tenant{Name: "acme"}) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("duplicate tenant: %v", err)
		}
		if err := dup(func(tx *store.Tx) error {
			return tx.CreateChannel(ctx, &store.Channel{TenantID: tn.ID, Name: "prod", Kind: store.ChannelSigned})
		}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("duplicate channel: %v", err)
		}
		// names are byte-exact on every engine (SQL Server's default collation is case-insensitive)
		if err := dup(func(tx *store.Tx) error {
			return tx.CreateChannel(ctx, &store.Channel{TenantID: tn.ID, Name: "staging", Kind: store.ChannelSigned})
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.ExecRaw(ctx, s, "INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, ?, '', 'active', ?, 1)",
			store.NewID(), "ACME", timeArg(s, s.Now())); err != nil {
			t.Fatalf("upper-case name collided with lower-case (collation is not binary): %v", err)
		}
		// CHECK constraints
		if err := store.ExecRaw(ctx, s, "UPDATE tenants SET state = 'deleted' WHERE name = 'acme'"); err == nil ||
			!strings.Contains(strings.ToLower(err.Error()), "check") {
			t.Fatalf("CHECK on tenants.state: %v", err)
		}
		// lookups are byte-exact too
		if g, err := s.GetTenant(ctx, "ACME"); err != nil || g.ID == tn.ID {
			t.Fatalf("GetTenant(ACME) returned acme: %+v %v", g, err)
		}
		if err := store.ExecRaw(ctx, s, "INSERT INTO channels (id, tenant_id, name, kind, created_at, version) VALUES (?, ?, 'PROD', 'signed', ?, 1)",
			store.NewID(), tn.ID, timeArg(s, s.Now())); err != nil {
			t.Fatalf("channel PROD collided with prod: %v", err)
		}
		// suspend with compare-and-set
		err = s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetTenantState(ctx, &got, store.TenantSuspended) })
		if err != nil {
			t.Fatal(err)
		}
		stale := tn
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetTenantState(ctx, &stale, store.TenantActive) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale version: %v", err)
		}
	})
}

func timeArg(s *store.Store, t time.Time) any {
	if s.Dialect().Name == "sqlite" {
		return t.UTC().Format(store.TimeLayout)
	}
	return t
}

func TestNames(t *testing.T) {
	for _, n := range []string{"acme", "a", "a-b-1", strings.Repeat("a", 63)} {
		if err := store.ValidName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "Acme", "-a", "a_b", "a.b", "api", "ui", strings.Repeat("a", 64), "a/b", "..", ".well-known"} {
		if err := store.ValidName(n); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%q accepted", n)
		}
	}
	for _, v := range []string{"v2.0.0", "v1.5.6", "eb0d9df48e", "v2.0.0-rc1"} {
		if err := store.ValidDuckDBVersion(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"2.0.0", "eb0d9df", "EB0D9DF48E", "v2", "../x"} {
		if err := store.ValidDuckDBVersion(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestChannelVersions(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		_, ch := fixture(t, s)
		err := s.InTx(ctx, "", func(tx *store.Tx) error {
			for _, v := range []store.DuckDBVersion{{Name: "v2.0.0", Kind: "release", CAPIVersion: "v1.2.0"}, {Name: "eb0d9df48e", Kind: "dev"}} {
				if err := tx.AddDuckDBVersion(ctx, &v); err != nil {
					return err
				}
				if err := tx.AddChannelVersion(ctx, ch.ID, v.ID); err != nil {
					return err
				}
			}
			return tx.BumpChannel(ctx, &ch)
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ChannelVersions(ctx, ch.ID)
		if err != nil || strings.Join(got, ",") != "eb0d9df48e,v2.0.0" {
			t.Fatalf("versions %v %v", got, err)
		}
		vs, _ := s.ListDuckDBVersions(ctx)
		if len(vs) != 2 || vs[1].CAPIVersion != "v1.2.0" || vs[0].CAPIVersion != "" {
			t.Fatalf("list %+v", vs)
		}
		c, _ := s.GetChannel(ctx, "acme", "prod")
		if c.Version != 2 {
			t.Fatalf("channel version %d", c.Version)
		}
	})
}

func TestKeysConstraints(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		k1 := addKey(t, s, ch, "sha256:aa", store.KeyActive)
		addKey(t, s, ch, "sha256:bb", store.KeyTrusted)

		// fingerprints are unique server-wide
		k := store.Key{TenantID: ch.TenantID, ChannelID: ch.ID, Fingerprint: "sha256:aa", SignerRef: "x", PublicKey: []byte{1}, State: store.KeyTrusted}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertKey(ctx, &k, "t", false) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("duplicate fingerprint: %v", err)
		}
		// a second active key is refused by the partial unique index itself
		k = store.Key{TenantID: ch.TenantID, ChannelID: ch.ID, Fingerprint: "sha256:cc", SignerRef: "x", PublicKey: []byte{1}, State: store.KeyActive}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertKey(ctx, &k, "t", false) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("second active key: %v", err)
		}
		// a key whose tenant differs from its channel's tenant is refused by the composite foreign key
		var other store.Tenant
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			other = store.Tenant{Name: "other"}
			return tx.CreateTenant(ctx, &other)
		}); err != nil {
			t.Fatal(err)
		}
		k = store.Key{TenantID: other.ID, ChannelID: ch.ID, Fingerprint: "sha256:dd", SignerRef: "x", PublicKey: []byte{1}, State: store.KeyTrusted}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertKey(ctx, &k, "t", false) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("cross-tenant key: %v", err)
		}
		_ = tn

		// state changes are compare-and-set and recorded
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetKeyState(ctx, &k1, store.KeyTrusted, "os:1:test", true) }); err != nil {
			t.Fatal(err)
		}
		stale := k1
		stale.Version--
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetKeyState(ctx, &stale, store.KeyRetired, "t", false) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale key version: %v", err)
		}
		keys, _ := s.ListKeys(ctx, ch.ID)
		if len(keys) != 2 || keys[0].State != store.KeyTrusted || !keys[0].TrustedSince.Equal(k1.TrustedSince) {
			t.Fatalf("keys %+v", keys)
		}
		evs, _ := s.KeyEvents(ctx, ch.ID)
		if len(evs) != 3 || evs[2].From != store.KeyActive || evs[2].To != store.KeyTrusted || !evs[2].Forced || evs[0].From != "" {
			t.Fatalf("events %+v", evs)
		}
		if err := store.ExecRaw(ctx, s, "UPDATE channel_keys SET state = 'lost'"); err == nil ||
			!strings.Contains(strings.ToLower(err.Error()), "check") {
			t.Fatalf("CHECK on channel_keys.state: %v", err)
		}
		// the partial index is per channel: another channel may have its own active key
		other2 := store.Channel{TenantID: ch.TenantID, Name: "staging", Kind: store.ChannelSigned}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateChannel(ctx, &other2) }); err != nil {
			t.Fatal(err)
		}
		addKey(t, s, other2, "sha256:ee", store.KeyActive)
	})
}

// Concurrent activations under the channel lock: exactly one active key, no lost update.
func TestConcurrentActivation(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		_, ch := fixture(t, s)
		const n = 8
		for i := 0; i < n; i++ {
			addKey(t, s, ch, "sha256:k"+string(rune('a'+i)), store.KeyTrusted)
		}
		keys, _ := s.ListKeys(ctx, ch.ID)
		var wg sync.WaitGroup
		var ok atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(target string) {
				defer wg.Done()
				err := s.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
					ks, err := tx.ChannelKeys(ctx, ch.ID)
					if err != nil {
						return err
					}
					var cur, next *store.Key
					for i := range ks {
						if ks[i].State == store.KeyActive {
							cur = &ks[i]
						}
						if ks[i].ID == target {
							next = &ks[i]
						}
					}
					if cur != nil {
						if err := tx.SetKeyState(ctx, cur, store.KeyTrusted, "t", false); err != nil {
							return err
						}
					}
					return tx.SetKeyState(ctx, next, store.KeyActive, "t", false)
				})
				if err != nil {
					t.Errorf("activate: %v", err)
					return
				}
				ok.Add(1)
			}(keys[i].ID)
		}
		wg.Wait()
		keys, _ = s.ListKeys(ctx, ch.ID)
		active := 0
		for _, k := range keys {
			if k.State == store.KeyActive {
				active++
			}
		}
		if active != 1 || ok.Load() != n {
			t.Fatalf("%d active keys after %d activations", active, ok.Load())
		}
		evs, _ := s.KeyEvents(ctx, ch.ID)
		if want := n + n + (n - 1); len(evs) != want { // inserts + activations + demotions
			t.Fatalf("%d events, want %d", len(evs), want)
		}
	})
}

func TestMigrations(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		replicas := e.Fresh(t, 2)
		s := replicas[0]
		if err := s.Check(ctx); !errors.Is(err, store.ErrSchemaBehind) {
			t.Fatalf("Check on an empty database: %v", err)
		}
		// two replicas (separate connection pools) migrate at once
		var wg sync.WaitGroup
		errs := make(chan error, len(replicas))
		for _, r := range replicas {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- r.Migrate(ctx) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		level, _ := s.SchemaLevel()
		var rows, distinct int
		if err := store.QueryRowRaw(ctx, s, "SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_migrations", &rows, &distinct); err != nil {
			t.Fatal(err)
		}
		if rows != level || distinct != level {
			t.Fatalf("schema_migrations has %d rows (%d versions), want %d: a migration ran twice", rows, distinct, level)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal("not idempotent:", err)
		}
		if err := s.Check(ctx); err != nil {
			t.Fatal(err)
		}
		// a newer binary added migration level+1 that an older reader can still use: this binary runs
		if err := store.ExecRaw(ctx, s, "INSERT INTO schema_migrations (version, name, min_reader, applied_at) VALUES (?, 'future', ?, ?)",
			level+1, level, timeArg(s, s.Now())); err != nil {
			t.Fatal(err)
		}
		if err := s.Check(ctx); err != nil {
			t.Fatalf("an older binary refused a compatible database: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate on a newer compatible database: %v", err)
		}
		// a migration that needs a newer reader: refused
		if err := store.ExecRaw(ctx, s, "INSERT INTO schema_migrations (version, name, min_reader, applied_at) VALUES (?, 'contract', ?, ?)",
			level+2, level+1, timeArg(s, s.Now())); err != nil {
			t.Fatal(err)
		}
		if err := s.Check(ctx); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("Check: %v", err)
		}
		if err := s.Migrate(ctx); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("Migrate: %v", err)
		}
	})
}

// A lock that cannot be taken within LockTimeout is retried, then reported as ErrConflict; the
// transaction never proceeds without it. (SQLite serializes writers with BEGIN IMMEDIATE instead.)
func TestLockTimeout(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		if e.Name == "sqlite" {
			t.Skip("SQLite has no named locks")
		}
		s := e.Open(t)
		s.LockTimeout = 100 * time.Millisecond
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- s.InTx(ctx, "kista/test", func(*store.Tx) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		ran := false
		err := s.InTx(ctx, "kista/test", func(*store.Tx) error { ran = true; return nil })
		close(release)
		if !errors.Is(err, store.ErrConflict) || ran {
			t.Fatalf("second holder: ran=%v err=%v", ran, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		// once released, the lock is available again
		if err := s.InTx(ctx, "kista/test", func(*store.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	})
}

// A panic inside a transaction rolls it back and releases its lock.
func TestPanicReleasesLock(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		s.LockTimeout = 2 * time.Second
		func() {
			defer func() { _ = recover() }()
			_ = s.InTx(ctx, "kista/test", func(tx *store.Tx) error {
				if err := tx.CreateTenant(ctx, &store.Tenant{Name: "ghost"}); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		if err := s.InTx(ctx, "kista/test", func(*store.Tx) error { return nil }); err != nil {
			t.Fatalf("lock still held after a panic: %v", err)
		}
		if _, err := s.GetTenant(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the panicking transaction was committed: %v", err)
		}
	})
}

func TestDSNHasPassword(t *testing.T) {
	yes := []string{
		"postgres://u:p@h/db", "postgres://u@h/db?password=x", "host=h password=x", "sqlserver://u:p@h",
		"sqlserver://u@h?password=x", "server=h;user id=u;Password=x", "server=h;pwd=x", "odbc:pwd=x;server=h",
		"odbc:server=h;password=x", "host=h sslpassword=x",
	}
	no := []string{"postgres://u@h/db?sslmode=verify-full", "host=h user=u", "sqlserver://u@h?database=x"}
	for _, d := range yes {
		if !store.DSNHasPassword(d) {
			t.Errorf("%q: password not detected", d)
		}
	}
	for _, d := range no {
		if store.DSNHasPassword(d) {
			t.Errorf("%q: false positive", d)
		}
	}
}

func TestOpenRefusesInsecure(t *testing.T) {
	// TLS is judged on the parsed config: the last sslmode wins, every fallback host counts, and
	// sslmode=require (no verification) is not enough
	for _, dsn := range []string{
		"postgres://u@db.example/x?sslmode=verify-full&sslmode=disable",
		"host=db.example sslmode=verify-full sslmode=disable",
		"host=127.0.0.1,db.example sslmode=prefer",
		"postgres://u@db.example/x?sslmode=require",
	} {
		if _, err := store.OpenPostgres(ctx, dsn, nil, 1); err == nil || !strings.Contains(err.Error(), "verify-full") {
			t.Errorf("%s: %v", dsn, err)
		}
	}
	// a failover partner off loopback needs TLS like the host
	if _, err := store.OpenSQLServer(ctx, "server=127.0.0.1;failover partner=db.example;encrypt=disable", nil, 1); err == nil ||
		!strings.Contains(err.Error(), "encrypt") {
		t.Errorf("failover partner: %v", err)
	}
	for kind, cases := range map[string]map[string]bool{
		"postgres": {
			"postgres://u@127.0.0.1/x":                 true,
			"postgres://u@127.0.0.1/x?host=db.example": false,
			"host=127.0.0.1,db.example":                false,
			"host=/var/run/postgresql":                 true,
		},
		"sqlserver": {
			"sqlserver://u@127.0.0.1:1433":                       true,
			"server=tcp:127.0.0.1,1433":                          true,
			"server=localhost,1433":                              true,
			"server=127.0.0.1;addr=db.example":                   false,
			"server=127.0.0.1;failover partner=db.example":       false,
			"sqlserver://u@localhost?failoverpartner=db.example": false,
		},
	} {
		for dsn, want := range cases {
			got, err := store.DSNIsLocal(kind, dsn)
			if err != nil || got != want {
				t.Errorf("DSNIsLocal(%s, %q) = %v, %v; want %v", kind, dsn, got, err, want)
			}
		}
	}
	if _, err := store.OpenPostgres(ctx, "postgres://u:secret@db.example/x?sslmode=verify-full", nil, 1); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("password DSN: %v", err)
	}
	if _, err := store.OpenPostgres(ctx, "postgres://u@db.example/x?sslmode=require", nil, 1); err == nil || !strings.Contains(err.Error(), "verify-full") {
		t.Fatalf("sslmode: %v", err)
	}
	if _, err := store.OpenSQLServer(ctx, "sqlserver://u@db.example?database=x&encrypt=disable", nil, 1); err == nil || !strings.Contains(err.Error(), "encrypt") {
		t.Fatalf("encrypt: %v", err)
	}
}

func TestSQLitePathAndBackup(t *testing.T) {
	for _, p := range []string{"/tmp/k.db?_pragma=foreign_keys(0)", "/tmp/k#x", "/tmp/k%3F"} {
		if _, err := store.OpenSQLite(ctx, p); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%q: %v", p, err)
		}
	}
	dir := t.TempDir()
	s, err := store.OpenSQLite(ctx, dir+"/k.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dir+"/b.db"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir + "/b.db"); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		t.Fatalf("backup: %v %v", fi, err)
	}
	if err := s.Backup(ctx, dir+"/b.db"); err == nil {
		t.Fatal("backup overwrote a file")
	}
	b, err := store.OpenSQLite(ctx, dir+"/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Check(ctx); err != nil {
		t.Fatalf("backup is not a migrated store: %v", err)
	}
}

func TestSQLiteFileMode(t *testing.T) {
	path := t.TempDir() + "/k.db"
	s, err := store.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", p, fi.Mode().Perm())
		}
	}
}
