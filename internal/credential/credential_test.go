package credential_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
)

var ctx = context.Background()

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenEndpoint is a fake OAuth 2.0 token endpoint.
type tokenEndpoint struct {
	mu     sync.Mutex
	forms  []url.Values
	status int
	answer string
}

func (e *tokenEndpoint) PostForm(_ context.Context, raw string, form url.Values) (int, []byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forms = append(e.forms, form)
	return e.status, []byte(e.answer), nil
}

func registry(t *testing.T, c config.Credential, e *tokenEndpoint, d credential.Deps) credential.Registry {
	t.Helper()
	if d.Poster == nil {
		d.Poster = func(config.Credential) (credential.Poster, error) { return e, nil }
	}
	r, err := credential.New([]config.Credential{c}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Spec 0009 phase 3: a credential is used only by the tenants it lists and at the prefixes it
// covers, on a segment boundary.
func TestFor(t *testing.T) {
	r := registry(t, config.Credential{Name: "c", Tenants: []string{"acme"}, Kind: "token_file", TokenFile: "/x",
		Prefixes: []string{"https://Enterest.example/acme/", "https://kista-b.internal:8443/x"}}, nil, credential.Deps{})
	for prefix, want := range map[string]bool{
		"https://enterest.example/acme/prod":       true,
		"https://enterest.example/acme":            true,
		"https://enterest.example/acmecorp/prod":   false, // not a segment boundary
		"https://enterest.example/beta/prod":       false,
		"http://enterest.example/acme/prod":        false, // https only
		"https://enterest.example:8443/acme/prod":  false, // another port
		"https://kista-b.internal:8443/x/y":        true,
		"https://kista-b.internal/x/y":             false,
		"https://enterest.example.evil/acme/prod":  false,
		"https://user@enterest.example/acme/prod":  false,
		"https://enterest.example/acme/../x/prod":  false,
		"https://enterest.example:443/acme/stable": true,
	} {
		_, err := r.For("c", "acme", prefix)
		if (err == nil) != want {
			t.Errorf("%s: %v", prefix, err)
		}
	}
	if _, err := r.For("c", "beta", "https://enterest.example/acme/prod"); !errors.Is(err, credential.ErrNotAllowed) {
		t.Errorf("another tenant: %v", err)
	}
	if _, err := r.For("nope", "acme", "https://enterest.example/acme/prod"); !errors.Is(err, credential.ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
}

func TestTokenFile(t *testing.T) {
	p := write(t, "token", "abc\n")
	r := registry(t, config.Credential{Name: "c", Tenants: []string{"*"}, Prefixes: []string{"https://u.example/"}, Kind: "token_file",
		TokenFile: p}, nil, credential.Deps{})
	c, _ := r.For("c", "any", "https://u.example/t/c")
	if tok, err := c.Token(ctx); err != nil || tok != "abc" {
		t.Fatalf("token: %q %v", tok, err)
	}
	if err := os.WriteFile(p, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := c.Token(ctx); tok != "rotated" {
		t.Fatalf("read at each use: %q", tok)
	}
	_ = os.WriteFile(p, []byte(""), 0o600)
	if _, err := c.Token(ctx); !errors.Is(err, credential.ErrToken) {
		t.Fatalf("empty: %v", err)
	}
}

func pemKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// verify checks an RS256 JWT with a public key and returns its claims.
func verify(t *testing.T, jwt string, pub *rsa.PublicKey) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT: %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, d[:], sig); err != nil {
		t.Fatalf("the assertion's signature: %v", err)
	}
	var h, c map[string]any
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(hb, &h)
	_ = json.Unmarshal(cb, &c)
	return h, c
}

// Each client authentication: what the token request carries; the token cached until a minute
// before it ends, fetched again after Invalidate.
func TestClientCredentials(t *testing.T) {
	key, keyPEM := pemKey(t)
	zitadel, _ := json.Marshal(map[string]string{"type": "application", "keyId": "kid-z", "key": keyPEM, "clientId": "cid"})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	base := config.Credential{Name: "c", Tenants: []string{"acme"}, Prefixes: []string{"https://u.example/acme/"}, Kind: "client_credentials",
		TokenURL: "https://idp.example/token", ClientID: "cid", Scope: "api://u/.default"}
	for name, c := range map[string]struct {
		set   func(*config.Credential)
		check func(t *testing.T, f url.Values)
	}{
		"secret": {func(c *config.Credential) { c.ClientAuth, c.ClientSecretFile = "secret", write(t, "s", "s3cret\n") },
			func(t *testing.T, f url.Values) {
				if f.Get("client_secret") != "s3cret" || f.Get("client_assertion") != "" {
					t.Fatalf("a secret: %v", f)
				}
			}},
		"file": {func(c *config.Credential) { c.ClientAuth, c.AssertionFile = "file", write(t, "a", "projected.jwt\n") },
			func(t *testing.T, f url.Values) {
				if f.Get("client_assertion") != "projected.jwt" || f.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
					t.Fatalf("an assertion file: %v", f)
				}
			}},
		"azure": {func(c *config.Credential) { c.ClientAuth = "azure" }, func(t *testing.T, f url.Values) {
			if f.Get("client_assertion") != "mi-token-for:api://AzureADTokenExchange/.default" {
				t.Fatalf("the platform's identity: %v", f)
			}
		}},
		"key_file zitadel": {func(c *config.Credential) {
			c.ClientAuth, c.KeyFile, c.AssertionAudience = "key_file", write(t, "z.json", string(zitadel)), "https://idp.example"
		}, func(t *testing.T, f url.Values) {
			h, cl := verify(t, f.Get("client_assertion"), &key.PublicKey)
			if h["kid"] != "kid-z" || cl["iss"] != "cid" || cl["sub"] != "cid" || cl["aud"] != "https://idp.example" ||
				cl["exp"].(float64)-cl["iat"].(float64) != 60 || cl["jti"] == "" {
				t.Fatalf("the assertion: %v %v", h, cl)
			}
		}},
		"key_file pem": {func(c *config.Credential) {
			c.ClientAuth, c.KeyFile, c.KeyID = "key_file", write(t, "k.pem", keyPEM), "kid-p"
		},
			func(t *testing.T, f url.Values) {
				h, cl := verify(t, f.Get("client_assertion"), &key.PublicKey)
				if h["kid"] != "kid-p" || cl["aud"] != "https://idp.example/token" {
					t.Fatalf("the assertion (aud the token endpoint): %v %v", h, cl)
				}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			c.set(&cfg)
			e := &tokenEndpoint{status: 200, answer: `{"access_token":"T1","expires_in":3600}`}
			r := registry(t, cfg, e, credential.Deps{Now: func() time.Time { return now },
				Azure: func(_ context.Context, scope string) (string, error) { return "mi-token-for:" + scope, nil }})
			cr, err := r.For("c", "acme", "https://u.example/acme/prod")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if tok, err := cr.Token(ctx); err != nil || tok != "T1" {
					t.Fatalf("token: %q %v", tok, err)
				}
			}
			if len(e.forms) != 1 {
				t.Fatalf("cached: %d requests", len(e.forms))
			}
			f := e.forms[0]
			if f.Get("grant_type") != "client_credentials" || f.Get("client_id") != "cid" || f.Get("scope") != "api://u/.default" {
				t.Fatalf("the request: %v", f)
			}
			c.check(t, f)
			now = now.Add(31 * time.Second) // refused tokens younger than 30 seconds are kept
			cr.Invalidate("T1")
			_, _ = cr.Token(ctx)
			now = now.Add(59*time.Minute + time.Second) // within the last minute: fetched again
			_, _ = cr.Token(ctx)
			if len(e.forms) != 3 {
				t.Fatalf("after Invalidate and near the end: %d requests", len(e.forms))
			}
			now = now.Add(-59*time.Minute - time.Second - 31*time.Second)
		})
	}
}

