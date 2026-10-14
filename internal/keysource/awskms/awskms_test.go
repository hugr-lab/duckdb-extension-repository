package awskms_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/awskms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/sourcetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

const (
	region  = "eu-central-1"
	account = "123456789012"
	keyID   = "1234abcd-12ab-34cd-56ef-1234567890ab"
	arn     = "arn:aws:kms:" + region + ":" + account + ":key/" + keyID
	other   = "arn:aws:kms:" + region + ":" + account + ":key/ffffffff-12ab-34cd-56ef-1234567890ab"
)

// fakeKMS speaks the four KMS calls (awsJson1.1, X-Amz-Target) the source makes; the real SDK client
// reaches it through its endpoint option.
type fakeKMS struct {
	mu        sync.Mutex
	key       *rsa.PrivateKey
	pub       *rsa.PublicKey
	meta      map[string]any
	tags      []map[string]string
	usage     string
	signARN   string
	corrupt   bool
	hang      atomic.Bool
	fail      atomic.Int32
	errorBody string
	signError string
	srv       *httptest.Server
	lastSign  map[string]any
	done      chan struct{} // releases hanging requests, so the server can close
	pkARN     string        // the KeyId GetPublicKey answers
	signAlg   string
	nilMeta   bool
	requests  atomic.Int32
	keyIDs    []string // every KeyId asked for
}

func newFake(t *testing.T) *fakeKMS {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKMS{key: k, pub: &k.PublicKey, usage: "SIGN_VERIFY", signARN: arn, pkARN: arn, signAlg: "RSASSA_PKCS1_V1_5_SHA_256",
		meta: map[string]any{"Arn": arn, "KeyId": keyID, "AWSAccountId": account, "KeySpec": "RSA_2048", "KeyUsage": "SIGN_VERIFY",
			"KeyState": "Enabled", "Enabled": true, "Origin": "AWS_KMS", "MultiRegion": false},
		tags: []map[string]string{{"TagKey": "env", "TagValue": "test"}, {"TagKey": awskms.PurposeTag, "TagValue": keysource.PurposeChannel}}}
	f.done = make(chan struct{})
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.done) }) // runs first
	return f
}

