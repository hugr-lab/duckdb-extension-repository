package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCA is a self-signed certificate in PEM, as store.ca_pem holds.
func testCA(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// indent puts a multi-line value under a YAML block scalar.
func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// -config env:NAME (spec 0020): the YAML from a variable, decoded as a file is, the overrides after
// it; the variable itself is not taken for an unknown setting.
func TestLoadFromEnv(t *testing.T) {
	cfg, err := Load("env:KISTA_CONFIG", []string{"KISTA_CONFIG=" + base, "KISTA_STORE__MAX_OPEN_CONNS=3"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Kind != "sqlite" || cfg.Store.MaxOpenConns != 3 {
		t.Fatalf("%+v", cfg.Store)
	}
	if _, err := Load("env:APP_CONFIG", []string{"APP_CONFIG=" + base}); err != nil {
		t.Fatalf("a variable of any name: %v", err)
	}
	cases := map[string]struct {
		path string
		env  []string
		want string
	}{
		"not set":       {"env:KISTA_CONFIG", nil, "KISTA_CONFIG is not set"},
		"empty":         {"env:KISTA_CONFIG", []string{"KISTA_CONFIG=  "}, "is not set"},
		"a bad name":    {"env:kista-config", nil, "variable name"},
		"too large":     {"env:KISTA_CONFIG", []string{"KISTA_CONFIG=" + base + "#" + strings.Repeat("x", MaxEnvConfig)}, "larger than"},
		"unknown field": {"env:KISTA_CONFIG", []string{"KISTA_CONFIG=" + base + "stor: {}\n"}, "the variable KISTA_CONFIG"},
		"another KISTA_ variable is still checked": {"env:KISTA_CONFIG",
			[]string{"KISTA_CONFIG=" + base, "KISTA_STORE__DNS=x"}, "KISTA_STORE__DNS is not a kista setting"},
	}
	for name, c := range cases {
		if _, err := Load(c.path, c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

// The database logins of spec 0020, store.ca_pem, and the top-level aws and gcp identities.
func TestCloudLogins(t *testing.T) {
	ca := testCA(t)
	const rds = "postgres://kista@db.abc.eu-central-1.rds.amazonaws.com:5432/kista?sslmode=verify-full"
	awsID := "aws: { region: eu-central-1, identity: { kind: ecs } }\n"
	gcpID := "gcp: { principal: kista-db@acme-kista.iam.gserviceaccount.com }\n"
	pg := func(dsn, login string) string {
		return "store:\n  kind: postgres\n  dsn: '" + dsn + "'\n  login: " + login + "\n"
	}
	good := map[string]string{
		"aws":           pg(rds, "{ kind: aws }") + awsID,
		"aws with a CA": pg(rds, "{ kind: aws }") + "  ca_pem: |\n" + indent(ca) + "\n" + awsID,
		"gcp":           pg("postgres://kista-db%40acme-kista.iam@abc.def.sql.goog./kista?sslmode=verify-full", "{ kind: gcp }") + gcpID,
		"gcp, kv DSN":   pg("host=abc.def.sql.goog. user=kista-db@acme-kista.iam dbname=kista sslmode=verify-full", "{ kind: gcp }") + gcpID,
		"irsa":          pg(rds, "{ kind: aws }") + "aws: { region: eu-central-1, identity: { kind: irsa, role_arn: 'arn:aws:iam::123456789012:role/kista' } }\n",
		"password + CA": pg(rds, "{ kind: password, password_file: /run/pw }") + "  ca_pem: |\n" + indent(ca) + "\n",
		"entra":         pg(rds, "{ kind: entra }"),
		"an unused aws": base + awsID,
		"an unused gcp": base + gcpID,
	}
	for name, body := range good {
		if _, err := Load(write(t, body), nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct{ body, want string }{
		"aws without the section": {pg(rds, "{ kind: aws }"), "needs aws.region and aws.identity"},
		"aws on sqlserver": {"store: { kind: sqlserver, dsn: 'server=db.example;encrypt=strict', login: { kind: aws } }\n" + awsID,
			"needs postgres"},
		"aws with a password":       {pg(rds, "{ kind: aws, password_env: PW }") + awsID, "takes no password_env"},
		"aws with two hosts":        {pg("postgres://kista@a.example,b.example/kista?sslmode=verify-full", "{ kind: aws }") + awsID, "one host"},
		"gcp with another user":     {pg("postgres://other@abc.def.sql.goog./kista?sslmode=verify-full", "{ kind: gcp }") + gcpID, "without .gserviceaccount.com"},
		"gcp, the full email":       {pg("postgres://kista-db%40acme-kista.iam.gserviceaccount.com@abc.def.sql.goog./kista?sslmode=verify-full", "{ kind: gcp }") + gcpID, "without .gserviceaccount.com"},
		"gcp without a principal":   {pg("postgres://kista@abc.def.sql.goog./kista?sslmode=verify-full", "{ kind: gcp }"), "needs gcp.principal"},
		"entra with a password":     {pg(rds, "{ kind: entra, password_file: /run/pw }"), "entra takes no password"},
		"an unknown login":          {pg(rds, "{ kind: iam }"), "password, entra, aws, gcp or vault"},
		"ca_pem not PEM":            {pg(rds, "{ kind: password }") + "  ca_pem: 'hello'\n", "PEM certificates"},
		"ca_pem with trailing junk": {pg(rds, "{ kind: password }") + "  ca_pem: |\n" + indent(ca+"junk") + "\n", "PEM certificates"},
		"ca_pem and sslrootcert": {pg(rds+"&sslrootcert=/etc/ca.pem", "{ kind: password }") + "  ca_pem: |\n" + indent(ca) + "\n",
			"do not go together"},
		"ca_pem on sqlite":  {base + "  ca_pem: |\n" + indent(ca) + "\n", "store.ca_pem is for postgres"},
		"aws: a bad region": {base + "aws: { region: mars-1, identity: { kind: ecs } }\n", "aws.region"},
		"aws: no identity":  {base + "aws: { region: eu-central-1 }\n", "aws.identity is required"},
		"aws: static outside dev": {base + "aws: { region: eu-central-1, identity: { kind: static, access_key_file: /a, secret_key_file: /b } }\n",
			"only with profile dev"},
		"gcp: not an email": {base + "gcp: { principal: kista }\n", "service account email"},
	}
	for name, c := range bad {
		if _, err := Load(write(t, c.body), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load(write(t, base), []string{"KISTA_AWS__REGION=eu-central-1"}); err == nil || !strings.Contains(err.Error(), "can only be set in the config file") {
		t.Errorf("aws from the environment: %v", err)
	}
}

// The review's edges: the region a token is signed for, a CA with no TLS host, overrides that may
// not name a trust anchor or the configuration's own variable, and one host behind sslmode prefer.
func TestCloudLoginEdges(t *testing.T) {
	ca := testCA(t)
	awsID := "aws: { region: eu-central-1, identity: { kind: ecs } }\n"
	pg := func(dsn, login string) string {
		return "store:\n  kind: postgres\n  dsn: '" + dsn + "'\n  login: " + login + "\n"
	}
	bad := map[string]struct {
		body string
		env  []string
		want string
	}{
		"an RDS endpoint in another region": {pg("postgres://kista@db.abc.us-east-1.rds.amazonaws.com/kista?sslmode=verify-full", "{ kind: aws }") + awsID,
			nil, "the database is in us-east-1"},
		"ca_pem with no TLS host": {pg("postgres://kista@127.0.0.1/kista?sslmode=disable", "{ kind: password }") + "  ca_pem: |\n" + indent(ca) + "\n",
			nil, "no host over TLS"},
		"ca_pem from the environment": {base, []string{"KISTA_STORE__CA_PEM=x"}, "can only be set in the config file"},
	}
	for name, c := range bad {
		if _, err := Load(write(t, c.body), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load(write(t, pg("postgres://kista@db.abc.eu-central-1.rds.amazonaws.com./kista?sslmode=verify-full", "{ kind: aws }")+awsID), nil); err != nil {
		t.Errorf("an RDS endpoint in aws.region: %v", err)
	}
	if _, err := Load(write(t, pg("postgres://kista@127.0.0.1:5432/kista", "{ kind: aws }")+awsID), nil); err != nil {
		t.Errorf("sslmode prefer's fallback is the same host: %v", err)
	}
	if _, err := Load("env:KISTA_STORE__DSN", []string{"KISTA_STORE__DSN=" + base}); err == nil || !strings.Contains(err.Error(), "is a kista setting") {
		t.Errorf("a configuration variable named after a setting: %v", err)
	}
	cfg, err := Load("env:APP_CONFIG", []string{"APP_CONFIG=" + base, "APP_CONFIG=store: { kind: postgres }\n"})
	if err != nil || cfg.Store.Kind != "sqlite" {
		t.Errorf("the first of two, as os.Getenv: %+v %v", cfg.Store, err)
	}
}
