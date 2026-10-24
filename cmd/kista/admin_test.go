package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// TestAdminCLI drives kista admin end to end on SQLite.

// publisherKeyLine is a publisher's public key (spec 0023's vectors).
const publisherKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIvmAKxjbOkVN7MhsYLLHONcPMZXvhAR+JPGbBHEmWgz ci ed25519"

func TestAdminCLI(t *testing.T) {
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0)) // the process's view follows this test's store
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.pem", "b.pem", "c.pem", "att.pem"} {
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
		"egress: { ca_file: "+caFile+", allow: [{ cidr: 127.0.0.1/32, ports: ["+strconv.Itoa(idpPort)+"] }] }\n"+
		"upstreams: { credentials: [{ name: src, tenants: [acme], prefixes: ['"+idp.URL+"/'], kind: token_file, token_file: "+
		filepath.Join(dir, "token")+" }] }\n"), 0o600)
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
	core := coreFile(t, "json", "parquet")
	run(1, "version", "add", "eb0d9df48e", "-kind", "dev") // without its core extensions (spec 0030)
	run(0, "version", "add", "eb0d9df48e", "-kind", "dev", "-core-extensions", core)
	run(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", core)
	run(0, "version", "core-extensions", "v2.0.0", "-core-extensions", coreFile(t, "json", "kista_test_core"))
	run(1, "version", "core-extensions", "v9.9.9", "-core-extensions", core) // not registered
	if got := run(0, "reserved", "list", "-name", "kista_test_core"); !strings.Contains(got, "kista_test_core") || !strings.Contains(got, "core") ||
		!strings.Contains(got, "version:v2.0.0@d54cbbe0123456789abcdef0123456789abcdef") {
		t.Fatalf("reserved list: %q", got)
	}
	run(0, "reserved", "add", "kista_cli_a", "kista_cli_b", "-kind", "community")
	run(1, "reserved", "add", "kista_cli_c", "-kind", "tenant")
	if got := run(0, "reserved", "list"); !strings.Contains(got, "kista_cli_b") || !strings.Contains(got, "legacy_parser") {
		t.Fatalf("reserved list: %q", got)
	}
	run(0, "reserved", "remove", "kista_cli_a")
	run(1, "reserved", "remove", "kista_test_core") // a version's name stays
	run(2, "reserved", "remove")
	// spec 0030 part 2: a name reserved in a tenant
	run(0, "tenant-reserved", "add", "acme", "acme_tool", "-holder", "enterest")
	run(1, "tenant-reserved", "add", "acme", "acme_tool") // a holder is required
	if got := run(0, "tenant-reserved", "list", "acme"); !strings.Contains(got, "acme_tool") || !strings.Contains(got, "enterest") {
		t.Fatalf("tenant-reserved list: %q", got)
	}
	run(0, "tenant-reserved", "remove", "acme", "acme_tool")
	run(1, "tenant-reserved", "remove", "acme", "acme_tool")
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
	if got := run(0, "key", "list", "acme/prod"); !strings.Contains(got, "retired") || !strings.Contains(got, "PROTECTION") ||
		!strings.Contains(got, "file:a.pem  file ") {
		t.Fatalf("key list: %q", got)
	}
	if n := strings.Count(run(0, "key", "events", "acme/prod"), "os:"); n != 5 {
		t.Fatalf("%d events", n)
	}
	if got := run(0, "key", "check", "-signer", "file:a.pem"); !strings.HasPrefix(got, "file:a.pem sha256:") || !strings.HasSuffix(got, " protection=file\n") {
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
	// spec 0031: the release set tresor's source; set, shown, cleared
	if got := run(0, "channel", "source", "acme/prod"); !strings.Contains(got, "tresor") || !strings.Contains(got, "tenant=1") {
		t.Fatalf("channel source: %q", got)
	}
	run(1, "channel", "source", "acme/prod", "tresor", "-set", "upstream:nope")
	run(2, "channel", "source", "acme/prod", "tresor", "-set", "tenant", "-clear")
	run(2, "channel", "source", "acme/prod", "-clear")
	if got := run(0, "channel", "source", "acme/prod", "tresor", "-set", "tenant"); !strings.Contains(got, "os:") {
		t.Fatalf("channel source -set: %q", got)
	}
	run(0, "channel", "source", "acme/prod", "tresor", "-clear")
	if got := run(0, "channel", "source", "acme/prod", "tresor"); !strings.Contains(got, "tresor  -") {
		t.Fatalf("channel source after -clear: %q", got)
	}
	run(0, "channel", "source", "acme/prod", "tresor", "-set", "tenant")
	// spec 0033: a rule's mode (the tenant's never auto), the catalogue, placing by hand
	run(1, "channel", "source", "acme/prod", "tresor", "-mode", "auto")
	if got := run(0, "channel", "source", "acme/prod", "tresor", "-mode", "manual"); !strings.Contains(got, "manual") {
		t.Fatalf("channel source -mode: %q", got)
	}
	run(1, "channel", "source", "acme/prod", "nothere", "-mode", "manual") // no source to keep
	if got := run(0, "catalogue", "list", "acme"); !strings.Contains(got, "tresor") || !strings.Contains(got, "prod=manual") {
		t.Fatalf("catalogue list: %q", got)
	}
	if got := run(0, "catalogue", "show", "acme", "tenant", "tresor"); !strings.Contains(got, "prod(active)") {
		t.Fatalf("catalogue show: %q", got)
	}
	run(1, "release", "place", "acme/prod", "-name", "tresor", "-source", "tenant", "-version", "1.0") // added unchecked
	run(1, "catalogue", "add", "acme", file, "-name", "tresor")                                        // not a binary: the checks refuse it
	run(2, "catalogue", "add", "acme", file)                                                           // no -name
	run(1, "catalogue", "purge", "acme", "nothere")
	run(1, "channel", "follow", "acme/prod", "tenant") // the tenant's own source is not followed
	if got := run(0, "channel", "follow", "acme/prod"); !strings.Contains(got, "SOURCE") {
		t.Fatalf("channel follow: %q", got)
	}
	run(1, "channel", "unfollow", "acme/prod", "upstream:nope")
	run(2, "channel", "unfollow", "acme/prod")
	run(2, "release", "place", "acme/prod", "-name", "tresor", "-source", "tenant")
	if got := run(0, "release", "list", "acme/prod"); !strings.Contains(got, rid) || !strings.Contains(got, "public") {
		t.Fatalf("release list: %q", got)
	}
	run(0, "release", "private", "acme/prod", rid)
	run(0, "release", "current", "acme/prod", rid)
	run(0, "key", "resign", "acme/prod")
	// attachments (spec 0011): a document about tresor's body, listed, shown, made readers-visible, removed
	bodyHash, _, err := extfile.HashBody(bytes.NewReader(body), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	sbom := filepath.Join(dir, "sbom.json")
	os.WriteFile(sbom, []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"type":"library"}]}`), 0o600)
	addAtt := func(want int, file, kind string) string {
		return strings.TrimSpace(run(want, "attachment", "add", "acme", "tresor", file, "-body-hash", bodyHash.String(), "-kind", kind,
			"-media-type", "application/vnd.cyclonedx+json", "-title", "SBOM"))
	}
	addAtt(1, sbom, "review")                                            // a CycloneDX document is not a review
	addAtt(1, filepath.Join(dir, "k.db"), "sbom")                        // not JSON
	run(2, "attachment", "add", "acme", "tresor", sbom, "-kind", "sbom") // no -body-hash
	aid := addAtt(0, sbom, "sbom")
	if got := run(0, "attachment", "list", "acme", "tresor"); !strings.Contains(got, aid) || !strings.Contains(got, `"components":1`) ||
		!strings.Contains(got, "administrators") {
		t.Fatalf("attachment list: %q", got)
	}
	if got := run(0, "attachment", "show", "acme", "tresor", aid, "-content"); !strings.Contains(got, "CycloneDX") {
		t.Fatalf("attachment content: %q", got)
	}
	run(0, "attachment", "readers", "acme", "tresor", aid)
	if got := run(0, "attachment", "show", "acme", "tresor", aid); !strings.Contains(got, "readers") {
		t.Fatalf("attachment show: %q", got)
	}
	// the feed (spec 0011 phase 1b): offering needs an active attestation key
	run(1, "attachment", "feedable", "acme", "tresor", aid)
	kid := strings.TrimSpace(run(0, "attestation-key", "add", "acme", "file:att.pem", "-active"))
	run(1, "attestation-key", "add", "acme", "file:a.pem") // a channel key already
	if got := run(0, "attestation-key", "list", "acme"); !strings.Contains(got, kid) || !strings.Contains(got, "active") {
		t.Fatalf("attestation-key list: %q", got)
	}
	run(1, "attestation-key", "retire", "acme", kid) // the active one
	run(0, "attachment", "feedable", "acme", "tresor", aid)
	run(0, "attachment", "unfeedable", "acme", "tresor", aid)
	feedURL := idp.URL + "/api/v1/tenants/upstream"
	fp := "sha256:" + strings.Repeat("ab", 32)
	run(1, "feed", "add", "acme", "src", "-url", feedURL, "-key", fp)                                                   // no credential
	run(1, "feed", "add", "acme", "src", "-url", "http://127.0.0.1/api/v1/tenants/x", "-credential", "src", "-key", fp) // not https
	run(0, "feed", "add", "acme", "src", "-url", feedURL, "-credential", "src", "-key", fp, "-kinds", "review,vulnerabilities", "-readers")
	if got := run(0, "feed", "list", "acme"); !strings.Contains(got, "src") || !strings.Contains(got, "review,vulnerabilities") ||
		!strings.Contains(got, "true") {
		t.Fatalf("feed list: %q", got)
	}
	run(0, "feed", "pause", "acme", "src")
	run(1, "feed", "sync", "acme", "src") // paused
	run(0, "feed", "resume", "acme", "src")
	run(0, "feed", "set", "acme", "src", "-readers=false", "-kinds", "-")
	if got := run(0, "feed", "show", "acme", "src"); !strings.Contains(got, "readers false") || !strings.Contains(got, "state active") {
		t.Fatalf("feed show: %q", got)
	}
	run(0, "feed", "sync", "acme", "src")
	run(0, "feed", "remove", "acme", "src", "-documents")
	run(1, "feed", "show", "acme", "src")
	run(0, "attachment", "remove", "acme", "tresor", aid)
	run(1, "attachment", "show", "acme", "tresor", aid)
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
	// spec 0016 phase 2: a yanked release purged; its slot stays taken
	run(1, "release", "purge", "acme/prod", rid) // gc.interval 0: replicas may not all keep purged slots
	run(0, "release", "purge", "acme/prod", rid, "-force")
	run(1, "release", "purge", "acme/prod", rid, "-force")
	run(0, "block", "remove", "acme", f.Hash.String())
	run(1, "release", "add", "acme/prod", file, "-name", "tresor", "-unchecked") // unblocked: the purged slot refuses it
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
	// lazy mirrors (spec 0032)
	run(0, "upstream", "fetch", "acme", "core", "lazy")
	run(1, "upstream", "fetch", "acme", "core", "sometimes")
	run(0, "upstream", "extension", "add", "acme", "core", "json", "-eager")
	if got := run(0, "upstream", "show", "acme", "core"); !strings.Contains(got, "fetch: lazy") || !strings.Contains(got, "extension: json  (eager)") {
		t.Fatalf("a lazy upstream: %q", got)
	}
	if got := run(0, "upstream", "list", "acme"); !strings.Contains(got, "mirror (lazy)") {
		t.Fatalf("upstream list: %q", got)
	}
	if got := run(0, "upstream", "cells", "acme", "core", "-outcome", "not_asked"); !strings.Contains(got, "parquet") || strings.Contains(got, "json") {
		t.Fatalf("not_asked: %q", got)
	}
	run(1, "upstream", "add", "acme", "lazypt", "-kind", "duckdb-community", "-channel", "staging", "-platforms", "linux_amd64",
		"-extensions", "h3", "-mode", "pull-through", "-fetch", "lazy")
	run(0, "shadow", "list", "acme")
	// events (spec 0010)
	if got := run(0, "events", "list", "acme", "-subject", "upstream:co"); !strings.Contains(got, "upstream.add") || strings.Contains(got, "block.add") {
		t.Fatalf("events list: %q", got)
	}
	if got := run(0, "events", "list", "-server", "-format", "jsonl", "-limit", "1"); !strings.Contains(got, `"kind":"`) {
		t.Fatalf("server events: %q", got)
	}
	run(2, "events", "list")
	run(2, "events", "list", "-server", "acme")
	run(1, "events", "list", "nope")
	run(0, "upstream", "add", "acme", "pull", "-kind", "duckdb-community", "-channel", "staging", "-platforms", "linux_amd64",
		"-extensions", "*", "-mode", "pull-through")
	run(1, "upstream", "sync", "acme", "pull")
	run(1, "shadow", "remove", "acme", "httpfs")
	run(0, "upstream", "remove", "acme", "core")
	run(1, "upstream", "show", "acme", "core")
	run(1, "block", "remove", "acme", f.Hash.String())
	// issuers, audiences, grants (an explicit JWKS URI: no discovery from the test)
	run(0, "issuer", "add", "acme", "-name", "corp", "-url", "https://login.example/t1/v2.0",
		"-jwks-uri", jwks, "-require", "tid=t1", "-roles-claim", `["https://x/roles"]`, "-alg", "RS256", "-alg", "RS256")
	// the console's client at an issuer (spec 0015)
	run(0, "issuer", "console", "acme", "corp", "-client-id", "kista-console", "-scopes", "openid,offline_access")
	run(1, "issuer", "console", "acme", "corp", "-client-id", "x", "-scopes", "profile") // no openid
	run(2, "issuer", "console", "acme", "corp", "-remove", "-client-id", "x")
	run(0, "issuer", "console", "acme", "corp", "-remove")
	run(1, "issuer", "console", "acme", "corp", "-remove") // none left
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
	// publishers (spec 0008) and their keys (spec 0023), publish only
	run(0, "publisher", "add", "acme", "acl-ci")
	run(1, "publisher", "add", "acme", "acl-ci")
	run(1, "grant", "add", "acme", "-principal", "publisher:acl-ci", "-verb", "install")
	run(0, "grant", "add", "acme", "-principal", "publisher:acl-ci", "-verb", "publish", "-channel", "prod", "-extension", "acl")
	if got := run(0, "grant", "list", "acme"); !strings.Contains(got, "publisher:acl-ci") {
		t.Fatalf("grant list: %q", got)
	}
	pub := filepath.Join(t.TempDir(), "ci.pub")
	if err := os.WriteFile(pub, []byte(publisherKeyLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyID := strings.TrimSpace(run(0, "publisher", "key", "add", "acme", "acl-ci", pub, "-expires", "90d"))
	run(1, "publisher", "key", "add", "acme", "acl-ci", pub) // the same key
	run(2, "publisher", "key", "add", "acme", "acl-ci")
	run(2, "publisher", "key", "add", "acme", "acl-ci", pub, "-expires", "400d")
	run(1, "publisher", "key", "add", "acme", "acl-ci", pub, "-expires", "2001-01-01T00:00:00Z")
	run(2, "publisher", "key", "list", "acme", "acl-ci", "-expires", "9d")
	run(2, "publisher", "key", "add", "acme", "acl-ci", pub, "-expires", "soon")
	run(1, "publisher", "key", "add", "acme", "acl-ci", filepath.Join(t.TempDir(), "missing.pub"))
	got := run(0, "publisher", "key", "list", "acme", "acl-ci")
	if !strings.Contains(got, keyID) || !strings.Contains(got, "ssh-ed25519") || !strings.Contains(got, "ci ed25519") {
		t.Fatalf("key list: %q", got)
	}
	if got := run(0, "publisher", "list", "acme"); !strings.Contains(got, keyID) {
		t.Fatalf("publisher list: %q", got)
	}
	run(0, "publisher", "key", "remove", "acme", "acl-ci", keyID)
	run(1, "publisher", "key", "remove", "acme", "acl-ci", keyID)
	if got := run(0, "publisher", "key", "list", "acme", "acl-ci"); strings.Contains(got, keyID) {
		t.Fatalf("a removed key: %q", got)
	}
	run(0, "publisher", "remove", "acme", "acl-ci")
	if got := run(0, "grant", "list", "acme"); strings.Contains(got, "publisher:acl-ci") {
		t.Fatalf("a removed publisher's grant: %q", got)
	}
	if got := run(0, "grant", "list", "acme"); !strings.Contains(got, "subject:corp|alice") || !strings.Contains(got, "issuer:corp") {
		t.Fatalf("grant list: %q", got)
	}
	run(0, "grant", "remove", "acme", gid)
	// extension groups (spec 0017): members in batches, grants on a group, removal refused while granted
	run(0, "extension-group", "add", "acme", "platform", "-description", "hugr")
	run(1, "extension-group", "add", "acme", "platform")
	run(1, "extension-group", "add", "acme", "Platform")
	var many []string
	for i := range 150 {
		many = append(many, fmt.Sprintf("x%03d", i))
	}
	run(0, "extension-group", "members", "acme", "platform", "-add", "tresor,acl,"+strings.Join(many, ","))
	run(1, "extension-group", "members", "acme", "platform", "-add", "ok_name,Bad") // checked before any batch
	run(0, "extension-group", "members", "acme", "platform", "-remove", strings.Join(many, ","))
	if got := run(0, "extension-group", "members", "acme", "platform"); !strings.Contains(got, "tresor") || strings.Contains(got, "x001") ||
		strings.Contains(got, "ok_name") {
		t.Fatalf("members: %q", got)
	}
	run(0, "grant", "add", "acme", "-principal", "role:corp|team", "-verb", "install", "-extension-group", "platform")
	run(1, "grant", "add", "acme", "-principal", "role:corp|team", "-verb", "install", "-extension-group", "platform", "-extension", "acl")
	run(1, "grant", "add", "acme", "-principal", "role:corp|team", "-verb", "install", "-extension-group", "nope")
	if got := run(0, "grant", "list", "acme", "-extension", "acl"); !strings.Contains(got, "extension-group:platform") || strings.Contains(got, "subject:corp|alice") {
		t.Fatalf("grants reaching acl: %q", got)
	}
	if got := run(0, "grant", "list", "acme", "-extension-group", "platform"); !strings.Contains(got, "role:corp|team") || strings.Contains(got, "issuer:corp") {
		t.Fatalf("grants on platform: %q", got)
	}
	run(1, "grant", "list", "acme", "-extension", "Bad")
	if got := run(0, "extension-group", "show", "acme", "platform"); !strings.Contains(got, "members (2)") || !strings.Contains(got, "role:corp|team") {
		t.Fatalf("show: %q", got)
	}
	if got := run(0, "extension-group", "list", "acme"); !strings.Contains(got, "platform") || !strings.Contains(got, "hugr") {
		t.Fatalf("list: %q", got)
	}
	run(0, "extension-group", "description", "acme", "platform", "the hugr platform")
	run(1, "extension-group", "remove", "acme", "platform") // a grant names it
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
	run(0, "extension-group", "remove", "acme", "platform") // its grant went with the issuer
	if got := run(0, "extension-group", "list", "acme"); strings.Contains(got, "platform") {
		t.Fatalf("a removed group: %q", got)
	}
	run(0, "backup", filepath.Join(dir, "backup.db"))
	if got := run(0, "blob", "check"); !strings.Contains(got, "domain default ok") {
		t.Fatalf("blob check: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "kista-domain.json")); err != nil {
		t.Fatalf("blob check wrote no marker beside the dev database: %v", err)
	}
	// storage GC (spec 0016): a dry run by default; -apply needs -force while gc.interval is 0
	if got := run(0, "blob", "gc"); !strings.Contains(got, "a dry run") {
		t.Fatalf("blob gc: %q", got)
	}
	run(1, "blob", "gc", "-apply")
	run(0, "blob", "gc", "-apply", "-force")
	run(1, "blob", "gc", "-domain", "nowhere")
	// verifying stored bodies (spec 0018): clean, then an object gone, found and marked
	if got := run(0, "blob", "verify", "-full", "-domain", "default"); !strings.Contains(got, "DOMAIN") || !strings.Contains(got, "default") {
		t.Fatalf("blob verify: %q", got)
	}
	streams, err := filepath.Glob(filepath.Join(dir, "blobs", "streams", "*"))
	if err != nil || len(streams) == 0 {
		t.Fatalf("no stored streams: %v", err)
	}
	if err := os.Remove(streams[0]); err != nil {
		t.Fatal(err)
	}
	if got := run(1, "blob", "verify", "-full", "-mark"); !strings.Contains(got, "\tmissing\t") || !strings.Contains(got, "(marked)") {
		t.Fatalf("blob verify after a loss: %q", got)
	}
	if got := run(1, "blob", "verify"); !strings.Contains(got, "(marked before: served 503)") {
		t.Fatalf("a second pass: %q", got)
	}
	run(2, "blob", "verify", "-mark")             // marks come from a full read only
	run(2, "blob", "verify", "-concurrency", "0") // 1 to 32
	if got := run(0, "events", "list", "-server", "-kind", "storage.verify"); !strings.Contains(got, "storage.verify") {
		t.Fatalf("no storage.verify event: %q", got)
	}
	run(1, "blob", "verify", "-domain", "nowhere")
	run(2, "blob", "verify", "extra")
	run(1, "key", "add", "acme/prod", "-signer", "file:../escape.pem")
	run(2, "nonsense")
	run(2, "key", "add", "acme")
}

// Spec 0019 from the CLI: a suspended tenant is deleted with its name given twice, its rows reaped
// here with -force, and its tombstone listed and shown.
func TestAdminTenantDelete(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"signers: { file_dir: "+keys+" }\ntenants: { min_suspended: 0s }\n"), 0o600)
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	run(0, "migrate")
	run(0, "tenant", "create", "gone")
	run(0, "channel", "create", "gone/prod", "-kind", "signed")
	run(0, "key", "add", "gone/prod", "-signer", "file:a.pem", "-active")
	if got := run(1, "tenant", "delete", "gone", "-confirm", "gone"); !strings.Contains(got, "suspend it first") {
		t.Errorf("an active tenant: %q", got)
	}
	run(0, "tenant", "suspend", "gone")
	run(2, "tenant", "delete", "gone")
	run(2, "tenant", "delete", "gone", "-confirm", "other")
	got := run(0, "tenant", "delete", "gone", "-confirm", "gone")
	if !strings.Contains(got, "being deleted") || !strings.Contains(got, "file:a.pem") || !strings.Contains(got, "gc.interval is 0") {
		t.Errorf("delete: %q", got)
	}
	if got := run(1, "channel", "list", "gone"); !strings.Contains(got, "being deleted") {
		t.Errorf("a deleting tenant's channels: %q", got)
	}
	if got := run(0, "tenant", "show", "gone"); !strings.Contains(got, "deleting") || !strings.Contains(got, "file:a.pem") {
		t.Errorf("show: %q", got)
	}
	if got := run(1, "tenant", "reap", "gone"); !strings.Contains(got, "settle") {
		t.Errorf("reap before the settle period: %q", got)
	}
	if got := run(0, "tenant", "reap", "gone", "-force"); !strings.Contains(got, "tenant gone deleted") {
		t.Errorf("reap: %q", got)
	}
	if got := run(0, "tenant", "list"); strings.Contains(got, "gone") {
		t.Errorf("a tombstone in the default list: %q", got)
	}
	if got := run(0, "tenant", "list", "-state", "deleted"); !strings.Contains(got, "gone") {
		t.Errorf("list -state deleted: %q", got)
	}
	if got := run(0, "events", "list", "gone"); !strings.Contains(got, "tenant.purge") || !strings.Contains(got, "tenant.delete") {
		t.Errorf("a tombstone's events: %q", got)
	}
	if got := run(1, "tenant", "create", "gone"); !strings.Contains(got, "never reused") {
		t.Errorf("the name again: %q", got)
	}
}
