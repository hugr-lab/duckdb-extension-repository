// Package awskms is the AWS KMS key source (spec 0004, phase 3). Keys are RSA_2048, SIGN_VERIFY,
// enabled, of AWS_KMS origin (not imported, not in an external key store), single-region unless
// allowed, in the configured account and region, and tagged kista-purpose with their role.
// Signing is RSASSA_PKCS1_V1_5_SHA_256 over the digest; the answer's KeyId must be the key's ARN.
package awskms

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"

	awscloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/aws"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
)

// PurposeTag is the tag every key must carry, with the value its role names.
const PurposeTag = "kista-purpose"

// Ops is the narrow KMS API the source uses (kms.Client; fakes in tests).
type Ops interface {
	DescribeKey(ctx context.Context, in *kms.DescribeKeyInput, opts ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
	ListResourceTags(ctx context.Context, in *kms.ListResourceTagsInput, opts ...func(*kms.Options)) (*kms.ListResourceTagsOutput, error)
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, opts ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Sign(ctx context.Context, in *kms.SignInput, opts ...func(*kms.Options)) (*kms.SignOutput, error)
}

// Source is one account and region.
type Source struct {
	ops         Ops
	region      string
	account     string
	multiRegion bool
	emulator    bool // a development endpoint: moto answers GetPublicKey with the bare key id
}

// Options configure a client for New.
type Options struct {
	// Endpoint replaces KMS's endpoint (the emulator in development, tests); empty in production.
	Endpoint string
	FIPS     bool // KMS's FIPS endpoint
}

// New returns a source in a region and account, signing with creds; at most 3 attempts per call.
func New(region, account string, multiRegion bool, creds aws.CredentialsProvider, o Options) (*Source, error) {
	if !awscloud.ValidRegion(region) || !awscloud.ValidAccount(account) {
		return nil, errors.New("awskms: region and a 12-digit account are required")
	}
	if o.FIPS && o.Endpoint != "" {
		return nil, errors.New("awskms: fips and an endpoint override do not go together")
	}
	ko := kms.Options{Region: region, Credentials: credentialsOnly{creds}, HTTPClient: awscloud.HTTPClient(), RetryMaxAttempts: 3}
	if o.Endpoint != "" {
		ko.BaseEndpoint = aws.String(o.Endpoint)
	}
	if o.FIPS {
		ko.EndpointOptions.UseFIPSEndpoint = aws.FIPSEndpointStateEnabled
	}
	s := NewWithOps(kms.New(ko), region, account, multiRegion)
	s.emulator = o.Endpoint != ""
	return s, nil
}

// errCredentials is any failure to get the identity's credentials (STS refusing the role, the
// platform endpoint failing): one fixed message, never STS's answer reported as KMS's.
var errCredentials = errors.New("awskms: the AWS credentials could not be obtained")

// credentialsOnly maps every credential failure to errCredentials, unwrapped.
type credentialsOnly struct{ p aws.CredentialsProvider }

func (c credentialsOnly) Retrieve(ctx context.Context) (aws.Credentials, error) {
	v, err := c.p.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, errCredentials
	}
	return v, nil
}

// NewWithOps returns a source over any Ops (tests).
func NewWithOps(ops Ops, region, account string, multiRegion bool) *Source {
	return &Source{ops: ops, region: region, account: account, multiRegion: multiRegion}
}

// Kind implements keysource.Backend.
func (s *Source) Kind() string { return "awskms" }

