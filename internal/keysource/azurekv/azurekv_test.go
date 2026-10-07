package azurekv_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/azurekv"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/sourcetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

const (
	host    = "kista-test.vault.azure.net"
	version = "0123456789abcdef0123456789abcdef"
)

// fakeVault speaks the Key Vault REST calls azkeys makes, including the bearer challenge. The real
// azkeys client reaches it through a transport that dials the fake for the real vault host, so the
// challenge-resource verification stays on.
type fakeVault struct {
	mu        sync.Mutex
	key       *rsa.PrivateKey
	bundle    map[string]any
	attrs     map[string]any
	signVer   string
	corrupt   bool
	hang      atomic.Bool
	fail      atomic.Int32 // the next n requests answer 503
	errorBody string
	signError string
	resource  string // the challenge resource; default https://vault.azure.net
	srv       *httptest.Server
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVault{key: k}
	f.attrs = map[string]any{"enabled": true, "exportable": false}
	f.bundle = map[string]any{"tags": map[string]string{azurekv.PurposeTag: azurekv.PurposeValue}}
	f.setKey(&k.PublicKey)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) setKey(pub *rsa.PublicKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bundle == nil {
		f.bundle = map[string]any{}
	}
	f.bundle["key"] = map[string]any{
		"kid": "https://" + host + "/keys/ext-a/" + version, "kty": "RSA-HSM", "key_ops": []string{"sign", "verify"},
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (f *fakeVault) keyField(name string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bundle["key"].(map[string]any)[name] = v
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	if f.hang.Load() {
		<-r.Context().Done()
		return
	}
	if f.fail.Load() > 0 {
		f.fail.Add(-1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") == "" {
		res := f.resource
		if res == "" {
			res = "https://vault.azure.net"
		}
		w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/tenant-id", resource="`+res+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errorBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(f.errorBody))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.ToLower(r.URL.Path) // Key Vault names are case-insensitive
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/keys/ext-a/"+version):
		out := map[string]any{"attributes": f.attrs}
		for k, v := range f.bundle {
			out[k] = v
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && path == "/keys/ext-a/"+version+"/sign" && f.signError != "":
		w.Header().Set("x-ms-request-id", "req-1")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(f.signError))
	case r.Method == http.MethodPost && path == "/keys/ext-a/"+version+"/sign":
		var req struct {
			Alg   string `json:"alg"`
			Value string `json:"value"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		d, _ := base64.RawURLEncoding.DecodeString(req.Value)
		if req.Alg != "RS256" || len(d) != 32 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig, _ := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, d)
		if f.corrupt {
			sig[0] ^= 1
		}
		v := version
		if f.signVer != "" {
			v = f.signVer
		}
		json.NewEncoder(w).Encode(map[string]any{"kid": "https://" + host + "/keys/ext-a/" + v, "value": b64(sig)})
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"KeyNotFound","message":"not found"}}`))
	}
}

// transport sends requests for the real vault host to the fake.
func (f *fakeVault) transport() policy.Transporter {
	addr := f.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the fake's certificate is for 127.0.0.1; test only
	}}
}

type fakeCred struct {
	scopes *[]string
	err    error
}

func (c fakeCred) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if c.scopes != nil {
		*c.scopes = append(*c.scopes, o.Scopes...)
	}
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// harness adapts the fake to the contract suite.
type harness struct{ f *fakeVault }

func (h *harness) New(t *testing.T, opts keysource.Options) (*keysource.Registry, string) {
	h.f = newFakeVault(t)
	src, err := azurekv.New(host, fakeCred{}, true, azurekv.Options{Transport: h.f.transport(), Timeout: opts.Timeout})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Allow == nil {
		opts.Allow = []string{"ext-"}
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("src", src, opts); err != nil {
		t.Fatal(err)
	}
	return reg, "src:ext-a/" + version
}

func (h *harness) Key() *rsa.PrivateKey { return h.f.key }

func (h *harness) Mutate(t *testing.T, m sourcetest.Mutation) bool {
	f := h.f
	set := func(k string, v any) { f.mu.Lock(); f.attrs[k] = v; f.mu.Unlock() }
	setB := func(k string, v any) { f.mu.Lock(); f.bundle[k] = v; f.mu.Unlock() }
	switch m {
	case sourcetest.Disabled:
		set("enabled", false)
	case sourcetest.Expired:
		set("exp", time.Now().Add(-time.Hour).Unix())
	case sourcetest.NotYetValid:
		set("nbf", time.Now().Add(time.Hour).Unix())
	case sourcetest.Exportable:
		set("exportable", true)
	case sourcetest.ReleasePolicy:
		setB("release_policy", map[string]any{"contentType": "application/json; charset=utf-8", "data": b64([]byte("{}"))})
	case sourcetest.WrongType:
		f.keyField("kty", "EC-HSM")
	case sourcetest.SoftwareKey:
		f.keyField("kty", "RSA")
	case sourcetest.Size3072:
		k, _ := rsa.GenerateKey(rand.Reader, 3072)
		f.setKey(&k.PublicKey)
	case sourcetest.Exponent3:
		pub := f.key.PublicKey
		pub.E = 3
		f.setKey(&pub)
	case sourcetest.CanEncrypt:
		f.keyField("key_ops", []string{"sign", "encrypt"})
	case sourcetest.NoPurpose:
		setB("tags", map[string]string{})
	case sourcetest.CertBacked:
		setB("managed", true)
	case sourcetest.OtherVersion:
		f.keyField("kid", "https://"+host+"/keys/ext-a/ffffffffffffffffffffffffffffffff")
	case sourcetest.PublicKeyChange:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		f.setKey(&k.PublicKey)
	case sourcetest.SignOtherVersion:
		f.mu.Lock()
		f.signVer = "ffffffffffffffffffffffffffffffff"
		f.mu.Unlock()
	case sourcetest.CorruptSignature:
		f.mu.Lock()
		f.corrupt = true
		f.mu.Unlock()
	case sourcetest.Hang:
		f.hang.Store(true)
	case sourcetest.SecretInBody:
		f.mu.Lock()
		f.errorBody = `{"error":{"code":"Forbidden` + sourcetest.Secret + `","message":"caller ` + sourcetest.Secret + ` is not allowed"}}`
		f.mu.Unlock()
	case sourcetest.SecretInSignBody:
		f.mu.Lock()
		f.signError = `{"error":{"code":"` + sourcetest.Secret + `","message":"` + sourcetest.Secret + `"}}`
		f.mu.Unlock()
	case sourcetest.Transient:
		f.fail.Store(1)
	case sourcetest.AlwaysFail:
		f.fail.Store(1000)
	default:
		return false // Imported: Key Vault does not report imported (BYOK) material
	}
	return true
}

func TestContract(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T) sourcetest.Harness { return &harness{} })
}

func TestGrammarAndIdentity(t *testing.T) {
	h := &harness{}
	reg, ref := h.New(t, keysource.Options{Allow: []string{"ext-"}})
	for _, bad := range []string{"src:ext-a", "src:ext-a/", "src:ext-a/latest", "src:ext-a/" + strings.ToUpper(version),
		"src:ext_a/" + version, "src:ext-a/" + version + "/x", "src:other/" + version, "src:https://evil/keys/ext-a/" + version} {
		if _, _, err := reg.Parse(bad); !errors.Is(err, keysource.ErrReference) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// names are case-insensitive: EXT-A matches the allow prefix and the vault's lowercase kid
	if _, err := reg.Open(context.Background(), "src:EXT-A/"+version); err != nil {
		t.Fatalf("upper-case name: %v", err)
	}
	if _, err := reg.Open(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestRequireHSMOffAcceptsRSA(t *testing.T) {
	f := newFakeVault(t)
	f.keyField("kty", "RSA")
	src, err := azurekv.New(host, fakeCred{}, false, azurekv.Options{Transport: f.transport()})
	if err != nil {
		t.Fatal(err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	reg.Add("src", src, keysource.Options{Allow: []string{"*"}})
	if _, err := reg.Open(context.Background(), "src:ext-a/"+version); err != nil {
		t.Fatal(err)
	}
}

func openWith(t *testing.T, f *fakeVault, cred azcore.TokenCredential) error {
	t.Helper()
	src, err := azurekv.New(host, cred, true, azurekv.Options{Transport: f.transport()})
	if err != nil {
		t.Fatal(err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	reg.Add("src", src, keysource.Options{Allow: []string{"*"}})
	_, err = reg.Open(context.Background(), "src:ext-a/"+version)
	return err
}

// The bearer challenge's resource must belong to the vault's domain: a challenge naming another
// resource is not answered with a token, and the token is requested for the vault's scope.
func TestChallengeResource(t *testing.T) {
	f := newFakeVault(t)
	var scopes []string
	if err := openWith(t, f, fakeCred{scopes: &scopes}); err != nil {
		t.Fatal(err)
	}
	if len(scopes) == 0 || scopes[0] != "https://vault.azure.net/.default" {
		t.Fatalf("token scopes %v", scopes)
	}
	f.resource = "https://evil.example"
	scopes = nil
	if err := openWith(t, f, fakeCred{scopes: &scopes}); err == nil || len(scopes) != 0 {
		t.Fatalf("authenticated to a foreign challenge resource: %v, scopes %v", err, scopes)
	}
}

func TestAuthenticationFailureIsFixed(t *testing.T) {
	f := newFakeVault(t)
	err := openWith(t, f, fakeCred{err: errors.New("token endpoint said " + sourcetest.Secret)})
	if err == nil || strings.Contains(err.Error(), sourcetest.Secret) {
		t.Fatalf("credential failure leaked: %v", err)
	}
}

func TestKeyProperties(t *testing.T) {
	cases := map[string]func(f *fakeVault){
		"verify only": func(f *fakeVault) { f.keyField("key_ops", []string{"verify"}) },
		"no key_ops":  func(f *fakeVault) { f.keyField("key_ops", []string{}) },
		"wrap":        func(f *fakeVault) { f.keyField("key_ops", []string{"sign", "wrapKey"}) },
		"tag with another value": func(f *fakeVault) {
			f.mu.Lock()
			f.bundle["tags"] = map[string]string{azurekv.PurposeTag: "licence-signing"}
			f.mu.Unlock()
		},
		"enabled missing":     func(f *fakeVault) { f.mu.Lock(); delete(f.attrs, "enabled"); f.mu.Unlock() },
		"attributes missing":  func(f *fakeVault) { f.mu.Lock(); f.attrs = nil; f.mu.Unlock() },
		"expires now":         func(f *fakeVault) { f.mu.Lock(); f.attrs["exp"] = time.Now().Unix(); f.mu.Unlock() },
		"exponent too long":   func(f *fakeVault) { f.keyField("e", b64([]byte{1, 0, 0, 0, 1})) },
		"kid on another host": func(f *fakeVault) { f.keyField("kid", "https://evil.vault.azure.net/keys/ext-a/"+version) },
		"kid with a port":     func(f *fakeVault) { f.keyField("kid", "https://"+host+":443/keys/ext-a/"+version) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeVault(t)
			mutate(f)
			if err := openWith(t, f, fakeCred{}); !errors.Is(err, keysource.ErrKey) {
				t.Fatalf("got %v, want ErrKey", err)
			}
		})
	}
}

// A sign answer naming a key on another host is refused.
func TestSignKidOtherHost(t *testing.T) {
	f := newFakeVault(t)
	src, _ := azurekv.New(host, fakeCred{}, true, azurekv.Options{Transport: f.transport()})
	reg := keysource.NewRegistry(signer.Resolver{})
	reg.Add("src", src, keysource.Options{Allow: []string{"*"}})
	sg, err := reg.Open(context.Background(), "src:ext-a/"+version)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.signVer = version + "/../x" // the answer's kid no longer parses as host/keys/name/version
	f.mu.Unlock()
	if _, err := sg.Sign(context.Background(), [32]byte{}); !errors.Is(err, keysource.ErrSign) {
		t.Fatalf("got %v", err)
	}
}
