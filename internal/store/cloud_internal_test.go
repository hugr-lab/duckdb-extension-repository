package store

import (
	"context"
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

	"github.com/jackc/pgx/v5"
)

// selfSigned is a CA certificate naming dns, in PEM.
func selfSigned(t *testing.T, dns string) (string, *x509.Certificate) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "db"}, DNSNames: []string{dns},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), c
}

func TestCAPEM(t *testing.T) {
	ca, _ := selfSigned(t, "db.example")
	if !ValidCAPEM(ca) || !ValidCAPEM(ca+ca) || ValidCAPEM("") || ValidCAPEM(ca+"x") || ValidCAPEM("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n") {
		t.Fatal("ValidCAPEM: certificates only")
	}
	for dsn, want := range map[string]bool{
		"postgres://u@db/x?sslmode=verify-full&sslrootcert=/ca.pem": true,
		"host=db sslrootcert=/ca.pem sslmode=verify-full":           true,
		"postgres://u@db/x?sslmode=verify-full":                     false,
	} {
		if DSNHasRootCert(dsn) != want {
			t.Errorf("DSNHasRootCert(%q) != %v", dsn, want)
		}
	}
	ctx := context.Background()
	if _, err := OpenPostgres(ctx, "postgres://u@db.example/x?sslmode=verify-full&sslrootcert=/ca.pem", nil, 1, []byte(ca)); err == nil ||
		!strings.Contains(err.Error(), "do not go together") {
		t.Fatalf("ca_pem with sslrootcert: %v", err)
	}
	if _, err := OpenPostgres(ctx, "postgres://u@db.example/x?sslmode=verify-full", nil, 1, []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "not a PEM certificate") {
		t.Fatalf("a bad ca_pem: %v", err)
	}
	// ca_pem does not lift the verify-full rule
	if _, err := OpenPostgres(ctx, "postgres://u@db.example/x?sslmode=require", nil, 1, []byte(ca)); err == nil ||
		!strings.Contains(err.Error(), "verify-full") {
		t.Fatalf("require with ca_pem: %v", err)
	}
}

// Cloud SQL names its instance with a trailing dot under GOOGLE_MANAGED_CAS_CA (spec 0020): a DSN
// host written with the dot is the server name pgx checks, and Go matches it exactly.
func TestTrailingDotHost(t *testing.T) {
	const host = "abc.def.sql.goog."
	ca, cert := selfSigned(t, host)
	cfg, err := pgx.ParseConfig("host=" + host + " user=u dbname=x sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSConfig == nil || cfg.TLSConfig.ServerName != host {
		t.Fatalf("server name %q", cfg.TLSConfig.ServerName)
	}
	pool, err := caPool([]byte(ca))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: cfg.TLSConfig.ServerName}); err != nil {
		t.Fatalf("the dotted name: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: strings.TrimSuffix(host, ".")}); err == nil {
		t.Fatal("without the dot the name does not match: the docs say to write it")
	}
}

// A token login needs one host: its token is bound to the host it was made for.
func TestTokenLoginOneHost(t *testing.T) {
	l := TokenLogin{For: func(context.Context, Target) (string, error) { return "t", nil }}
	_, err := OpenPostgres(context.Background(), "postgres://u@a.example,b.example/x?sslmode=verify-full", l, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "one host") {
		t.Fatalf("two hosts: %v", err)
	}
	if _, err := l.Password(context.Background()); err == nil {
		t.Fatal("a token login without its target")
	}
	tg, err := PostgresTarget("host=a.example,b.example user=k sslmode=verify-full")
	if err != nil || tg.Hosts != 2 || tg.User != "k" {
		t.Fatalf("%+v %v", tg, err)
	}
}
