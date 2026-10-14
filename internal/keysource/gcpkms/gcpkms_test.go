package gcpkms_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/gcpkms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/sourcetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

const (
	ring    = "projects/kista-test/locations/europe-west4/keyRings/kista"
	key     = ring + "/cryptoKeys/ext-a"
	version = key + "/cryptoKeyVersions/1"
)

func crc(b []byte) string {
	return strconv.FormatUint(uint64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))), 10)
}

// fakeKMS speaks the Cloud KMS REST calls the source makes.
type fakeKMS struct {
	mu         sync.Mutex
	key        *rsa.PrivateKey
	pub        *rsa.PublicKey
	ver        map[string]any
	labels     map[string]string
	purpose    string
	signName   string
	signLevel  string
	corrupt    bool
	badCRC     int // the next n sign answers carry a wrong signature CRC32C
	unverified bool
	hang       atomic.Bool
	fail       atomic.Int32
	requests   atomic.Int32
	errorBody  string
	signError  string
	pemCRC     string // replaces the public key's CRC32C
	pkAlg      string // replaces the public key's algorithm
	pemRaw     string // replaces the PEM
	keyName    string
	importOnly bool
	done       chan struct{}
	srv        *httptest.Server
}

func newFake(t *testing.T) *fakeKMS {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKMS{key: k, pub: &k.PublicKey, purpose: "ASYMMETRIC_SIGN", signName: version, signLevel: "HSM", done: make(chan struct{}),
		ver:    map[string]any{"name": version, "state": "ENABLED", "algorithm": "RSA_SIGN_PKCS1_2048_SHA256", "protectionLevel": "HSM"},
		labels: map[string]string{gcpkms.PurposeLabel: keysource.PurposeChannel, "env": "test"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *fakeKMS) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.hang.Load() {
		select {
		case <-r.Context().Done():
		case <-f.done:
		}
		return
	}
	if f.fail.Load() > 0 {
		f.fail.Add(-1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":503,"status":"UNAVAILABLE","message":"try again"}}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimPrefix(r.URL.Path, "/v1/")
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, body string) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
	if f.errorBody != "" && !strings.HasSuffix(name, ":asymmetricSign") {
		fail(http.StatusForbidden, f.errorBody)
		return
	}
	var out any
	switch {
	case r.Method == http.MethodGet && name == version:
		out = f.ver
	case r.Method == http.MethodGet && name == key:
		name := key
		if f.keyName != "" {
			name = f.keyName
		}
		out = map[string]any{"name": name, "purpose": f.purpose, "labels": f.labels, "importOnly": f.importOnly}
	case r.Method == http.MethodGet && name == version+"/publicKey":
		der, _ := x509.MarshalPKIXPublicKey(f.pub)
		p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		if f.pemRaw != "" {
			p = f.pemRaw
		}
		c, alg := crc([]byte(p)), "RSA_SIGN_PKCS1_2048_SHA256"
		if f.pemCRC != "" {
			c = f.pemCRC
		}
		if f.pkAlg != "" {
			alg = f.pkAlg
		}
		out = map[string]any{"name": version, "pem": p, "pemCrc32c": c, "algorithm": alg, "protectionLevel": f.ver["protectionLevel"]}
	case r.Method == http.MethodPost && name == version+":asymmetricSign":
		if f.signError != "" {
			fail(http.StatusForbidden, f.signError)
			return
		}
		var in struct {
			Digest struct {
				SHA256 string `json:"sha256"`
			} `json:"digest"`
			DigestCRC32C string `json:"digestCrc32c"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		d, _ := base64.StdEncoding.DecodeString(in.Digest.SHA256)
		if len(d) != 32 || in.DigestCRC32C != crc(d) {
			fail(http.StatusBadRequest, `{"error":{"status":"INVALID_ARGUMENT"}}`)
			return
		}
		sig, _ := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, d)
		if f.corrupt {
			sig[3] ^= 0xff
		}
		sc := crc(sig)
		if f.badCRC > 0 {
			f.badCRC--
			sc = "1"
		}
		out = map[string]any{"signature": base64.StdEncoding.EncodeToString(sig), "signatureCrc32c": sc,
			"verifiedDigestCrc32c": !f.unverified, "name": f.signName, "protectionLevel": f.signLevel}
	default:
		fail(http.StatusNotFound, `{"error":{"status":"NOT_FOUND"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func tokens() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"})
}

func source(f *fakeKMS, requireHSM bool) *gcpkms.Source {
	return gcpkms.New(ring, tokens(), requireHSM, http.DefaultClient, gcpkms.Options{Endpoint: f.srv.URL + "/v1/"})
}

type harness struct{ f *fakeKMS }

func (h *harness) New(t *testing.T, opts keysource.Options) (*keysource.Registry, string) {
	h.f = newFake(t)
	if opts.Allow == nil {
		opts.Allow = []string{"ext-"}
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("src", source(h.f, true), opts); err != nil {
		t.Fatal(err)
	}
	return reg, "src:ext-a/1"
}

func (h *harness) Key() *rsa.PrivateKey { return h.f.key }

func (h *harness) Mutate(t *testing.T, m sourcetest.Mutation) bool {
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m {
	case sourcetest.Disabled:
		f.ver["state"] = "DISABLED"
	case sourcetest.Imported:
		f.ver["importJob"] = ring + "/importJobs/j"
	case sourcetest.WrongType:
		f.ver["algorithm"] = "EC_SIGN_P256_SHA256"
	case sourcetest.SoftwareKey:
		f.ver["protectionLevel"] = "SOFTWARE"
	case sourcetest.Size3072:
		k, _ := rsa.GenerateKey(rand.Reader, 3072)
		f.pub = &k.PublicKey
	case sourcetest.Exponent3:
		pub := f.key.PublicKey
		pub.E = 3
		f.pub = &pub
	case sourcetest.CanEncrypt:
		f.purpose = "ASYMMETRIC_DECRYPT"
	case sourcetest.NoPurpose:
		delete(f.labels, gcpkms.PurposeLabel)
	case sourcetest.OtherVersion:
		f.ver["name"] = key + "/cryptoKeyVersions/2"
	case sourcetest.PublicKeyChange:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		f.pub = &k.PublicKey
	case sourcetest.SignOtherVersion:
		f.signName = key + "/cryptoKeyVersions/2"
	case sourcetest.CorruptSignature:
		f.corrupt = true
	case sourcetest.Hang:
		f.hang.Store(true)
	case sourcetest.SecretInBody:
		f.errorBody = `{"error":{"code":403,"status":"PERMISSION_` + sourcetest.Secret + `","message":"` + sourcetest.Secret + `"}}`
	case sourcetest.SecretInSignBody:
		f.signError = `{"error":{"status":"` + sourcetest.Secret + `","message":"` + sourcetest.Secret + `"}}`
	case sourcetest.Transient:
		f.fail.Store(1)
	case sourcetest.AlwaysFail:
		f.fail.Store(1000)
	default: // Expired, NotYetValid, Exportable, ReleasePolicy, CertBacked: no such property in Cloud KMS
		return false
	}
	return true
}

func TestContract(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T) sourcetest.Harness { return &harness{} })
}

// The rest of spec 0004 phase 4's checks.
func TestGCPChecks(t *testing.T) {
	open := func(t *testing.T, f *fakeKMS, requireHSM bool, purpose string) (signer.Signer, error) {
		t.Helper()
		reg := keysource.NewRegistry(signer.Resolver{})
		if err := reg.Add("src", source(f, requireHSM), keysource.Options{Allow: []string{"*"}}); err != nil {
			t.Fatal(err)
		}
		return reg.OpenPurpose(t.Context(), "src:ext-a/1", purpose)
	}
	for name, mut := range map[string]func(f *fakeKMS){
		"external": func(f *fakeKMS) { f.ver["protectionLevel"] = "EXTERNAL" },
		"external options": func(f *fakeKMS) {
			f.ver["externalProtectionLevelOptions"] = map[string]any{"externalKeyUri": "https://x"}
		},
		"destroy scheduled":          func(f *fakeKMS) { f.ver["state"] = "DESTROY_SCHEDULED" },
		"attestation key as channel": func(f *fakeKMS) { f.labels[gcpkms.PurposeLabel] = keysource.PurposeAttestation },
	} {
		f := newFake(t)
		mut(f)
		if _, err := open(t, f, true, keysource.PurposeChannel); !errors.Is(err, keysource.ErrKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f := newFake(t)
	f.ver["protectionLevel"] = "HSM_SINGLE_TENANT"
	if _, err := open(t, f, true, keysource.PurposeChannel); err != nil {
		t.Errorf("single-tenant HSM: %v", err)
	}
	f = newFake(t)
	f.ver["protectionLevel"], f.signLevel = "SOFTWARE", "SOFTWARE"
	sg, err := open(t, f, false, keysource.PurposeChannel)
	if err != nil {
		t.Fatalf("a software key without require_hsm: %v", err)
	}
	if _, err := sg.Sign(t.Context(), [32]byte{1}); err != nil {
		t.Fatal(err)
	}
	// a signature CRC mismatch is retried once, then refused; an unverified digest is refused
	f = newFake(t)
	if sg, err = open(t, f, true, keysource.PurposeChannel); err != nil {
		t.Fatal(err)
	}
	f.badCRC = 1
	if _, err := sg.Sign(t.Context(), [32]byte{2}); err != nil {
		t.Errorf("one CRC mismatch: %v", err)
	}
	f.badCRC = 2
	if _, err := sg.Sign(t.Context(), [32]byte{3}); !errors.Is(err, keysource.ErrSign) {
		t.Errorf("two CRC mismatches: %v", err)
	}
	f.unverified = true
	if _, err := sg.Sign(t.Context(), [32]byte{4}); !errors.Is(err, keysource.ErrSign) {
		t.Errorf("an unverified digest: %v", err)
	}
	f.unverified, f.signLevel = false, "SOFTWARE"
	if _, err := sg.Sign(t.Context(), [32]byte{5}); !errors.Is(err, keysource.ErrSign) {
		t.Errorf("signed at software level: %v", err)
	}
	// at most 3 attempts
	f = newFake(t)
	f.fail.Store(1000)
	if _, err := open(t, f, true, keysource.PurposeChannel); err == nil || f.requests.Load() != 3 {
		t.Errorf("attempts: %d %v", f.requests.Load(), err)
	}
	// the reference grammar
	src := source(f, true)
	for ref, ok := range map[string]bool{"ext-a/1": true, "ext-a/10": true, "ext-a/0": false, "ext-a": false, "ext-a/latest": false,
		"ext-a/1/2": false, "../x/1": false, "projects/p/1": false} {
		if _, err := src.ParseKey(ref); (err == nil) != ok {
			t.Errorf("ParseKey(%q): %v", ref, err)
		}
	}
}

// countingTokens counts token fetches; hanging blocks them.
type countingTokens struct {
	n    atomic.Int32
	hang chan struct{}
}

func (c *countingTokens) Token() (*oauth2.Token, error) {
	c.n.Add(1)
	if c.hang != nil {
		<-c.hang
	}
	return &oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}, nil
}

// The public key answer, the key's name and importOnly; one token per call across retries; a token
// source that hangs is bounded by the call's timeout.
func TestGCPAnswers(t *testing.T) {
	open := func(t *testing.T, f *fakeKMS, ts oauth2.TokenSource, timeout time.Duration) error {
		t.Helper()
		reg := keysource.NewRegistry(signer.Resolver{})
		src := gcpkms.New(ring, ts, true, http.DefaultClient, gcpkms.Options{Endpoint: f.srv.URL + "/v1/"})
		if err := reg.Add("src", src, keysource.Options{Allow: []string{"*"}, Timeout: timeout}); err != nil {
			t.Fatal(err)
		}
		_, err := reg.Open(t.Context(), "src:ext-a/1")
		return err
	}
	ecPEM := "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEo7l1cPAWx1jqNOTI8WIFlTDJyKe8uGnmyTJbH8GWb/9lDT0p3XeZvvpXnhlNfBV/EeL1UB0rOewYytCO8/5OTw==\n-----END PUBLIC KEY-----\n"
	for name, mut := range map[string]func(f *fakeKMS){
		"pem CRC32C":         func(f *fakeKMS) { f.pemCRC = "1" },
		"public key algo":    func(f *fakeKMS) { f.pkAlg = "RSA_SIGN_PKCS1_3072_SHA256" },
		"not PEM":            func(f *fakeKMS) { f.pemRaw = "not a pem" },
		"not RSA":            func(f *fakeKMS) { f.pemRaw = ecPEM },
		"import only":        func(f *fakeKMS) { f.importOnly = true },
		"another key's name": func(f *fakeKMS) { f.keyName = ring + "/cryptoKeys/other" },
	} {
		f := newFake(t)
		mut(f)
		if err := open(t, f, tokens(), 0); !errors.Is(err, keysource.ErrKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f := newFake(t)
	f.fail.Store(2)
	c := &countingTokens{}
	if err := open(t, f, c, 0); err != nil {
		t.Fatal(err)
	}
	if n := c.n.Load(); n != 3 { // three calls (version, key, public key), one token each despite the retries
		t.Errorf("%d token fetches", n)
	}
	h := &countingTokens{hang: make(chan struct{})}
	defer close(h.hang)
	start := time.Now()
	if err := open(t, newFake(t), h, 300*time.Millisecond); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a hanging token source: %v after %s", err, time.Since(start))
	}
}
