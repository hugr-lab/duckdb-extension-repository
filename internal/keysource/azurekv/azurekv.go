// Package azurekv is the Azure Key Vault / Managed HSM key source (spec 0004, phase 2). Keys are
// RSA-HSM (RSA only when require_hsm is off, in development), 2048 bits, enabled, sign-only, not
// exportable, without a release policy, not certificate-backed, and tagged kista-purpose =
// channel-signing. Signing is RS256 over the digest; the answer's kid must name the pinned version.
package azurekv

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
)

// PurposeTag is the tag every key must carry.
const (
	PurposeTag   = "kista-purpose"
	PurposeValue = "channel-signing"
)

// Ops is the narrow Key Vault API the source uses (azkeys.Client; fakes in tests).
type Ops interface {
	GetKey(ctx context.Context, name, version string, opts *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error)
	Sign(ctx context.Context, name, version string, params azkeys.SignParameters, opts *azkeys.SignOptions) (azkeys.SignResponse, error)
}

// Source is one vault or Managed HSM.
type Source struct {
	ops        Ops
	host       string // lowercase, e.g. kista-prod.vault.azure.net
	requireHSM bool
	now        func() time.Time
}

// Options configure a client for New.
type Options struct {
	// Transport replaces the HTTP transport (tests point it at a fake); production leaves it nil.
	Transport policy.Transporter
	Timeout   time.Duration
}

// New returns a source for https://<host>, built on azkeys with challenge-resource verification on
// and at most 3 attempts per call.
func New(host string, cred azcore.TokenCredential, requireHSM bool, o Options) (*Source, error) {
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	co := azcore.ClientOptions{
		Retry:     policy.RetryOptions{MaxRetries: 2, TryTimeout: o.Timeout, MaxRetryDelay: 2 * time.Second},
		Transport: o.Transport,
	}
	c, err := azkeys.NewClient("https://"+host, cred, &azkeys.ClientOptions{ClientOptions: co})
	if err != nil {
		return nil, fmt.Errorf("azurekv: %w", err)
	}
	return NewWithOps(c, host, requireHSM), nil
}

// NewWithOps returns a source over any Ops (tests).
func NewWithOps(ops Ops, host string, requireHSM bool) *Source {
	return &Source{ops: ops, host: strings.ToLower(host), requireHSM: requireHSM, now: time.Now}
}

// Kind implements keysource.Backend.
func (s *Source) Kind() string { return "azurekv" }

var keyRef = regexp.MustCompile(`^([0-9A-Za-z-]{1,127})/([0-9a-f]{32})$`)

// ParseKey parses "<name>/<version>". The version is required (empty would mean "latest"). Key Vault
// names are case-insensitive, so the allow list is matched on the lowercased name.
func (s *Source) ParseKey(ref string) (keysource.Key, error) {
	m := keyRef.FindStringSubmatch(ref)
	if m == nil {
		return keysource.Key{}, fmt.Errorf("azure key %q is not <name>/<32-hex version>", ref)
	}
	return keysource.Key{Name: strings.ToLower(m[1]), Version: m[2], Ref: ref}, nil
}

type handle struct {
	s       *Source
	name    string // as written in the reference
	version string
	pub     *rsa.PublicKey
}

// Open implements keysource.Backend.
func (s *Source) Open(ctx context.Context, k keysource.Key) (keysource.Handle, error) {
	_, rest, _ := strings.Cut(k.Ref, ":")
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		name = k.Name
	}
	h := &handle{s: s, name: name, version: k.Version}
	pub, err := h.check(ctx)
	if err != nil {
		return nil, err
	}
	h.pub = pub
	return h, nil
}

// identity is the comparable form of a key id: lowercase host and name, exact version.
func identity(host, name, version string) string {
	return strings.ToLower(host) + "/keys/" + strings.ToLower(name) + "/" + version
}

// parseKID turns https://<host>/keys/<name>/<version> into its identity.
func parseKID(kid *azkeys.ID) (string, bool) {
	if kid == nil {
		return "", false
	}
	s := string(*kid)
	rest, ok := strings.CutPrefix(s, "https://")
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "keys" || parts[3] == "" {
		return "", false
	}
	return identity(parts[0], parts[2], parts[3]), true
}

func (h *handle) Identity() string { return identity(h.s.host, h.name, h.version) }

func (h *handle) Public() *rsa.PublicKey { return h.pub }

func (h *handle) Check(ctx context.Context) error {
	_, err := h.check(ctx)
	return err
}

