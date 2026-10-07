package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// Tier A: repositories on a local directory. No HTTP, no httpfs.

func createRepo(name, prefix string, keys ...string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = sqlString(k)
	}
	stmt := "CREATE OR REPLACE EXTENSION REPOSITORY " + name + " WITH PREFIX " + sqlString(prefix)
	if len(keys) > 0 {
		stmt += " USING PUBLIC KEYS " + strings.Join(q, ", ")
	}
	return stmt
}

// Case 1: a signed tree, keys as PEM and as base64 DER; CPP and C_STRUCT extensions install and load
// from the repository, and the reported fingerprint is extfile.Fingerprint.
func TestLocalInstallAndLoad(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "")
	r.add("demo_capi", k, "")

	for _, form := range []string{k.pem, k.b64} {
		s := newSession(t, b, nil)
		res := s.exec(
			createRepo("r", r.dir, form),
			"SELECT unnest(key_fingerprints) FROM duckdb_extension_repositories() WHERE repository_name = 'r'",
			"INSTALL loadable_extension_demo FROM r",
			"LOAD loadable_extension_demo FROM r",
			"SELECT test_alias_hello()",
			"INSTALL demo_capi FROM r",
			"LOAD demo_capi FROM r",
			"SELECT add_numbers_together(40, 2)",
		)
		mustOK(t, res)
		if got := res[1].Rows[0][0].(string); !strings.Contains(got, k.fp) {
			t.Fatalf("fingerprints %s, want %s", got, k.fp)
		}
		if got := res[7].Rows[0][0]; got != "42" {
			t.Fatalf("add_numbers_together: %v", got)
		}
	}
}

// Case 2: a binary signed with another key is refused at INSTALL.
func TestLocalForeignSignatureRefused(t *testing.T) {
	b := needBuild(t)
	trusted, other := newKey(t), newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", other, "")
	s := newSession(t, b, nil)
	res := s.exec(createRepo("r", r.dir, trusted.pem), "INSTALL loadable_extension_demo FROM r")
	mustOK(t, res[:1])
	mustFail(t, res[1], "valid signature")
}

// Case 3: key forms DuckDB refuses at CREATE: PKCS#1 PEM and a 3072-bit key.
func TestLocalRefusedKeyForms(t *testing.T) {
	b := needBuild(t)
	k2048, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&k2048.PublicKey)}))
	k3072, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k3072.PublicKey)
	big := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	dir := t.TempDir()
	good := newKey(t)
	res := newSession(t, b, nil).exec(createRepo("a", dir, pkcs1), createRepo("b", dir, big), createRepo("c", dir, good.pem))
	mustFail(t, res[0], "public key")
	mustFail(t, res[1], "2048")
	mustOK(t, res[2:])
	for _, r := range res[:2] {
		t.Logf("%s", r.Error)
	}
	// and kista refuses them the same way
	for _, key := range []string{pkcs1, big} {
		if _, err := extfile.ParsePublicKey(key); err == nil {
			t.Error("extfile accepted a key DuckDB refuses")
		}
	}
}

// Case 4: INSTALL x FROM r VERSION 'v' reads the versioned layout, with v verbatim.
func TestLocalVersionedLayout(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "Weird-1.0")
	s := newSession(t, b, nil)
	res := s.exec(
		createRepo("r", r.dir, k.pem),
		"INSTALL loadable_extension_demo FROM r VERSION 'Weird-1.0'",
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
	)
	mustOK(t, res)
	// nothing at the flat path, so a plain INSTALL fails
	res = newSession(t, b, nil).exec(createRepo("r", r.dir, k.pem), "INSTALL loadable_extension_demo FROM r")
	if res[1].OK {
		t.Fatal("flat install found a file that only exists in the versioned layout")
	}
}

// Cases 5 and 6: keys are checked on every LOAD x FROM r. After a CREATE OR REPLACE with key B only,
// an A-signed install no longer loads; FORCE INSTALL of a B-signed file fixes it. With A and B both
// trusted, both load.
func TestLocalRotation(t *testing.T) {
	b := needBuild(t)
	a, bk := newKey(t), newKey(t)
	ra, rb := newRepo(t, b), newRepo(t, b)
	ra.add("loadable_extension_demo", a, "")
	rb.add("loadable_extension_demo", bk, "")

	extDir := t.TempDir()
	opts := map[string]string{"extension_directory": extDir}
	// install the A-signed build
	mustOK(t, newSession(t, b, opts).exec(createRepo("r", ra.dir, a.pem), "INSTALL loadable_extension_demo FROM r"))

	// a new process: the repository now trusts B only
	res := newSession(t, b, opts).exec(
		createRepo("r", ra.dir, bk.pem),
		"LOAD loadable_extension_demo FROM r",
	)
	mustOK(t, res[:1])
	mustFail(t, res[1], "signature")

	// both keys: the A-signed install loads, and so does a B-signed one
	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r", ra.dir, a.pem, bk.pem),
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
	))
	optsB := map[string]string{"extension_directory": t.TempDir()}
	mustOK(t, newSession(t, b, optsB).exec(
		createRepo("r", rb.dir, a.pem, bk.pem),
		"INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
	))

	// B only, then FORCE INSTALL from the B-signed tree: loads again
	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r", rb.dir, bk.pem),
		"FORCE INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
	))
}

// Case 7: installs from two repositories coexist under repositories/<name>/, and LOAD x FROM r2
// refuses an install that came from r1.
func TestLocalTwoRepositories(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r1, r2 := newRepo(t, b), newRepo(t, b)
	r1.add("loadable_extension_demo", k, "")
	r2.add("loadable_extension_demo", k, "")
	extDir := t.TempDir()
	opts := map[string]string{"extension_directory": extDir}

	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r1", r1.dir, k.pem),
		createRepo("r2", r2.dir, k.pem),
		"INSTALL loadable_extension_demo FROM r1",
		"INSTALL loadable_extension_demo FROM r2",
	))
	for _, name := range []string{"r1", "r2"} {
		matches, _ := filepath.Glob(filepath.Join(extDir, "*", "*", "repositories", name, "loadable_extension_demo.duckdb_extension"))
		if len(matches) != 1 {
			t.Fatalf("no install under repositories/%s in %s", name, extDir)
		}
	}

	extDir2 := t.TempDir()
	opts2 := map[string]string{"extension_directory": extDir2}
	res := newSession(t, b, opts2).exec(
		createRepo("r1", r1.dir, k.pem),
		createRepo("r2", r2.dir, k.pem),
		"INSTALL loadable_extension_demo FROM r1",
		"LOAD loadable_extension_demo FROM r2",
	)
	mustOK(t, res[:3])
	// DuckDB looks only in repositories/r2/, so an install from r1 is not found there
	mustFail(t, res[3], "not found")
}

// Case 8: custom_extension_repository is core-typed: a plain INSTALL from our tree checks DuckDB's
// built-in keys, so our signature is refused.
func TestLocalCustomRepositoryIsCoreTyped(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "")
	res := newSession(t, b, map[string]string{"custom_extension_repository": r.dir}).exec(
		"INSTALL loadable_extension_demo",
	)
	mustFail(t, res[0], "signature")
}
