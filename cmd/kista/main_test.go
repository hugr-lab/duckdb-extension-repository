package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// writeExtension writes a small signed extension and the signer's public key; it returns their paths.
func writeExtension(t *testing.T) (ext, pub string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := signer.GenerateKeyFile(keyPath); err != nil {
		t.Fatal(err)
	}
	s, err := signer.OpenFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte("code"), extfile.MetadataPrefix...)
	body = append(body, block[:]...)
	h, _, err := extfile.HashBody(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	ext = filepath.Join(dir, "x.duckdb_extension")
	if err := os.WriteFile(ext, append(body, sig...), 0o644); err != nil {
		t.Fatal(err)
	}
	pemKey, _ := extfile.MarshalPublicKeyPEM(s.Public())
	pub = filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(pub, []byte(pemKey), 0o644); err != nil {
		t.Fatal(err)
	}
	return ext, pub
}

func TestCLI(t *testing.T) {
	ext, pub := writeExtension(t)
	other := filepath.Join(t.TempDir(), "other.pem")
	if err := signer.GenerateKeyFile(other); err != nil {
		t.Fatal(err)
	}
	s, _ := signer.OpenFile(other)
	otherPEM, _ := extfile.MarshalPublicKeyPEM(s.Public())
	otherPub := filepath.Join(t.TempDir(), "otherpub.pem")
	os.WriteFile(otherPub, []byte(otherPEM), 0o644)

	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"ext", "inspect", ext}, 0, "platform:          linux_amd64"},
		{[]string{"ext", "inspect", ext, "--key", pub}, 0, "signature:         sha256:"},
		{[]string{"ext", "verify", ext, "--key", pub}, 0, "sha256:"},
		{[]string{"ext", "verify", "--key", otherPub, "--key", pub, ext}, 0, "sha256:"},
		{[]string{"ext", "verify", ext, "--key", otherPub}, 1, ""},
		{[]string{"ext", "verify", ext}, 2, ""},
		{[]string{"ext", "inspect"}, 2, ""},
		{[]string{"nope"}, 2, ""},
		{[]string{"ext", "verify", ext, "--key", t.TempDir()}, 1, ""},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(c.args, &stdout, &stderr); code != c.code {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, code, c.code, stderr.String())
		}
		if !strings.Contains(stdout.String(), c.out) {
			t.Errorf("%v: output %q lacks %q", c.args, stdout.String(), c.out)
		}
	}
}
