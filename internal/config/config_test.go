package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kista.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const base = `
store:
  kind: sqlite
  sqlite: { path: /var/lib/kista/kista.db }
`

func TestLoad(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != ProfileProd || cfg.Rotation.MinTrusted != 7*24*time.Hour || cfg.Store.Migrate != "auto" {
		t.Fatalf("defaults %+v", cfg)
	}
	cfg, err = Load(write(t, base+"rotation: { min_trusted: 48h }\n"), []string{
		"KISTA_STORE__MAX_OPEN_CONNS=3", "KISTA_ROTATION__MIN_DEMOTED=30h", "KISTA_TEST_POSTGRES=x", "HOME=/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.MaxOpenConns != 3 || cfg.Rotation.MinTrusted != 48*time.Hour || cfg.Rotation.MinDemoted != 30*time.Hour {
		t.Fatalf("overrides %+v", cfg)
	}
}

func TestRefused(t *testing.T) {
	cases := []struct {
		name, file string
		env        []string
		want       string
	}{
		{"unknown key", base + "stor: {}\n", nil, "stor"},
		{"unknown variable", base, []string{"KISTA_STORE__DNS=x"}, "KISTA_STORE__DNS is not a kista setting"},
		{"profile from env", base, []string{"KISTA_PROFILE=dev"}, "can only be set in the config file"},
		{"allow_file from env", base, []string{"KISTA_SIGNERS__ALLOW_FILE=true"}, "can only be set in the config file"},
		{"file_dir from env", base, []string{"KISTA_SIGNERS__FILE_DIR=/tmp"}, "can only be set in the config file"},
		{"identity kind from env", base, []string{"KISTA_AZURE__IDENTITY__KIND=default"}, "can only be set in the config file"},
		{"rotation floor", base + "rotation: { min_trusted: 1h }\n", nil, "cannot be below"},
		{"rotation floor from env", base, []string{"KISTA_ROTATION__MIN_TRUSTED=0s"}, "cannot be below"},
		{"default credential outside dev", base + "azure: { identity: { kind: default } }\n", nil, "only with profile dev"},
		{"dev with a remote store", "profile: dev\nstore: { kind: postgres, dsn: 'postgres://k@db.example/k?sslmode=verify-full' }\n", nil, "loopback"},
		{"no store", "profile: prod\n", nil, "store.kind is required"},
		{"bad duration", base, []string{"KISTA_ROTATION__MIN_TRUSTED=week"}, "not a duration"},
		{"relative key dir", base + "signers: { file_dir: keys }\n", nil, "absolute"},
		{"min_demoted floor", base + "rotation: { min_demoted: 23h59m }\n", nil, "cannot be below"},
		{"password in dsn", "store: { kind: postgres, dsn: 'postgres://k:secret@db.example/k?sslmode=verify-full' }\n", nil, "must not contain a password"},
		{"dev reaching a remote host through ?host=", "profile: dev\nstore: { kind: postgres, dsn: 'postgres://k@127.0.0.1/k?host=db.example' }\n", nil, "loopback"},
		{"dev with a remote failover partner", "profile: dev\nstore: { kind: sqlserver, dsn: 'server=127.0.0.1;failover partner=db.example' }\n", nil, "loopback"},
	}
	for _, c := range cases {
		_, err := Load(write(t, c.file), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	_, err := Load(write(t, "store: { kind: sqlite, sqlite: { path: /x }, max_open_conns: \"hunter2-secret\" }\n"), nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("value echoed: %v", err)
	}
	_, err = Load(write(t, base), []string{"KISTA_STORE__MAX_OPEN_CONNS=hunter2-secret"})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("value echoed: %v", err)
	}
}

func TestDevProfile(t *testing.T) {
	cfg, err := Load(write(t, "profile: dev\nrotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: /k }\n"+
		"store: { kind: postgres, dsn: 'postgres://k@127.0.0.1:5432/k' }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FileSignersAllowed() {
		t.Fatal("dev should allow file signers with a key dir")
	}
	cfg.Profile = ProfileProd
	if cfg.FileSignersAllowed() {
		t.Fatal("prod without allow_file")
	}
}

func TestBoundaries(t *testing.T) {
	if _, err := Load(write(t, base+"rotation: { min_trusted: 24h, min_demoted: 24h }\n"), nil); err != nil {
		t.Fatalf("24h is the floor and is allowed: %v", err)
	}
	if _, err := Load(write(t, "profile: dev\nstore: { kind: sqlserver, dsn: 'server=tcp:127.0.0.1,1433' }\n"), nil); err != nil {
		t.Fatalf("dev with a local SQL Server: %v", err)
	}
	// the password variable may be named through the environment as well
	if _, err := Load(write(t, base), []string{"KISTA_STORE__LOGIN__PASSWORD_ENV=KISTA_DB_PW", "KISTA_DB_PW=x"}); err != nil {
		t.Fatalf("password_env from the environment: %v", err)
	}
}

func TestPasswordEnvIsNotASetting(t *testing.T) {
	_, err := Load(write(t, "store: { kind: sqlite, sqlite: { path: /x }, login: { kind: password, password_env: KISTA_DB_PASSWORD } }\n"),
		[]string{"KISTA_DB_PASSWORD=x"})
	if err != nil {
		t.Fatal(err)
	}
}
