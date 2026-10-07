package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// key is a test signing key, generated per run (or taken from a store-managed signer).
type key struct {
	signer signer.Signer
	pem    string // SPKI PEM
	b64    string // base64 SPKI DER
	fp     string
}

func newKey(t *testing.T) *key {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := signer.GenerateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	s, err := signer.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return keyFrom(t, s)
}

// keyFrom wraps any signer.
func keyFrom(t *testing.T, s signer.Signer) *key {
	t.Helper()
	pemKey, err := extfile.MarshalPublicKeyPEM(s.Public())
	if err != nil {
		t.Fatal(err)
	}
	b64, err := extfile.MarshalPublicKeyBase64(s.Public())
	if err != nil {
		t.Fatal(err)
	}
	return &key{signer: s, pem: pemKey, b64: b64, fp: extfile.Fingerprint(s.Public())}
}

// repo is a signed repository tree on disk, in DuckDB's layouts.
type repo struct {
	t   *testing.T
	b   *build
	dir string
}

func newRepo(t *testing.T, b *build) *repo {
	return &repo{t: t, b: b, dir: t.TempDir()}
}

// signed returns an extension of the build re-signed with k: body unchanged, new signature.
func (r *repo) signed(name string, k *key) (*extfile.File, []byte) {
	r.t.Helper()
	f, err := os.Open(r.b.extensions[name])
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { f.Close() })
	fi, _ := f.Stat()
	ext, err := extfile.Open(f, fi.Size(), 1<<30)
	if err != nil {
		r.t.Fatal(err)
	}
	sig, err := k.signer.Sign(context.Background(), ext.Hash)
	if err != nil {
		r.t.Fatal(err)
	}
	return ext, sig
}

// gzipAssembled returns the .gz form kista serves: the precompressed body plus a stored signature
// block (extfile.WriteGzip).
func (r *repo) gzipAssembled(ext *extfile.File, sig []byte) []byte {
	r.t.Helper()
	var stream bytes.Buffer
	pre, err := extfile.Precompress(ext.Body(), &stream, 1<<30)
	if err != nil {
		r.t.Fatal(err)
	}
	var gz bytes.Buffer
	if err := extfile.WriteGzip(&gz, pre, bytes.NewReader(stream.Bytes()), ext.Hash, sig); err != nil {
		r.t.Fatal(err)
	}
	return gz.Bytes()
}

func (r *repo) write(rel string, data []byte) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// add places an extension signed with k at the flat path, both as the plain file and as the
// assembled .gz. With version != "" it goes to the versioned path instead.
func (r *repo) add(name string, k *key, version string) {
	r.t.Helper()
	ext, sig := r.signed(name, k)
	var plain bytes.Buffer
	if err := ext.WriteSigned(&plain, sig); err != nil {
		r.t.Fatal(err)
	}
	rel := r.b.versionDir + "/" + r.b.platform + "/" + name + ".duckdb_extension"
	if version != "" {
		rel = name + "/" + version + "/" + rel
	}
	r.write(rel, plain.Bytes())
	r.write(rel+".gz", r.gzipAssembled(ext, sig))
}

// wellKnown writes .well-known/duckdb-extension-repo.json with the keys' PEM.
func (r *repo) wellKnown(keys ...*key) {
	r.t.Helper()
	var doc struct {
		SignatureKeys []string `json:"signature_keys"`
	}
	for _, k := range keys {
		doc.SignatureKeys = append(doc.SignatureKeys, k.pem)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		r.t.Fatal(err)
	}
	r.write(".well-known/duckdb-extension-repo.json", data)
}