// The identity provider refusing kista's own client is the operator's; another refusal is no
// token; neither error carries the answer.
func TestTokenEndpointRefusals(t *testing.T) {
	cfg := config.Credential{Name: "c", Tenants: []string{"acme"}, Prefixes: []string{"https://u.example/"}, Kind: "client_credentials",
		TokenURL: "https://idp.example/token", ClientID: "cid", ClientAuth: "secret", ClientSecretFile: write(t, "s", "s3cret")}
	for answer, want := range map[string]error{
		`{"error":"invalid_client","error_description":"AADSTS7000215 secret s3cret is wrong"}`: credential.ErrClient,
		`{"error":"unauthorized_client"}`:                                  credential.ErrClient,
		`{"error":"invalid_scope","error_description":"the scope s3cret"}`: credential.ErrToken,
	} {
		e := &tokenEndpoint{status: 400, answer: answer}
		c, _ := registry(t, cfg, e, credential.Deps{}).For("c", "acme", "https://u.example/x")
		_, err := c.Token(ctx)
		if !errors.Is(err, want) || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "AADSTS") {
			t.Errorf("%s: %v", answer, err)
		}
	}
}

// slowEndpoint answers after release closes, counting requests.
type slowEndpoint struct {
	tokenEndpoint
	release chan struct{}
}

