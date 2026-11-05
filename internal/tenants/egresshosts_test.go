package tenants_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// splitIDP is an issuer whose discovery is on one host and its keys on another (Google's
// accounts.google.com and www.googleapis.com), recording what was fetched.
type splitIDP struct {
	key  *rsa.PrivateKey
	mu   sync.Mutex
	gets []string
}

func (f *splitIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	f.mu.Lock()
	f.gets = append(f.gets, url)
	f.mu.Unlock()
	switch url {
	case "https://accounts.example/.well-known/openid-configuration":
		return []byte(`{"issuer":"https://accounts.example","jwks_uri":"https://keys.example/jwks"}`), http.Header{}, nil
	case "https://keys.example/jwks":
		b64 := base64.RawURLEncoding.EncodeToString
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

func (f *splitIDP) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.gets
	f.gets = nil
	return out
}

func listing(t *testing.T, entries ...string) *egress.Client {
	t.Helper()
	h, err := egress.NewHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := egress.New(egress.Config{Hosts: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Spec 0044: issuer add checks the issuer URL's host and an explicit jwks_uri's before any fetch,
// and a discovered jwks_uri's before the keys are read; a refusal is the *egress.HostError naming
// the host, never ErrIssuerFetch.
func TestIssuerHosts(t *testing.T) {
	ctx := context.Background()
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			st := e.Open(t)
			svc := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			if _, err := svc.CreateTenant(ctx, admin, "acme", "", "", ""); err != nil {
				t.Fatal(err)
			}
			idp := &splitIDP{key: k}
			adm := &tenants.AuthAdmin{Store: st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://kista.example"}
			for name, c := range map[string]struct {
				hosts   []string
				is      store.Issuer
				host    string
				fetched []string
			}{
				"the URL's host": {[]string{"keys.example"}, store.Issuer{Name: "a", URL: "https://accounts.example"}, "accounts.example", nil},
				"an explicit jwks_uri": {[]string{"accounts.example"}, store.Issuer{Name: "b", URL: "https://accounts.example",
					JWKSURI: "https://keys.example/jwks"}, "keys.example", nil},
				"a discovered jwks_uri": {[]string{"accounts.example"}, store.Issuer{Name: "c", URL: "https://accounts.example"}, "keys.example",
					[]string{"https://accounts.example/.well-known/openid-configuration"}},
			} {
				adm.Hosts = listing(t, c.hosts...)
				_, err := adm.AddIssuer(ctx, admin, "acme", c.is)
				var he *egress.HostError
				if !errors.As(err, &he) || he.Host != c.host || errors.Is(err, tenants.ErrIssuerFetch) {
					t.Errorf("%s: %v", name, err)
				}
				if got := idp.fetched(); !slices.Equal(got, c.fetched) {
					t.Errorf("%s: fetched %v, want %v", name, got, c.fetched)
				}
			}
			// both hosts listed: added; no list: added
			adm.Hosts = listing(t, "accounts.example", "keys.example")
			if _, err := adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "google", URL: "https://accounts.example"}); err != nil {
				t.Fatalf("both listed: %v", err)
			}
			adm.Hosts = nil
			if _, err := adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "keys", URL: "https://accounts.example/x",
				JWKSURI: "https://keys.example/jwks"}); err != nil {
				t.Fatalf("no list: %v", err)
			}
		})
	}
}
