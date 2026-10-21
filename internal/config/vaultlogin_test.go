package config

import (
	"strings"
	"testing"
	"time"
)

func TestVaultLogin(t *testing.T) {
	ok := `
store:
  kind: postgres
  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full
  login:
    kind: vault
    vault:
      address: https://bao.internal:8200
      ca_file: /etc/kista/secrets/ca/bao-ca.pem
      auth: {kind: kubernetes, role: kista-db, token_file: /var/run/secrets/kista/bao/token}
      kv: {mount: secret, path: kista/db, field: password}
`
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Store.Login.Vault
	if v == nil || v.KV.Path != "kista/db" || v.VaultLoginCacheOrDefault() != time.Minute {
		t.Fatalf("%+v", v)
	}
	static := strings.Replace(ok, "kv: {mount: secret, path: kista/db, field: password}", "static_role: {mount: database, name: kista}", 1)
	if _, err := Load(write(t, static), nil); err != nil {
		t.Fatalf("static_role: %v", err)
	}
	sqlserver := strings.Replace(static, "kind: postgres\n  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full",
		"kind: sqlserver\n  dsn: sqlserver://kista@db.internal:1433?database=kista&encrypt=strict", 1)
	if _, err := Load(write(t, sqlserver), nil); err != nil {
		t.Fatalf("sqlserver: %v", err)
	}
	cached := strings.Replace(ok, "      kv:", "      cache: 10m\n      kv:", 1)
	if cfg, err := Load(write(t, cached), nil); err != nil || cfg.Store.Login.Vault.VaultLoginCacheOrDefault() != 10*time.Minute {
		t.Fatalf("cache: %v", err)
	}
	if cfg, err := Load(write(t, strings.Replace(ok, "      kv:", "      cache: 0s\n      kv:", 1)), nil); err != nil ||
		cfg.Store.Login.Vault.VaultLoginCacheOrDefault() != 0 {
		t.Fatalf("no cache: %v", err)
	}
	bad := map[string]struct{ file, want string }{
		"no block":            {strings.Split(ok, "    vault:")[0], "needs the store.login.vault block"},
		"block without kind":  {strings.Replace(ok, "kind: vault", "kind: password", 1), "goes with store.login.kind vault"},
		"sqlite":              {strings.Replace(ok, "kind: postgres\n  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full", "kind: sqlite\n  sqlite: {path: /x/k.db}", 1), "needs postgres or sqlserver"},
		"a password file too": {strings.Replace(ok, "    kind: vault", "    kind: vault\n    password_file: /etc/kista/pw", 1), "takes no password_env"},
		"http":                {strings.Replace(ok, "https://bao.internal:8200", "http://bao.internal:8200", 1), "plain http only on loopback"},
		"an address path":     {strings.Replace(ok, "https://bao.internal:8200", "https://bao.internal:8200/v1", 1), "scheme://host"},
		"approle":             {strings.Replace(ok, "kind: kubernetes", "kind: approle", 1), "auth.kind must be"},
		"no role":             {strings.Replace(ok, "role: kista-db, ", "", 1), "auth.role is required"},
		"relative token":      {strings.Replace(ok, "/var/run/secrets/kista/bao/token", "token", 1), "token_file must be an absolute path"},
		"relative ca":         {strings.Replace(ok, "/etc/kista/secrets/ca/bao-ca.pem", "ca.pem", 1), "ca_file must be an absolute path"},
		"bad auth mount":      {strings.Replace(ok, "auth: {kind: kubernetes,", "auth: {mount: ../sys, kind: kubernetes,", 1), "auth.mount"},
		"both":                {strings.Replace(ok, "      kv:", "      static_role: {mount: database, name: kista}\n      kv:", 1), "exactly one of kv and static_role"},
		"neither":             {strings.Replace(ok, "      kv: {mount: secret, path: kista/db, field: password}\n", "", 1), "exactly one of kv and static_role"},
		"dynamic":             {strings.Replace(ok, "kv: {mount: secret, path: kista/db, field: password}", "dynamic_role: {mount: database, name: kista}", 1), "phase 2"},
		"kv path escapes":     {strings.Replace(ok, "path: kista/db", "path: ../sys/raw", 1), "relative Vault paths"},
		"kv absolute mount":   {strings.Replace(ok, "mount: secret", "mount: /secret", 1), "relative Vault paths"},
		"kv query":            {strings.Replace(ok, "path: kista/db", "path: \"kista/db?version=1\"", 1), "relative Vault paths"},
		"kv no field":         {strings.Replace(ok, ", field: password", "", 1), "field is required"},
		"kv version":          {strings.Replace(ok, "field: password}", "field: password, version: -1}", 1), "cannot be negative"},
		"cache too long":      {strings.Replace(ok, "      kv:", "      cache: 11m\n      kv:", 1), "0s..10m"},
		"cache on a role":     {strings.Replace(static, "      static_role:", "      cache: 1m\n      static_role:", 1), "cache is for kv"},
		"role name":           {strings.Replace(static, "name: kista}", "name: \"a/b\"}", 1), "static_role"},
		"role mount":          {strings.Replace(static, "mount: database", "mount: database/../sys", 1), "static_role"},
	}
	for name, tc := range bad {
		_, err := Load(write(t, tc.file), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	// the block is the file's: never from the environment
	if _, err := Load(write(t, ok), []string{"KISTA_STORE__LOGIN__VAULT=x"}); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Fatalf("env: %v", err)
	}
	// profile dev: http on loopback (a dev Vault)
	dev := "profile: dev\n" + strings.Replace(strings.Replace(ok, "https://bao.internal:8200", "http://127.0.0.1:8200", 1),
		"db.internal:5432/kista?sslmode=verify-full", "127.0.0.1:5432/kista", 1)
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("dev: %v", err)
	}
	// a kubesecret source's directory may not hold the login's token
	ks := ok + `signers:
  sources:
    - {name: k8s, kind: kubesecret, allow: ["*"], kubesecret: {dir: /var/run/secrets/kista/bao, etcd_encryption_at_rest: true}}
`
	if _, err := Load(write(t, ks), nil); err == nil || !strings.Contains(err.Error(), "store.login.vault.auth.token_file") {
		t.Fatalf("token in a key directory: %v", err)
	}
}