var (
	keyID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	mrkID  = regexp.MustCompile(`^mrk-[0-9a-f]{32}$`)
	codeRe = regexp.MustCompile(`^[A-Z][A-Za-z]{1,63}$`) // AWS error codes: AccessDeniedException
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ParseKey parses a key id: a UUID, or a multi-Region key id (mrk-…) when the source allows them.
// Aliases and ARNs are refused: an alias can be retargeted, and an ARN would name an account or
// region the config did not.
func (s *Source) ParseKey(ref string) (keysource.Key, error) {
	switch {
	case keyID.MatchString(ref):
	case mrkID.MatchString(ref):
		if !s.multiRegion {
			return keysource.Key{}, fmt.Errorf("aws key %q is a multi-Region key; the source does not allow them", ref)
		}
	default:
		return keysource.Key{}, fmt.Errorf("aws key %q is not a key id (a UUID, or mrk-<32 hex>)", ref)
	}
	return keysource.Key{Name: ref, Version: ref, Ref: ref}, nil
}

// arn is the key's ARN, built from config and the parsed id only.
func (s *Source) arn(id string) string {
	return "arn:" + awscloud.Partition(s.region) + ":kms:" + s.region + ":" + s.account + ":key/" + id
}

type handle struct {
	s       *Source
	id      string
	arn     string
	purpose string
	pub     *rsa.PublicKey
}

// Open implements keysource.Backend.
func (s *Source) Open(ctx context.Context, k keysource.Key) (keysource.Handle, error) {
	purpose := k.Purpose
	if purpose == "" {
		purpose = keysource.PurposeChannel
	}
	h := &handle{s: s, id: k.Name, arn: s.arn(k.Name), purpose: purpose}
	pub, err := h.check(ctx)
	if err != nil {
		return nil, err
	}
	h.pub = pub
	return h, nil
}

func (h *handle) Identity() string       { return h.arn }
func (h *handle) Public() *rsa.PublicKey { return h.pub }

func (h *handle) Check(ctx context.Context) error {
	_, err := h.check(ctx)
	return err
}

// check reads the key's metadata, tags and public key and enforces the contract.
func (h *handle) check(ctx context.Context) (*rsa.PublicKey, error) {
	refuse := func(format string, a ...any) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("%w: %s", keysource.ErrKey, fmt.Sprintf(format, a...))
	}
	d, err := h.s.ops.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(h.arn)})
	if err != nil {
		return nil, describe(err)
	}
	m := d.KeyMetadata
	if m == nil {
		return refuse("KMS returned no key metadata")
	}
	switch {
	case aws.ToString(m.Arn) != h.arn:
		return refuse("KMS described another key")
	case aws.ToString(m.AWSAccountId) != h.s.account:
		return refuse("the key is in another account")
	case m.KeySpec != types.KeySpecRsa2048:
		return refuse("key spec %s (a channel key is RSA_2048)", enum(m.KeySpec))
	case m.KeyUsage != types.KeyUsageTypeSignVerify:
		return refuse("key usage %s (only SIGN_VERIFY)", enum(m.KeyUsage))
	case m.KeyState != types.KeyStateEnabled || !m.Enabled:
		return refuse("the key is %s", enum(m.KeyState))
	case m.Origin != types.OriginTypeAwsKms:
		return refuse("key origin %s (only AWS_KMS: not imported, not an external key store)", enum(m.Origin))
	case m.CustomKeyStoreId != nil:
		return refuse("the key is in a custom key store")
	case aws.ToBool(m.MultiRegion) && !h.s.multiRegion:
		return refuse("the key is multi-Region; the source does not allow it")
	case m.ValidTo != nil:
		return refuse("the key has an expiration (imported material)")
	}
	tagged := false
	for page, in := 0, (&kms.ListResourceTagsInput{KeyId: aws.String(h.arn)}); ; page++ {
		if page == maxTagPages { // AWS holds at most 50 tags, one page
			return refuse("KMS kept paging the key's tags")
		}
		out, err := h.s.ops.ListResourceTags(ctx, in)
		if err != nil {
			return nil, describe(err)
		}
		for _, t := range out.Tags {
			if aws.ToString(t.TagKey) == PurposeTag && aws.ToString(t.TagValue) == h.purpose {
				tagged = true
			}
		}
		if !out.Truncated || out.NextMarker == nil {
			break
		}
		in = &kms.ListResourceTagsInput{KeyId: aws.String(h.arn), Marker: out.NextMarker}
	}
	if !tagged {
		return refuse("the key lacks the tag %s=%s", PurposeTag, h.purpose)
	}
	pk, err := h.s.ops.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(h.arn)})
	if err != nil {
		return nil, describe(err)
	}
	switch {
	case aws.ToString(pk.KeyId) != h.arn && (!h.s.emulator || aws.ToString(pk.KeyId) != h.id): // AWS answers the ARN, moto the id
		return refuse("KMS returned another key's public key")
	case pk.KeyUsage != types.KeyUsageTypeSignVerify:
		return refuse("the public key's usage is %s", enum(pk.KeyUsage))
	case !slices.Contains(pk.SigningAlgorithms, types.SigningAlgorithmSpecRsassaPkcs1V15Sha256):
		return refuse("the key does not sign RSASSA_PKCS1_V1_5_SHA_256")
	}
	parsed, err := x509.ParsePKIXPublicKey(pk.PublicKey)
	pub, ok := parsed.(*rsa.PublicKey)
	if err != nil || !ok {
		return refuse("the public key is not RSA")
	}
	if h.pub != nil && !h.pub.Equal(pub) {
		return refuse("the key's public key changed")
	}
	return pub, nil
}

// Sign implements keysource.Handle: PKCS#1 v1.5 with SHA-256 over the precomputed digest.
func (h *handle) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if len(digest) != 32 {
		return nil, "", errors.New("awskms: the digest must be 32 bytes")
	}
	out, err := h.s.ops.Sign(ctx, &kms.SignInput{KeyId: aws.String(h.arn), Message: digest, MessageType: types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecRsassaPkcs1V15Sha256})
	if err != nil {
		return nil, "", describe(err)
	}
	if out.SigningAlgorithm != types.SigningAlgorithmSpecRsassaPkcs1V15Sha256 {
		return nil, "", errors.New("awskms: the answer names another signing algorithm")
	}
	return out.Signature, aws.ToString(out.KeyId), nil
}

// maxTagPages bounds ListResourceTags paging.
const maxTagPages = 5

var enumRe = regexp.MustCompile(`^[A-Z0-9_]{1,40}$`)

// enum is a server's enum value, kept only in its expected shape.
func enum[T ~string](v T) string { return clean(enumRe, string(v)) }

// clean keeps a server-chosen value only if it has the expected shape.
func clean(re *regexp.Regexp, s string) string {
	if re.MatchString(s) {
		return s
	}
	return "?"
}

// describe keeps the AWS error code (letters only) and the request id (a UUID); both come from the
// server, so anything of another shape is replaced. Never a message or an ARN; anything else
// (no answer, no credentials) is one fixed message.
func describe(err error) error {
	if errors.Is(err, errCredentials) {
		return errCredentials
	}
	reqID := "?"
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		reqID = clean(uuidRe, re.ServiceRequestID())
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return fmt.Errorf("awskms: KMS answered %s (request %s)", clean(codeRe, ae.ErrorCode()), reqID)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("awskms: KMS did not answer in time")
	}
	return errors.New("awskms: KMS could not be reached, or no AWS credentials are available")
}
