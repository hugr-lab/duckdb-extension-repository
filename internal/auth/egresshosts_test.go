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
	"errors"
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

// Spec 0044: an issuer whose host is off egress.hosts (after a restart that removed it, nothing is
// cached) fails every token at once, through the real egress client, with the reason issuer host
// not allowed: by discovery, and by an explicit jwks_uri; listed, the same issuer verifies.
func TestVerifyIssuerHostNotAllowed(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var issuer string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/jwks"})
		case "/jwks":
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
	client := func(entries ...string) *egress.Client {
		h, err := egress.NewHosts(entries)
		if err != nil {
			t.Fatal(err)
		}
		eg, err := egress.New(egress.Config{CAFile: ca, Hosts: h,
			Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{port}}}})
		if err != nil {
			t.Fatal(err)
		}
		return eg
	}
	enc := func(x any) string { b, _ := json.Marshal(x); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now()
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(map[string]any{"iss": issuer, "sub": "alice",
		"aud": "https://kista.example/acme", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	token := input + "." + base64.RawURLEncoding.EncodeToString(sig)
	for name, rec := range map[string]store.Issuer{
		"discovered": {ID: "i1", Name: "corp", URL: issuer, Algorithms: auth.Algorithms},
		"explicit":   {ID: "i1", Name: "corp", URL: issuer, JWKSURI: issuer + "/jwks", Algorithms: auth.Algorithms},
	} {
		ta := store.TenantAuth{Issuers: []store.Issuer{rec}}
		v := &auth.Verifier{Fetch: client("idp.example")}
		for range 2 { // the first fetch and the cached refusal alike
			_, err := v.VerifyIdentity(context.Background(), ta, "https://kista.example/acme", token)
			var f *auth.Failure
			if !errors.As(err, &f) || f.Reason != "issuer host not allowed" || f.Issuer != "corp" {
				t.Fatalf("%s: %v", name, err)
			}
			if r := auth.EventReason(err); r != "issuer_host_not_allowed" {
				t.Fatalf("%s: the event's reason %q", name, r)
			}
		}
		listed := &auth.Verifier{Fetch: client("127.0.0.1")}
		if _, err := listed.VerifyIdentity(context.Background(), ta, "https://kista.example/acme", token); err != nil {
			t.Fatalf("%s, listed: %v", name, err)
		}
	}
	// any other failure is invalid in its event
	for _, err := range []error{&auth.Failure{Reason: auth.ReasonKey}, &auth.Failure{Reason: auth.ReasonSignature}, errors.New("x"), nil} {
		if r := auth.EventReason(err); r != "invalid" {
			t.Errorf("%v: %q", err, r)
		}
	}
}
