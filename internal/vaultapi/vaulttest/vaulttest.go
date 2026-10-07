// Package vaulttest prepares real Vault and OpenBao servers for tests. A server is named by
// KISTA_TEST_VAULT / KISTA_TEST_OPENBAO (an address) with the root token in KISTA_TEST_VAULT_TOKEN /
// KISTA_TEST_OPENBAO_TOKEN; with KISTA_TEST_REQUIRE_VAULT=1 a missing server fails instead of
// skipping. The root token only sets things up: kista authenticates with a least-privilege token.
package vaulttest

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Server is a reachable Vault or OpenBao.
type Server struct {
	Name    string // vault | openbao
	Address string
	root    string
}

// Servers returns the configured servers.
func Servers(t testing.TB) []Server {
	t.Helper()
	var out []Server
	for _, s := range []struct{ name, addr, tok string }{
		{"vault", "KISTA_TEST_VAULT", "KISTA_TEST_VAULT_TOKEN"},
		{"openbao", "KISTA_TEST_OPENBAO", "KISTA_TEST_OPENBAO_TOKEN"},
	} {
		addr := os.Getenv(s.addr)
		if addr == "" {
			if os.Getenv("KISTA_TEST_REQUIRE_VAULT") == "1" {
				t.Fatalf("%s is not set and KISTA_TEST_REQUIRE_VAULT=1", s.addr)
			}
			continue
		}
		out = append(out, Server{Name: s.name, Address: addr, root: os.Getenv(s.tok)})
	}
	return out
}

// Root sends a request with the root token (setup only).
func (s Server) Root(t testing.TB, method, path string, body any) map[string]any {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.Address+"/v1/"+path, buf)
	if err != nil {
		t.Fatalf("vaulttest: %v", err)
	}
	req.Header.Set("X-Vault-Token", s.root)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}

func random() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Transit is a Transit mount with one rsa-2048 key and a least-privilege token.
type Transit struct {
	Server
	Mount     string
	Key       string
	TokenFile string // a file holding a token that may read the key and sign with it, nothing else
	Policy    string
}

// NewTransit enables a fresh Transit mount, creates an rsa-2048 key named "ext-<random>" and a token
// with read and sign rights only, written to a 0600 file.
func (s Server) NewTransit(t testing.TB) *Transit {
	t.Helper()
	mount := "kt" + random()
	key := "ext-" + random()
	s.Root(t, http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "transit"})
	t.Cleanup(func() { s.Root(t, http.MethodDelete, "sys/mounts/"+mount, nil) })
	s.Root(t, http.MethodPost, mount+"/keys/"+key, map[string]any{"type": "rsa-2048"})
	policy := "kista-" + mount
	rules := fmt.Sprintf("path %q { capabilities = [\"read\"] }\npath %q { capabilities = [\"update\"] }\n"+
		"path \"sys/capabilities-self\" { capabilities = [\"update\"] }\n",
		mount+"/keys/"+key, mount+"/sign/"+key+"/sha2-256")
	s.Root(t, http.MethodPut, "sys/policies/acl/"+policy, map[string]any{"policy": rules})
	tr := &Transit{Server: s, Mount: mount, Key: key, Policy: policy}
	tr.TokenFile = tr.WriteToken(t, policy)
	return tr
}

// WriteToken creates a token with the given policies and writes it to a 0600 file.
func (tr *Transit) WriteToken(t testing.TB, policies ...string) string {
	t.Helper()
	out := tr.Root(t, http.MethodPost, "auth/token/create", map[string]any{"policies": policies, "ttl": "1h", "no_default_policy": true})
	auth, _ := out["auth"].(map[string]any)
	tok, _ := auth["client_token"].(string)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// JWTLogin enables a jwt auth mount trusting a fresh RSA key, with a role bound to the transit
// policy, and writes a signed JWT for it to a 0600 file. It returns the auth mount, role and file.
func (tr *Transit) JWTLogin(t testing.TB) (mount, role, file string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	mount, role = "jwt"+random(), "kista"
	tr.Root(t, http.MethodPost, "sys/auth/"+mount, map[string]any{"type": "jwt"})
	t.Cleanup(func() { tr.Root(t, http.MethodDelete, "sys/auth/"+mount, nil) })
	tr.Root(t, http.MethodPost, "auth/"+mount+"/config", map[string]any{"jwt_validation_pubkeys": []string{pemKey}})
	tr.Root(t, http.MethodPost, "auth/"+mount+"/role/"+role, map[string]any{
		"role_type": "jwt", "user_claim": "sub", "bound_audiences": []string{"kista-vault"},
		"policies": []string{tr.Policy}, "token_ttl": "1h",
	})
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"sub": "kista", "aud": "kista-vault", "iat": now, "nbf": now - 5, "exp": now + 3600})
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(file, []byte(signing+"."+base64.RawURLEncoding.EncodeToString(sig)), 0o600); err != nil {
		t.Fatal(err)
	}
	return mount, role, file
}

// SetConfig changes the key's config with the root token (to flip properties in tests).
func (tr *Transit) SetConfig(t testing.TB, cfg map[string]any) {
	t.Helper()
	tr.Root(t, http.MethodPost, tr.Mount+"/keys/"+tr.Key+"/config", cfg)
}