func (e *slowEndpoint) PostForm(ctx context.Context, raw string, form url.Values) (int, []byte, error) {
	<-e.release
	return e.tokenEndpoint.PostForm(ctx, raw, form)
}

// One fetch for many callers; a failure given back for a while; a short-lived token still cached;
// Invalidate forgets only the token refused.
func TestTokenFetching(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	cfg := config.Credential{Name: "c", Tenants: []string{"acme"}, Prefixes: []string{"https://u.example/"}, Kind: "client_credentials",
		TokenURL: "https://idp.example/token", ClientID: "cid", ClientAuth: "secret", ClientSecretFile: write(t, "s", "x")}
	e := &slowEndpoint{tokenEndpoint: tokenEndpoint{status: 200, answer: `{"access_token":"T1","expires_in":30}`}, release: make(chan struct{})}
	r, err := credential.New([]config.Credential{cfg}, credential.Deps{Now: clock,
		Poster: func(config.Credential) (credential.Poster, error) { return e, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.For("c", "acme", "https://u.example/x")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := c.Token(ctx); err != nil || tok != "T1" {
				t.Errorf("token: %q %v", tok, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(e.release)
	wg.Wait()
	if len(e.forms) != 1 {
		t.Fatalf("one fetch for eight callers: %d", len(e.forms))
	}
	// a 30-second token is used for half its life
	advance(10 * time.Second)
	_, _ = c.Token(ctx)
	if len(e.forms) != 1 {
		t.Fatalf("a short token cached: %d", len(e.forms))
	}
	// a stale refusal does not throw away the current token
	c.Invalidate("T0")
	_, _ = c.Token(ctx)
	if len(e.forms) != 1 {
		t.Fatalf("Invalidate of another token: %d", len(e.forms))
	}
	// a failure is given back for 30 seconds, then the endpoint is asked again
	advance(10 * time.Second) // the token is older than 30 seconds: a refusal forgets it
	c.Invalidate("T1")
	e.mu.Lock()
	e.status, e.answer = 503, `{}`
	e.mu.Unlock()
	for range 3 {
		if _, err := c.Token(ctx); !errors.Is(err, credential.ErrToken) {
			t.Fatalf("a failing endpoint: %v", err)
		}
	}
	if len(e.forms) != 2 {
		t.Fatalf("a failure cached: %d", len(e.forms))
	}
	advance(31 * time.Second)
	_, _ = c.Token(ctx)
	if len(e.forms) != 3 {
		t.Fatalf("asked again after the failure's while: %d", len(e.forms))
	}
}

// A key file is read again when it changes; a broken one keeps the last good key.
func TestKeyRotation(t *testing.T) {
	k1, p1 := pemKey(t)
	k2, p2 := pemKey(t)
	file := write(t, "k.pem", p1)
	e := &tokenEndpoint{status: 200, answer: `{"access_token":"T","expires_in":1}`}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := registry(t, config.Credential{Name: "c", Tenants: []string{"acme"}, Prefixes: []string{"https://u.example/"}, Kind: "client_credentials",
		TokenURL: "https://idp.example/token", ClientID: "cid", ClientAuth: "key_file", KeyFile: file, KeyID: "k"}, e,
		credential.Deps{Now: func() time.Time { return now }})
	c, _ := r.For("c", "acme", "https://u.example/x")
	assertion := func() string {
		t.Helper()
		now = now.Add(31 * time.Second) // refused tokens younger than 30 seconds are kept
		c.Invalidate("T")
		if _, err := c.Token(ctx); err != nil {
			t.Fatal(err)
		}
		return e.forms[len(e.forms)-1].Get("client_assertion")
	}
	verify(t, assertion(), &k1.PublicKey)
	if err := os.WriteFile(file, []byte(p2+"\n"), 0o600); err != nil { // another size: a change
		t.Fatal(err)
	}
	verify(t, assertion(), &k2.PublicKey)
	if err := os.WriteFile(file, []byte("not a key, and longer than before................................."), 0o600); err != nil {
		t.Fatal(err)
	}
	verify(t, assertion(), &k2.PublicKey)
}
