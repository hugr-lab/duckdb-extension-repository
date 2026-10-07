package signer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := GenerateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileSignAndVerify(t *testing.T) {
	s, err := OpenFile(keyFile(t))
	if err != nil {
		t.Fatal(err)
	}
	var _ Signer = s
	h := extfile.BodyHash{1, 2, 3}
	sig, err := s.Sign(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := extfile.Verify(h, sig, []*rsa.PublicKey{s.Public()})
	if !ok || fp != extfile.Fingerprint(s.Public()) {
		t.Fatal("signature does not verify")
	}
	if !strings.HasPrefix(s.ID(), "file:") {
		t.Fatalf("ID %q", s.ID())
	}
}

func TestGenerateKeyFileDoesNotOverwrite(t *testing.T) {
	path := keyFile(t)
	if err := GenerateKeyFile(path); err == nil {
		t.Fatal("overwrote an existing key")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func writeKey(t *testing.T, block *pem.Block, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenFileRefuses(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	smallDER, _ := x509.MarshalPKCS8PrivateKey(small)

	// PKCS#1 is accepted
	if _, err := OpenFile(writeKey(t, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}, 0o600)); err != nil {
		t.Fatalf("pkcs1: %v", err)
	}

	// each case: the path and a substring the error must contain
	cases := map[string][2]string{
		"encrypted pkcs8": {writeKey(t, &pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: pkcs8}, 0o600), "encrypted"},
		"encrypted pem": {writeKey(t, &pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
			Bytes: x509.MarshalPKCS1PrivateKey(key)}, 0o600), "encrypted"},
		"1024 bits":  {writeKey(t, &pem.Block{Type: "PRIVATE KEY", Bytes: smallDER}, 0o600), "1024 bits"},
		"public key": {writeKey(t, &pem.Block{Type: "PUBLIC KEY", Bytes: pkcs8}, 0o600), "not a private key"},
		"missing":    {filepath.Join(t.TempDir(), "nope.pem"), "no such file"},
	}
	if runtime.GOOS != "windows" {
		cases["group readable"] = [2]string{writeKey(t, &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}, 0o640), "mode 0640"}
		link := filepath.Join(t.TempDir(), "link.pem")
		if err := os.Symlink(writeKey(t, &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}, 0o600), link); err != nil {
			t.Fatal(err)
		}
		cases["symlink"] = [2]string{link, "symlink"}
		cases["directory"] = [2]string{t.TempDir(), "not a regular file"}
	}
	for name, c := range cases {
		_, err := OpenFile(c[0])
		if err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c[1])
		}
	}

	twoBlocks := filepath.Join(t.TempDir(), "two.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if err := os.WriteFile(twoBlocks, append(b, b...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(twoBlocks); err == nil {
		t.Error("two blocks: accepted")
	}
}
