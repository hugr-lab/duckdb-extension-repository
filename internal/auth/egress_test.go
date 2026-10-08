package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The verifier against an OIDC issuer on a loopback TLS server, through the real egress client
// (allowlisted address and port, the server's CA as ca_file): discovery, JWKS, a verified token.
func TestVerifyThroughEgress(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var issuer string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/jwks"})
		case "/jwks":
			w.Header().Set("Cache-Control", "max-age=600")
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	issuer = srv.URL
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	port := uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
	eg, err := egress.New(egress.Config{CAFile: ca, Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{port}}}})
	if err != nil {
		t.Fatal(err)
	}
	v := &auth.Verifier{Fetch: eg}
	ta := store.TenantAuth{Issuers: []store.Issuer{{ID: "i1", Name: "corp", URL: issuer, Algorithms: auth.Algorithms}}}
	enc := func(x any) string { b, _ := json.Marshal(x); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now()
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(map[string]any{"iss": issuer, "sub": "alice",
		"aud": "https://kista.example/acme", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	p, name, err := v.Verify(context.Background(), ta, "https://kista.example/acme", input+"."+base64.RawURLEncoding.EncodeToString(sig))
	if err != nil || name != "corp" || !p[auth.Key{IssuerID: "i1", Kind: "subject", Value: "alice"}] {
		t.Fatalf("through egress: %v %v", p, err)
	}
	// the same issuer without the allowlist is refused by egress
	strict, _ := egress.New(egress.Config{CAFile: ca})
	v2 := &auth.Verifier{Fetch: strict}
	if _, _, err := v2.Verify(context.Background(), ta, "https://kista.example/acme", input+"."+base64.RawURLEncoding.EncodeToString(sig)); err == nil {
		t.Fatal("verified with an issuer egress may not reach")
	}
}