func (f *fakeKMS) serve(w http.ResponseWriter, r *http.Request) {
	if f.hang.Load() {
		select {
		case <-r.Context().Done():
		case <-f.done:
		}
		return
	}
	f.requests.Add(1)
	w.Header().Set("x-amzn-RequestId", "0d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d")
	if f.fail.Load() > 0 {
		f.fail.Add(-1)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"__type":"KMSInternalException","message":"try again"}`))
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var in map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &in)
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.")
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := in["KeyId"].(string); ok {
		f.keyIDs = append(f.keyIDs, id)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	fail := func(body string) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(body))
	}
	if f.errorBody != "" && op != "Sign" {
		fail(f.errorBody)
		return
	}
	if in["KeyId"] != arn {
		fail(`{"__type":"NotFoundException","message":"no such key"}`)
		return
	}
	var out any
	switch op {
	case "DescribeKey":
		out = map[string]any{"KeyMetadata": f.meta}
		if f.nilMeta {
			out = map[string]any{}
		}
	case "ListResourceTags":
		out = map[string]any{"Tags": f.tags, "Truncated": false}
	case "GetPublicKey":
		der, _ := x509.MarshalPKIXPublicKey(f.pub)
		out = map[string]any{"KeyId": f.pkARN, "PublicKey": der, "KeyUsage": f.usage, "KeySpec": f.meta["KeySpec"],
			"SigningAlgorithms": []string{"RSASSA_PSS_SHA_256", "RSASSA_PKCS1_V1_5_SHA_256"}}
	case "Sign":
		if f.signError != "" {
			fail(f.signError)
			return
		}
		f.lastSign = in
		msg, _ := in["Message"].(string)
		var digest []byte
		_ = json.Unmarshal([]byte(`"`+msg+`"`), &digest)
		if in["MessageType"] != "DIGEST" || in["SigningAlgorithm"] != "RSASSA_PKCS1_V1_5_SHA_256" || len(digest) != 32 {
			fail(`{"__type":"ValidationException","message":"bad sign request"}`)
			return
		}
		sig, _ := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, digest)
		if f.corrupt {
			sig[10] ^= 0xff
		}
		out = map[string]any{"KeyId": f.signARN, "Signature": sig, "SigningAlgorithm": f.signAlg}
	default:
		fail(`{"__type":"UnknownOperationException"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

type harness struct{ f *fakeKMS }

func source(t *testing.T, f *fakeKMS, multiRegion bool) *awskms.Source {
	t.Helper()
	src, err := awskms.New(region, account, multiRegion, credentials.NewStaticCredentialsProvider("AKID", "secret", ""),
		awskms.Options{Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func (h *harness) New(t *testing.T, opts keysource.Options) (*keysource.Registry, string) {
	h.f = newFake(t)
	if opts.Allow == nil {
		opts.Allow = []string{keyID}
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("src", source(t, h.f, false), opts); err != nil {
		t.Fatal(err)
	}
	return reg, "src:" + keyID
}

func (h *harness) Key() *rsa.PrivateKey { return h.f.key }

func (h *harness) Mutate(t *testing.T, m sourcetest.Mutation) bool {
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m {
	case sourcetest.Disabled:
		f.meta["KeyState"], f.meta["Enabled"] = "Disabled", false
	case sourcetest.Expired:
		f.meta["ValidTo"] = 1700000000
	case sourcetest.Imported:
		f.meta["Origin"] = "EXTERNAL"
	case sourcetest.WrongType:
		f.meta["KeySpec"] = "ECC_NIST_P256"
	case sourcetest.SoftwareKey:
		f.meta["CustomKeyStoreId"] = "cks-1234567890abcdef0"
	case sourcetest.Size3072:
		k, _ := rsa.GenerateKey(rand.Reader, 3072)
		f.pub = &k.PublicKey
	case sourcetest.Exponent3:
		pub := f.key.PublicKey
		pub.E = 3
		f.pub = &pub
	case sourcetest.CanEncrypt:
		f.meta["KeyUsage"], f.usage = "ENCRYPT_DECRYPT", "ENCRYPT_DECRYPT"
	case sourcetest.NoPurpose:
		f.tags = f.tags[:1]
	case sourcetest.OtherVersion:
		f.meta["Arn"] = other
	case sourcetest.PublicKeyChange:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		f.pub = &k.PublicKey
	case sourcetest.SignOtherVersion:
		f.signARN = other
	case sourcetest.CorruptSignature:
		f.corrupt = true
	case sourcetest.Hang:
		f.hang.Store(true)
	case sourcetest.SecretInBody:
		f.errorBody = `{"__type":"AccessDenied` + sourcetest.Secret + `","message":"User ` + sourcetest.Secret + ` is not authorized"}`
	case sourcetest.SecretInSignBody:
		f.signError = `{"__type":"` + sourcetest.Secret + `","message":"` + sourcetest.Secret + `"}`
	case sourcetest.Transient:
		f.fail.Store(1)
	case sourcetest.AlwaysFail:
		f.fail.Store(1000)
	default: // NotYetValid, Exportable, ReleasePolicy, CertBacked: no such property in KMS
		return false
	}
	return true
}

func TestContract(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T) sourcetest.Harness { return &harness{} })
}

// The rest of spec 0004 phase 3's checks: another account, multi-Region keys, the reference grammar,
// the purpose an attestation key must carry, and what goes into the sign request.
func TestAWSChecks(t *testing.T) {
	open := func(t *testing.T, f *fakeKMS, multiRegion bool, purpose string) error {
		t.Helper()
		reg := keysource.NewRegistry(signer.Resolver{})
		if err := reg.Add("src", source(t, f, multiRegion), keysource.Options{Allow: []string{"*"}}); err != nil {
			t.Fatal(err)
		}
		_, err := reg.OpenPurpose(t.Context(), "src:"+keyID, purpose)
		return err
	}
	f := newFake(t)
	f.meta["AWSAccountId"] = "999999999999"
	if err := open(t, f, false, keysource.PurposeChannel); err == nil || !strings.Contains(err.Error(), "another account") {
		t.Errorf("another account: %v", err)
	}
	f = newFake(t)
	f.meta["MultiRegion"] = true
	if err := open(t, f, false, keysource.PurposeChannel); err == nil {
		t.Error("a multi-Region key opened without allow_multi_region")
	}
	if err := open(t, f, true, keysource.PurposeChannel); err != nil {
		t.Errorf("a multi-Region key with allow_multi_region: %v", err)
	}
	f = newFake(t)
	if err := open(t, f, false, keysource.PurposeAttestation); err == nil {
		t.Error("a channel-signing key opened as an attestation key")
	}
	f.tags[1]["TagValue"] = keysource.PurposeAttestation
	if err := open(t, f, false, keysource.PurposeAttestation); err != nil {
		t.Errorf("an attestation key: %v", err)
	}

	src := source(t, f, false)
	for ref, ok := range map[string]bool{keyID: true, "mrk-0123456789abcdef0123456789abcdef": false, "alias/kista": false, arn: false,
		strings.ToUpper(keyID): false, keyID + "/1": false, "": false} {
		if _, err := src.ParseKey(ref); (err == nil) != ok {
			t.Errorf("ParseKey(%q): %v", ref, err)
		}
	}
	if _, err := source(t, f, true).ParseKey("mrk-0123456789abcdef0123456789abcdef"); err != nil {
		t.Errorf("an mrk id with allow_multi_region: %v", err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("src", src, keysource.Options{Allow: []string{keyID}}); err != nil {
		t.Fatal(err)
	}
	f.tags[1]["TagValue"] = keysource.PurposeChannel
	sg, err := reg.Open(t.Context(), "src:"+keyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg.Sign(t.Context(), [32]byte{1}); err != nil {
		t.Fatal(err)
	}
	if f.lastSign["KeyId"] != arn {
		t.Errorf("signed with KeyId %v, want the built ARN", f.lastSign["KeyId"])
	}
}

// The ARN's partition follows the region: a key in China is asked for in aws-cn.
func TestPartitionARN(t *testing.T) {
	f := newFake(t)
	src, err := awskms.New("cn-north-1", account, false, credentials.NewStaticCredentialsProvider("AKID", "secret", ""),
		awskms.Options{Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("src", src, keysource.Options{Allow: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open(t.Context(), "src:"+keyID); err == nil {
		t.Fatal("opened a key the fake does not hold")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keyIDs) == 0 || f.keyIDs[0] != "arn:aws-cn:kms:cn-north-1:"+account+":key/"+keyID {
		t.Fatalf("asked for %v", f.keyIDs)
	}
}

// More refusals and bounds: GetPublicKey or Sign answers naming another key or algorithm, a state
// other than Enabled, no metadata, at most 3 attempts, and a credential failure as one fixed message.
func TestAWSAnswers(t *testing.T) {
	open := func(t *testing.T, f *fakeKMS, creds aws.CredentialsProvider) (signer.Signer, error) {
		t.Helper()
		if creds == nil {
			creds = credentials.NewStaticCredentialsProvider("AKID", "secret", "")
		}
		src, err := awskms.New(region, account, false, creds, awskms.Options{Endpoint: f.srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		reg := keysource.NewRegistry(signer.Resolver{})
		if err := reg.Add("src", src, keysource.Options{Allow: []string{"*"}}); err != nil {
			t.Fatal(err)
		}
		return reg.Open(t.Context(), "src:"+keyID)
	}
	for name, mut := range map[string]func(f *fakeKMS){
		"public key of another key": func(f *fakeKMS) { f.pkARN = other },
		"state updating":            func(f *fakeKMS) { f.meta["KeyState"] = "Updating" },
		"no metadata":               func(f *fakeKMS) { f.nilMeta = true },
	} {
		f := newFake(t)
		mut(f)
		if _, err := open(t, f, nil); !errors.Is(err, keysource.ErrKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the emulator's bare key id in GetPublicKey is accepted only with an endpoint override, which
	// every test has; NewWithOps (production's shape without one) refuses it
	f := newFake(t)
	f.pkARN = keyID
	if _, err := open(t, f, nil); err != nil {
		t.Errorf("the emulator's bare id: %v", err)
	}
	prod := awskms.NewWithOps(kms.New(kms.Options{Region: region, Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", ""),
		BaseEndpoint: aws.String(f.srv.URL)}), region, account, false)
	preg := keysource.NewRegistry(signer.Resolver{})
	if err := preg.Add("src", prod, keysource.Options{Allow: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := preg.Open(t.Context(), "src:"+keyID); !errors.Is(err, keysource.ErrKey) {
		t.Errorf("a bare id outside the emulator: %v", err)
	}
	f = newFake(t)
	sg, err := open(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.signAlg = "RSASSA_PSS_SHA_256"
	f.mu.Unlock()
	if _, err := sg.Sign(t.Context(), [32]byte{1}); !errors.Is(err, keysource.ErrSign) {
		t.Errorf("another signing algorithm: %v", err)
	}
	f = newFake(t)
	f.fail.Store(1000)
	if _, err := open(t, f, nil); err == nil {
		t.Fatal("opened while every attempt fails")
	}
	if n := f.requests.Load(); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
	f = newFake(t)
	_, err = open(t, f, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("sts: AccessDenied for role " + sourcetest.Secret)
	}))
	if err == nil || err.Error() != "awskms: the AWS credentials could not be obtained" {
		t.Errorf("a credential failure: %v", err)
	}
}
