package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// TestAdminCLI drives kista admin end to end on SQLite.
func TestAdminCLI(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.pem", "b.pem", "c.pem"} {
		if err := signer.GenerateKeyFile(filepath.Join(keys, k)); err != nil {
			t.Fatal(err)
		}
	}
	// an IdP on loopback TLS serving a JWKS, reached through egress (allowlisted, its CA trusted)
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(idpKey.N.Bytes()), "e": "AQAB"}}})
	}))
	defer idp.Close()
	caFile := filepath.Join(dir, "idp-ca.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0o600)
	idpPort := idp.Listener.Addr().(*net.TCPAddr).Port
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"rotation: { min_trusted: 0s, min_demoted: 0s }\nsigners: { file_dir: "+keys+" }\n"+
		"serve: { public_url: 'https://kista.example' }\n"+
		"egress: { ca_file: "+caFile+", allow: [{ cidr: 127.0.0.1/32, ports: ["+strconv.Itoa(idpPort)+"] }] }\n"), 0o600)
	jwks := idp.URL + "/keys"

	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, errw.String())
		}
		return out.String()
	}
	run(0, "migrate")
	run(0, "check")
	run(0, "tenant", "create", "acme", "-display-name", "Acme")
	run(1, "tenant", "create", "acme-cn", "-domain", "cn") // not configured
	if got := run(0, "tenant", "list"); !strings.Contains(got, "acme") || !strings.Contains(got, "default") || strings.Contains(got, "acme-cn") {
		t.Fatalf("tenant list: %q", got)
	}
	run(0, "version", "add", "eb0d9df48e", "-kind", "dev")
	run(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6")
	run(0, "version", "c-api", "v2.0.0", "-c-api", "v2.0.0")
	run(1, "version", "c-api", "v2.0.0", "-c-api", "v2.1.0") // a major is never changed
	if got := run(0, "version", "list"); !strings.Contains(got, "v1.5.6 v2.0.0") {
		t.Fatalf("version list: %q", got)
	}
	run(0, "channel", "create", "acme/prod", "-kind", "signed")
	if got := run(0, "channel", "versions", "acme/prod", "-add", "eb0d9df48e", "-add", "v2.0.0"); !strings.Contains(got, "v2.0.0") {
		t.Fatalf("versions %q", got)
	}
	a := strings.Fields(run(0, "key", "add", "acme/prod", "-signer", "file:a.pem", "-active"))
	b := strings.Fields(run(0, "key", "add", "acme/prod", "-signer", "file:b.pem"))
	var doc struct {
		SignatureKeys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal([]byte(run(0, "wellknown", "acme/prod")), &doc); err != nil || len(doc.SignatureKeys) != 2 {
		t.Fatalf("wellknown: %v %d", err, len(doc.SignatureKeys))
	}
	run(0, "key", "activate", "acme/prod", b[1])
	run(1, "key", "retire", "acme/prod", b[0]) // the active key
	run(0, "key", "retire", "acme/prod", a[0])
	if !strings.Contains(run(0, "key", "list", "acme/prod"), "retired") {
		t.Fatal("key list")
	}
	if n := strings.Count(run(0, "key", "events", "acme/prod"), "os:"); n != 5 {
		t.Fatalf("%d events", n)
	}
	if got := run(0, "key", "check", "-signer", "file:a.pem"); !strings.HasPrefix(got, "file:a.pem sha256:") {
		t.Fatalf("key check: %q", got)
	}
	run(1, "key", "check", "-signer", "nope:x")
	// releases: add from a file, list, change; key resign after an activation
	file := filepath.Join(dir, "tresor.duckdb_extension")
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(bytes.Repeat([]byte{1}, 5000), extfile.MetadataPrefix...), block[:]...)
	if err := os.WriteFile(file, append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	run(1, "release", "add", "acme/prod", file, "-name", "tresor") // not a binary: spec 0008's checks refuse it
	rid := strings.TrimSpace(run(0, "release", "add", "acme/prod", file, "-name", "tresor", "-unchecked"))
	if again := strings.TrimSpace(run(0, "release", "add", "acme/prod", file, "-name", "tresor", "-unchecked")); again != rid {
		t.Fatalf("re-add: %q vs %q", again, rid)
	}
	run(1, "release", "add", "acme/prod", file, "-name", "postgres", "-unchecked") // an alias DuckDB never requests
	run(2, "release", "add", "acme/prod", file)                                    // no -name
	if got := run(0, "release", "list", "acme/prod"); !strings.Contains(got, rid) || !strings.Contains(got, "public") {
		t.Fatalf("release list: %q", got)
	}
	run(0, "release", "private", "acme/prod", rid)
	run(0, "release", "current", "acme/prod", rid)
	run(0, "key", "resign", "acme/prod")
	run(1, "release", "promote", "acme/prod", "-name", "tresor", "-from", "prod", "-version", "1.0") // to itself
	run(0, "release", "yank", "acme/prod", rid)
	run(1, "release", "yank", "acme/prod", rid)
	// blocks
	f, err := extfile.Open(bytes.NewReader(append(body, make([]byte, extfile.SignatureSize)...)), int64(len(body)+extfile.SignatureSize), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	run(2, "block", "add", "acme", f.Hash.String()) // no -reason
	run(0, "block", "add", "acme", f.Hash.String(), "-reason", "CVE-1")
	if got := run(0, "block", "list", "acme"); !strings.Contains(got, f.Hash.String()) || !strings.Contains(got, "CVE-1") {
		t.Fatalf("block list: %q", got)
	}
	run(0, "channel", "create", "acme/staging", "-kind", "signed")
	run(0, "channel", "versions", "acme/staging", "-add", "v2.0.0")
	run(0, "key", "add", "acme/staging", "-signer", "file:c.pem", "-active")
	run(1, "release", "add", "acme/staging", file, "-name", "tresor", "-unchecked")                   // blocked (a fresh slot)
	run(1, "release", "promote", "acme/staging", "-name", "tresor", "-from", "prod", "-release", rid) // unchecked and yanked
	run(0, "block", "remove", "acme", f.Hash.String())
	// upstreams (spec 0009)
	run(0, "upstream", "add", "acme", "core", "-kind", "duckdb-core", "-channel", "staging", "-platforms", "linux_amd64,osx_arm64",
		"-extensions", "json,parquet")
	run(1, "upstream", "add", "acme", "bad", "-kind", "duckdb-core", "-channel", "staging", "-platforms", "linux_amd64", "-extensions", "postgres")
	run(1, "upstream", "add", "acme", "bad", "-kind", "repository", "-channel", "staging", "-platforms", "linux_amd64")
	run(0, "upstream", "extension", "add", "acme", "core", "httpfs", "-versions", "abc,def")
	run(2, "upstream", "platform", "add", "acme", "core", "linux_arm64", "-versions", "x")
	run(0, "upstream", "platform", "remove", "acme", "core", "osx_arm64")
	run(1, "upstream", "key", "add", "acme", "core", "sha256:"+strings.Repeat("a", 64))
	if got := run(0, "upstream", "show", "acme", "core"); !strings.Contains(got, "extension: httpfs abc,def") || strings.Contains(got, "osx_arm64") {
		t.Fatalf("upstream show: %q", got)
	}
	run(0, "upstream", "sync", "acme", "core", "-dry-run")
	if got := run(0, "upstream", "show", "acme", "core"); !strings.Contains(got, "run: requested") {
		t.Fatalf("a requested run: %q", got)
	}
	run(0, "upstream", "pause", "acme", "core")
	run(1, "upstream", "sync", "acme", "core")
	if got := run(0, "upstream", "list", "acme"); !strings.Contains(got, "paused") {
		t.Fatalf("upstream list: %q", got)
	}
	run(0, "upstream", "cells", "acme", "core")
	run(0, "shadow", "list", "acme")
	run(1, "shadow", "remove", "acme", "httpfs")
	run(0, "upstream", "remove", "acme", "core")
	run(1, "upstream", "show", "acme", "core")
	run(1, "block", "remove", "acme", f.Hash.String())
	// issuers, audiences, grants (an explicit JWKS URI: no discovery from the test)
	run(0, "issuer", "add", "acme", "-name", "corp", "-url", "https://login.example/t1/v2.0",
		"-jwks-uri", jwks, "-require", "tid=t1", "-roles-claim", `["https://x/roles"]`, "-alg", "RS256", "-alg", "RS256")
	run(1, "issuer", "add", "acme", "-name", "corp2", "-url", "http://login.example", "-jwks-uri", jwks)
	run(1, "issuer", "add", "acme", "-name", "corp3", "-url", "https://login.example/b", "-jwks-uri", jwks, "-alg", "HS256")
	run(1, "issuer", "add", "acme", "-name", "corp4", "-url", "https://login.example/c", "-jwks-uri", "https://127.0.0.2:1/keys") // unreachable JWKS
	run(2, "issuer", "add", "acme", "-name", "corp5", "-url", "https://login.example/d", "-jwks-uri", jwks, "-roles-claim", "a..b")
	if got := run(0, "issuer", "list", "acme"); !strings.Contains(got, "corp") || !strings.Contains(got, "tid=t1") {
		t.Fatalf("issuer list: %q", got)
	}
	gid := strings.TrimSpace(run(0, "grant", "add", "acme", "-principal", "subject:corp|alice", "-verb", "install", "-channel", "prod", "-extension", "tresor"))
	run(1, "grant", "add", "acme", "-principal", "subject:corp|alice", "-verb", "install", "-channel", "prod", "-extension", "tresor") // the same grant
	run(0, "grant", "add", "acme", "-principal", "issuer:corp", "-verb", "install")
	run(1, "grant", "add", "acme", "-principal", "subject:nope|alice", "-verb", "install")
	run(1, "grant", "add", "acme", "-principal", "server:corp|alice", "-verb", "install")
	run(1, "grant", "add", "acme", "-principal", "subject:corp|alice", "-verb", "read")
	run(1, "grant", "add", "acme", "-principal", "issuer:corp", "-verb", "publish") // never issuer-wide (spec 0008)
	// publishers (spec 0008): a GitHub credential, publish only
	run(0, "publisher", "add", "acme", "acl-ci")
	run(1, "publisher", "add", "acme", "acl-ci")
	cred := strings.TrimSpace(run(0, "publisher", "github", "add", "acme", "acl-ci", "-owner-id", "10", "-repository-id", "20",
		"-workflow", "hugr-lab/duckdb-acl/.github/workflows/release.yml", "-ref", "refs/tags/v*"))
	run(1, "publisher", "github", "add", "acme", "acl-ci", "-owner-id", "hugr-lab", "-repository-id", "20", "-workflow", "a/b/c.yml")
	if got := run(0, "publisher", "list", "acme"); !strings.Contains(got, cred) || !strings.Contains(got, "refs/tags/v*") {
		t.Fatalf("publisher list: %q", got)
	}
	run(1, "grant", "add", "acme", "-principal", "publisher:acl-ci", "-verb", "install")
	run(0, "grant", "add", "acme", "-principal", "publisher:acl-ci", "-verb", "publish", "-channel", "prod", "-extension", "acl")
	if got := run(0, "grant", "list", "acme"); !strings.Contains(got, "publisher:acl-ci") {
		t.Fatalf("grant list: %q", got)
	}
	// API keys (spec 0008 phase 2): shown once, listed by prefix, an expiry required
	key := strings.TrimSpace(run(0, "publisher", "key", "add", "acme", "acl-ci", "-expires", "90d"))
	if !strings.HasPrefix(key, "kista_") {
		t.Fatalf("key add: %q", key)
	}
	run(2, "publisher", "key", "add", "acme", "acl-ci")
	run(2, "publisher", "key", "add", "acme", "acl-ci", "-expires", "400d")
	run(1, "publisher", "key", "add", "acme", "acl-ci", "-expires", "2001-01-01T00:00:00Z")
	run(2, "publisher", "key", "list", "acme", "acl-ci", "-expires", "9d")
	run(2, "publisher", "key", "add", "acme", "acl-ci", "-expires", "soon")
	run(0, "publisher", "key", "add", "acme", "acl-ci", "-expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	got := run(0, "publisher", "key", "list", "acme", "acl-ci")
	if strings.Contains(got, key) || !strings.Contains(got, key[6:14]) {
		t.Fatalf("key list: %q", got)
	}
	var keyID string
	for _, l := range strings.Split(got, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == key[6:14] {
			keyID = f[0]
		}
	}
	run(0, "publisher", "key", "remove", "acme", "acl-ci", keyID)
	run(1, "publisher", "key", "remove", "acme", "acl-ci", keyID)
	if got := run(0, "publisher", "key", "list", "acme", "acl-ci"); strings.Contains(got, key[6:14]) {
		t.Fatalf("a removed key: %q", got)
	}
	run(0, "publisher", "github", "remove", "acme", "acl-ci", cred)
	run(0, "publisher", "remove", "acme", "acl-ci")
	if got := run(0, "grant", "list", "acme"); strings.Contains(got, "publisher:acl-ci") {
		t.Fatalf("a removed publisher's grant: %q", got)
	}
	if got := run(0, "grant", "list", "acme"); !strings.Contains(got, "subject:corp|alice") || !strings.Contains(got, "issuer:corp") {
		t.Fatalf("grant list: %q", got)
	}
	run(0, "grant", "remove", "acme", gid)
	run(0, "tenant", "audience", "add", "acme", "api://kista-acme")
	run(1, "tenant", "audience", "add", "acme", "api://kista-acme")
	run(1, "tenant", "audience", "add", "acme", "https://kista.example/beta") // another tenant's canonical audience
	run(1, "tenant", "audience", "add", "acme", "https://kista.example")      // the server's
	if got := run(0, "tenant", "audience", "list", "acme"); !strings.Contains(got, "api://kista-acme") {
		t.Fatalf("audience list: %q", got)
	}
	run(0, "issuer", "remove", "acme", "corp")
	if got := run(0, "grant", "list", "acme"); strings.Contains(got, "corp") {
		t.Fatalf("grants survived their issuer: %q", got)
	}
	run(0, "backup", filepath.Join(dir, "backup.db"))
	if got := run(0, "blob", "check"); !strings.Contains(got, "domain default ok") {
		t.Fatalf("blob check: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "kista-domain.json")); err != nil {
		t.Fatalf("blob check wrote no marker beside the dev database: %v", err)
	}
	run(1, "key", "add", "acme/prod", "-signer", "file:../escape.pem")
	run(2, "nonsense")
	run(2, "key", "add", "acme")
}