// check reads the key at the pinned version and enforces the contract.
func (h *handle) check(ctx context.Context) (*rsa.PublicKey, error) {
	resp, err := h.s.ops.GetKey(ctx, h.name, h.version, nil)
	if err != nil {
		return nil, describe(err)
	}
	b := resp.KeyBundle
	refuse := func(format string, a ...any) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("%w: %s", keysource.ErrKey, fmt.Sprintf(format, a...))
	}
	if b.Key == nil || b.Attributes == nil {
		return refuse("the vault returned no key or attributes")
	}
	if got, ok := parseKID(b.Key.KID); !ok || got != h.Identity() {
		return refuse("the vault returned another key version")
	}
	kty := ""
	if b.Key.Kty != nil {
		kty = string(*b.Key.Kty)
	}
	switch {
	case kty == string(azkeys.KeyTypeRSAHSM):
	case kty == string(azkeys.KeyTypeRSA) && !h.s.requireHSM:
	default:
		return refuse("key type %q (require_hsm accepts RSA-HSM only)", kty)
	}
	a := b.Attributes
	now := h.s.now()
	switch {
	case a.Enabled == nil || !*a.Enabled:
		return refuse("the key is disabled")
	case a.NotBefore != nil && now.Before(*a.NotBefore):
		return refuse("the key is not valid yet")
	case a.Expires != nil && !now.Before(*a.Expires):
		return refuse("the key has expired")
	case a.Exportable != nil && *a.Exportable:
		return refuse("the key is exportable")
	case b.ReleasePolicy != nil:
		return refuse("the key has a release policy")
	case b.Managed != nil && *b.Managed:
		return refuse("the key backs a certificate")
	}
	var canSign bool
	for _, op := range b.Key.KeyOps {
		switch {
		case op == nil:
		case *op == azkeys.KeyOperationSign:
			canSign = true
		case *op == azkeys.KeyOperationVerify:
		default:
			return refuse("key_ops include %q; a channel key may only sign (and verify)", *op)
		}
	}
	if !canSign {
		return refuse("key_ops do not include sign")
	}
	if v, ok := b.Tags[PurposeTag]; !ok || v == nil || *v != PurposeValue {
		return refuse("the key lacks the tag %s=%s", PurposeTag, PurposeValue)
	}
	if len(b.Key.N) == 0 || len(b.Key.E) == 0 || len(b.Key.E) > 4 {
		return refuse("the key has no usable RSA public key")
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(b.Key.N), E: int(new(big.Int).SetBytes(b.Key.E).Int64())}
	if h.pub != nil && !h.pub.Equal(pub) {
		return refuse("the version's public key changed")
	}
	return pub, nil
}

// Sign implements keysource.Handle: RS256 over the precomputed digest at the pinned version.
func (h *handle) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if len(digest) != 32 {
		return nil, "", errors.New("azurekv: the digest must be 32 bytes")
	}
	alg := azkeys.SignatureAlgorithmRS256
	resp, err := h.s.ops.Sign(ctx, h.name, h.version, azkeys.SignParameters{Algorithm: &alg, Value: digest}, nil)
	if err != nil {
		return nil, "", describe(err)
	}
	reported, ok := parseKID(resp.KID)
	if !ok {
		return nil, "", errors.New("azurekv: the sign answer names no key")
	}
	return resp.Result, reported, nil
}

var (
	errorCode = regexp.MustCompile(`^[A-Z][A-Za-z]{1,63}$`)                                                         // Azure codes: Forbidden, KeyNotFound
	requestID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`) // a GUID
)

// clean keeps a server-chosen value only if it has the expected shape.
func clean(re *regexp.Regexp, s string) string {
	if re.MatchString(s) {
		return s
	}
	return "?"
}

// describe keeps the HTTP status, the Azure error code (letters only) and the request id (a GUID);
// both come from the server, so anything of another shape is replaced. Never a body or URL; an
// authentication failure is one fixed message.
func describe(err error) error {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		reqID := ""
		if re.RawResponse != nil {
			reqID = re.RawResponse.Header.Get("x-ms-request-id")
		}
		return fmt.Errorf("azurekv: the vault answered %d %s (request %s)", re.StatusCode, clean(errorCode, re.ErrorCode), clean(requestID, reqID))
	}
	var af *azidentity.AuthenticationFailedError
	if errors.As(err, &af) {
		return errors.New("azurekv: the Azure credential did not get a token")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("azurekv: the vault did not answer in time")
	}
	return errors.New("azurekv: the vault could not be reached, or no Azure credential is available")
}
