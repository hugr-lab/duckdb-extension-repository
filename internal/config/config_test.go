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
	if cfg.Profile != ProfileProd || cfg.Rotation.MinTrusted != 7*24*time.Hour || cfg.Store.Migrate != "auto" || cfg.Tenants.MinSuspended != 7*24*time.Hour {
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

// Spec 0019, amended 2026-10-31: the floor under tenants.min_suspended is the server's setting
// (24h by default), so a prod test server may take a one-second minimum by lowering it.
func TestMinSuspendedFloor(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil || cfg.Tenants.Floor() != 24*time.Hour || cfg.Tenants.MinSuspended != 7*24*time.Hour {
		t.Fatalf("defaults: %+v %v", cfg.Tenants, err)
	}
	cfg, err = Load(write(t, base+"tenants: { min_suspended_floor: 1s, min_suspended: 1s }\n"), nil)
	if err != nil || cfg.Tenants.Floor() != time.Second || cfg.Tenants.MinSuspended != time.Second {
		t.Fatalf("a lowered floor: %+v %v", cfg.Tenants, err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "tenants.min_suspended_floor is 1s, below its default 24h") {
		t.Fatalf("a lowered floor warns: %q", w)
	}
	if _, err := Load(write(t, base+"tenants: { min_suspended_floor: 1s, min_suspended: 0s }\n"), nil); err == nil || !strings.Contains(err.Error(), "tenants.min_suspended") {
		t.Fatalf("0 under a lowered floor, outside dev: %v", err)
	}
	cfg, err = Load(write(t, base+"tenants: { min_suspended_floor: 240h, min_suspended: 240h }\n"), nil)
	if err != nil || cfg.Tenants.Floor() != 240*time.Hour || len(cfg.Warnings()) != 0 {
		t.Fatalf("a raised floor, the minimum at it: %+v %v", cfg.Tenants, err)
	}
	if _, err := Load(write(t, base+"tenants: { min_suspended_floor: 240h }\n"), nil); err == nil ||
		!strings.Contains(err.Error(), "below tenants.min_suspended_floor (240h) outside") {
		t.Fatalf("the 168h default under a raised floor: %v", err)
	}
}

// Spec 0003, amended 2026-10-31: the floor under the rotation minimums is rotation.min_floor (24h by
// default), file-only.
func TestRotationFloor(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil || cfg.Rotation.Floor() != 24*time.Hour {
		t.Fatalf("defaults: %+v %v", cfg.Rotation, err)
	}
	cfg, err = Load(write(t, base+"rotation: { min_floor: 1s, min_trusted: 1s, min_demoted: 1m }\n"), nil)
	if err != nil || cfg.Rotation.Floor() != time.Second || cfg.Rotation.MinTrusted != time.Second {
		t.Fatalf("a lowered floor: %+v %v", cfg.Rotation, err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "rotation.min_floor is 1s, below its default 24h") {
		t.Fatalf("a lowered floor warns: %q", w)
	}
	cfg, err = Load(write(t, base+"rotation: { min_floor: 240h, min_trusted: 240h, min_demoted: 300h }\n"), nil)
	if err != nil || cfg.Rotation.Floor() != 240*time.Hour || len(cfg.Warnings()) != 0 {
		t.Fatalf("a raised floor, minimums at and above it: %+v %v", cfg.Rotation, err)
	}
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"floor 0":                 {base + "rotation: { min_floor: 0s }\n", nil, "rotation.min_floor is at least 1s"},
		"floor negative":          {base + "rotation: { min_floor: -1h }\n", nil, "rotation.min_floor is at least 1s"},
		"under a raised floor":    {base + "rotation: { min_floor: 240h }\n", nil, "cannot be below rotation.min_floor"},
		"0 under a lowered floor": {base + "rotation: { min_floor: 1s, min_trusted: 0s }\n", nil, "cannot be below rotation.min_floor"},
		"floor from env":          {base, []string{"KISTA_ROTATION__MIN_FLOOR=1s"}, "can only be set in the config file"},
		"floor 0 in dev": {"profile: dev\nstore: { kind: sqlite, sqlite: { path: /tmp/k.db } }\nrotation: { min_floor: 0s, min_trusted: 0s, min_demoted: 0s }\n", nil,
			"rotation.min_floor is at least 1s (leave it out for its default, 24h)"},
		"the floor as the docs write it": {base + "rotation: { min_trusted: 1h }\n", nil, "below rotation.min_floor (24h) outside"},
		"suspended floor from env":       {base, []string{"KISTA_TENANTS__MIN_SUSPENDED_FLOOR=1s"}, "can only be set in the config file"},
		"a ceiling from env":             {base, []string{"KISTA_LIMITS__CEILINGS__EVENTS_RETENTION=8760h"}, "can only be set in the config file"},
		"a count ceiling from env":       {base, []string{"KISTA_LIMITS__CEILINGS__CHANNELS=5"}, "can only be set in the config file"},
	} {
		if _, err := Load(write(t, c.file), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
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
		{"min_suspended floor", base + "tenants: { min_suspended: 1h }\n", nil, "tenants.min_suspended"},
		{"min_suspended_floor 0", base + "tenants: { min_suspended_floor: 0s, min_suspended: 1h }\n", nil, "tenants.min_suspended_floor is at least 1s"},
		{"min_suspended_floor negative", base + "tenants: { min_suspended_floor: -1s }\n", nil, "tenants.min_suspended_floor is at least 1s"},
		{"min_suspended_floor 0 in dev", "profile: dev\nstore: { kind: sqlite, sqlite: { path: /tmp/k.db } }\ntenants: { min_suspended_floor: 0s, min_suspended: 0s }\n", nil, "tenants.min_suspended_floor is at least 1s"},
		{"min_suspended under a raised floor", base + "tenants: { min_suspended_floor: 240h }\n", nil, "below tenants.min_suspended_floor"},
		{"a refused key that is not a key id", base + "licenses: { refuse_keys: [ 'sha256:AB' ] }\n", nil, "licenses.refuse_keys: entry 1"},
		{"licensing off from env", base, []string{"KISTA_LICENSES__ENABLED=false"}, "can only be set in the config file"},
		{"min_suspended from env", base, []string{"KISTA_TENANTS__MIN_SUSPENDED=48h"}, "can only be set in the config file"},
		{"min_suspended negative in dev", "profile: dev\nstore: { kind: sqlite, sqlite: { path: /tmp/k.db } }\nrotation: { min_trusted: 1h, min_demoted: 1h }\ntenants: { min_suspended: -1s }\n", nil, "tenants.min_suspended"},
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

// Spec 0021: licensing is on by default; licenses.keys is accepted (and ignored, with a warning).
func TestLicenses(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil || !cfg.Licenses.On() || len(cfg.Licenses.Refused()) != 0 {
		t.Fatalf("defaults: %+v %v", cfg.Licenses, err)
	}
	kid := "sha256:" + strings.Repeat("ab", 32)
	cfg, err = Load(write(t, base+"licenses: { enabled: false, refuse_keys: [ '"+kid+"' ], keys: [ 'old' ] }\n"), nil)
	if err != nil || cfg.Licenses.On() || !cfg.Licenses.Refused()[kid] || len(cfg.Licenses.Keys) != 1 {
		t.Fatalf("set: %+v %v", cfg.Licenses, err)
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
		"later kind":       strings.Replace(ok, "kind: vault", "kind: gcpkms", 1),
		"awskms no block":  strings.Replace(ok, "kind: vault", "kind: awskms", 1),
		"unknown kind":     strings.Replace(ok, "kind: vault", "kind: pkcs11", 1),
		"relative token":   strings.Replace(ok, "/var/run/secrets/kista/vault", "vault", 1),
		"no role":          strings.Replace(ok, "role: kista, ", "", 1),
		"approle":          strings.Replace(ok, "kind: kubernetes", "kind: approle", 1),
		"mount escapes":    strings.Replace(ok, "mount: transit", "mount: ../sys", 1),
		"auth mount query": strings.Replace(ok, "auth: { kind: kubernetes,", "auth: { mount: \"kubernetes?x=1\", kind: kubernetes,", 1),
		"auth mount up":    strings.Replace(ok, "auth: { kind: kubernetes,", "auth: { mount: ../sys, kind: kubernetes,", 1),
		"relative ca_file": strings.Replace(ok, "mount: transit,", "mount: transit, ca_file: ca.pem,", 1),
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

// An awskms source (spec 0004 phase 3): region, account, key ids in allow, an AWS identity.
func TestAWSKMSSource(t *testing.T) {
	src := func(fields, identity, allow string) string {
		return base + "signers:\n  sources:\n    - name: aws\n      kind: awskms\n      awskms: { " + fields + " }\n" +
			"      identity: " + identity + "\n      allow: " + allow + "\n"
	}
	okF, okID, okAllow := `region: eu-central-1, account: "123456789012"`, "{ kind: pod_identity }", `["1234abcd-12ab-34cd-56ef-1234567890ab"]`
	if _, err := Load(write(t, src(okF, okID, okAllow)), nil); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if _, err := Load(write(t, src(okF+`, fips: true, role_arn: "arn:aws:iam::210987654321:role/keys"`, okID, `["*"]`)), nil); err != nil {
		t.Fatalf("fips, assume role, any key: %v", err)
	}
	if _, err := Load(write(t, "profile: dev\nrotation: { min_trusted: 0s, min_demoted: 0s }\n"+strings.Replace(src(okF+`, endpoint_url: "http://127.0.0.1:5000"`,
		"{ kind: static, access_key_file: /a, secret_key_file: /s }", okAllow), "/var/lib/kista/kista.db", "/tmp/k.db", 1)), nil); err != nil {
		t.Fatalf("dev emulator: %v", err)
	}
	for name, c := range map[string][3]string{
		"no region":            {`account: "123456789012"`, okID, okAllow},
		"bad account":          {`region: eu-central-1, account: "1234"`, okID, okAllow},
		"endpoint outside dev": {okF + `, endpoint_url: "http://127.0.0.1:5000"`, okID, okAllow},
		"role other partition": {okF + `, role_arn: "arn:aws-cn:iam::123456789012:role/x"`, okID, okAllow},
		"prefix allow":         {okF, okID, `["1234"]`},
		"mrk without multi":    {okF, okID, `["mrk-0123456789abcdef0123456789abcdef"]`},
		"static outside dev":   {okF, "{ kind: static, access_key_file: /a, secret_key_file: /s }", okAllow},
		"azure identity":       {okF, "{ kind: managed }", okAllow},
		"irsa no role":         {okF, "{ kind: irsa }", okAllow},
	} {
		if _, err := Load(write(t, src(c[0], c[1], c[2])), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A gcpkms source (spec 0004 phase 4): a key ring, the principal outside dev, a credentials file or
// the metadata server, the fake only in dev without credentials.
func TestGCPKMSSource(t *testing.T) {
	src := func(fields string) string {
		return base + "signers:\n  sources:\n    - name: gcp\n      kind: gcpkms\n      gcpkms: { " + fields + " }\n      allow: [\"ext-\"]\n"
	}
	ring := `key_ring: projects/kista-test-1/locations/europe-west4/keyRings/kista`
	pr := `, principal: kista-signer@kista-test-1.iam.gserviceaccount.com`
	for name, f := range map[string]string{"metadata": ring + pr, "federation": ring + pr + ", credentials_file: /etc/kista/gcp.json"} {
		if _, err := Load(write(t, src(f)), nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	dev := "profile: dev\nrotation: { min_trusted: 0s, min_demoted: 0s }\n" + strings.Replace(src(ring+`, endpoint_url: "http://127.0.0.1:8080"`), "/var/lib/kista/kista.db", "/tmp/k.db", 1)
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("dev fake: %v", err)
	}
	for name, f := range map[string]string{
		"no principal":         ring,
		"bad principal":        ring + `, principal: kista@gmail.com`,
		"bad ring":             `key_ring: projects/p/keyRings/k` + pr,
		"project number":       `key_ring: projects/123456/locations/europe-west4/keyRings/kista` + pr,
		"relative creds":       ring + pr + ", credentials_file: gcp.json",
		"fake outside dev":     ring + pr + `, endpoint_url: "http://127.0.0.1:8080"`,
		"software outside dev": ring + pr + ", require_hsm: false",
	} {
		if _, err := Load(write(t, src(f)), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(write(t, "profile: dev\nrotation: { min_trusted: 0s, min_demoted: 0s }\n"+strings.Replace(src(ring+
		`, endpoint_url: "http://127.0.0.1:8080", credentials_file: /k.json`), "/var/lib/kista/kista.db", "/tmp/k.db", 1)), nil); err == nil {
		t.Error("the fake with a credentials file: accepted")
	}
}
