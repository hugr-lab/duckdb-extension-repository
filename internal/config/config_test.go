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

func TestSources(t *testing.T) {
	ok := base + `
signers:
  sources:
    - name: bao
      kind: vault
      allow: ["ext-"]
      vault: { address: "https://bao.internal:8200", mount: transit, software_keys: true,
               auth: { kind: kubernetes, role: kista, token_file: /var/run/secrets/kista/vault } }
`
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Signers.Sources) != 1 || cfg.Signers.Sources[0].Vault.Auth.Role != "kista" {
		t.Fatalf("%+v", cfg.Signers.Sources)
	}
	bad := map[string]string{
		"reserved name":    strings.Replace(ok, "name: bao", "name: file", 1),
		"bad name":         strings.Replace(ok, "name: bao", "name: Bao", 1),
		"empty allow":      strings.Replace(ok, `allow: ["ext-"]`, "allow: []", 1),
		"empty prefix":     strings.Replace(ok, `allow: ["ext-"]`, `allow: [""]`, 1),
		"http remote":      strings.Replace(ok, "https://bao.internal:8200", "http://bao.internal:8200", 1),
		"no software_keys": strings.Replace(ok, "software_keys: true", "software_keys: false", 1),
		"later kind":       strings.Replace(ok, "kind: vault", "kind: awskms", 1),
		"unknown kind":     strings.Replace(ok, "kind: vault", "kind: pkcs11", 1),
		"relative token":   strings.Replace(ok, "/var/run/secrets/kista/vault", "vault", 1),
		"no role":          strings.Replace(ok, "role: kista, ", "", 1),
		"approle":          strings.Replace(ok, "kind: kubernetes", "kind: approle", 1),
		"mount escapes":    strings.Replace(ok, "mount: transit", "mount: ../sys", 1),
		"duplicate source": ok + `    - name: bao
      kind: vault
      allow: ["*"]
      vault: { address: "https://b:8200", mount: t, software_keys: true, auth: { kind: token_file, token_file: /t } }
`,
	}
	for name, file := range bad {
		if _, err := Load(write(t, file), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// sources cannot come from the environment
	if _, err := Load(write(t, base), []string{"KISTA_SIGNERS__SOURCES=x"}); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Fatalf("env: %v", err)
	}
	// dev allows http on loopback without software_keys
	dev := "profile: dev\n" + strings.Replace(strings.Replace(ok, "https://bao.internal:8200", "http://127.0.0.1:8200", 1), "software_keys: true", "software_keys: false", 1)
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("dev: %v", err)
	}
}

func TestAzureKVSources(t *testing.T) {
	ok := base + `
signers:
  sources:
    - name: prod
      kind: azurekv
      allow: ["ext-"]
      azurekv: { vault: kista-prod, cloud: china }
      identity: { kind: workload, client_id: "c", tenant_id: "t" }
`
	if _, err := Load(write(t, ok), nil); err != nil {
		t.Fatal(err)
	}
	hsm := strings.Replace(ok, "vault: kista-prod", "managed_hsm: kista-hsm", 1)
	if _, err := Load(write(t, hsm), nil); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"both":             strings.Replace(ok, "vault: kista-prod", "vault: kista-prod, managed_hsm: h", 1),
		"neither":          strings.Replace(ok, "vault: kista-prod, ", "", 1),
		"bad name":         strings.Replace(ok, "kista-prod", "kista.prod", 1),
		"host in name":     strings.Replace(ok, "kista-prod", "evil.example.com", 1),
		"unknown cloud":    strings.Replace(ok, "cloud: china", "cloud: mars", 1),
		"no identity":      strings.Replace(ok, `      identity: { kind: workload, client_id: "c", tenant_id: "t" }`+"\n", "", 1),
		"default identity": strings.Replace(ok, "kind: workload", "kind: default", 1),
		"software keys":    strings.Replace(ok, "cloud: china", "cloud: china, require_hsm: false", 1),
		"vault block":      strings.Replace(ok, "kind: azurekv", "kind: vault", 1),
		"approle identity": strings.Replace(ok, "kind: workload", "kind: approle", 1),
		"workload, no ids": strings.Replace(ok, `client_id: "c", tenant_id: "t"`, `client_id: "c"`, 1),
		"double dash":      strings.Replace(ok, "kista-prod", "kista--prod", 1),
		"trailing dash":    strings.Replace(ok, "kista-prod", "kista-prod-", 1),
		"too short":        strings.Replace(ok, "kista-prod", "kp", 1),
		"too long":         strings.Replace(ok, "kista-prod", "k123456789012345678901234", 1),
		"upper-case allow": strings.Replace(ok, `allow: ["ext-"]`, `allow: ["EXT-"]`, 1),
	} {
		if _, err := Load(write(t, file), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAzureKVDev(t *testing.T) {
	dev := "profile: dev\n" + base + `
signers:
  sources:
    - name: prod
      kind: azurekv
      allow: ["ext-"]
      azurekv: { vault: kista-prod, require_hsm: false }
      identity: { kind: default }
`
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatal(err)
	}
}
