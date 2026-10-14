// Package gcpkms is the Google Cloud KMS key source (spec 0004, phase 4), over REST. Keys are
// ASYMMETRIC_SIGN, RSA_SIGN_PKCS1_2048_SHA256, enabled, HSM-protected (software only in
// development), not imported or external, and labelled kista-purpose with their role. Signing sends
// the digest with its CRC32C; the answer must be verified by KMS, carry the signature's CRC32C and
// name the pinned version.
package gcpkms

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/oauth2"

	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
)

// PurposeLabel is the label every key must carry, with the value its role names.
const PurposeLabel = "kista-purpose"

// Endpoint is Cloud KMS's REST endpoint.
const Endpoint = "https://cloudkms.googleapis.com/v1/"

// Source is one key ring.
type Source struct {
	keyRing    string
	base       string
	hc         *http.Client
	ts         oauth2.TokenSource
	requireHSM bool
}

// Options configure New.
type Options struct {
	// Endpoint replaces Cloud KMS's (the tests' fake, in development); empty in production.
	Endpoint string
}

// New returns a source for a key ring, with tokens from ts, calling through hc (nil: gcp's client).
func New(keyRing string, ts oauth2.TokenSource, requireHSM bool, hc *http.Client, o Options) *Source {
	if hc == nil {
		hc = gcpcloud.HTTPClient()
	}
	base := Endpoint
	if o.Endpoint != "" {
		base = o.Endpoint
		if base[len(base)-1] != '/' {
			base += "/"
		}
	}
	return &Source{keyRing: keyRing, base: base, hc: hc, ts: ts, requireHSM: requireHSM}
}

// Kind implements keysource.Backend.
func (s *Source) Kind() string { return "gcpkms" }

var keyRef = regexp.MustCompile(`^([A-Za-z0-9_-]{1,63})/([1-9][0-9]{0,9})$`)

// ParseKey parses "<key>/<version>"; the version is required.
func (s *Source) ParseKey(ref string) (keysource.Key, error) {
	m := keyRef.FindStringSubmatch(ref)
	if m == nil {
		return keysource.Key{}, fmt.Errorf("gcp key %q is not <key>/<version number>", ref)
	}
	return keysource.Key{Name: m[1], Version: m[2], Ref: ref}, nil
}

type handle struct {
	s       *Source
	key     string // the CryptoKey's resource name
	version string // the CryptoKeyVersion's
	purpose string
	pub     *rsa.PublicKey
}

// Open implements keysource.Backend.
func (s *Source) Open(ctx context.Context, k keysource.Key) (keysource.Handle, error) {
	purpose := k.Purpose
	if purpose == "" {
		purpose = keysource.PurposeChannel
	}
	key := s.keyRing + "/cryptoKeys/" + k.Name
	h := &handle{s: s, key: key, version: key + "/cryptoKeyVersions/" + k.Version, purpose: purpose}
	pub, err := h.check(ctx)
	if err != nil {
		return nil, err
	}
	h.pub = pub
	return h, nil
}

func (h *handle) Identity() string       { return h.version }
func (h *handle) Public() *rsa.PublicKey { return h.pub }

func (h *handle) Check(ctx context.Context) error {
	_, err := h.check(ctx)
	return err
}

var protections = map[string]bool{"HSM": true, "HSM_SINGLE_TENANT": true}

func (h *handle) protected(level string) bool {
	return protections[level] || level == "SOFTWARE" && !h.s.requireHSM
}

// check reads the version, the key and the public key, and enforces the contract.
func (h *handle) check(ctx context.Context) (*rsa.PublicKey, error) {
	refuse := func(format string, a ...any) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("%w: %s", keysource.ErrKey, fmt.Sprintf(format, a...))
	}
	var v struct {
		Name                           string          `json:"name"`
		State                          string          `json:"state"`
		Algorithm                      string          `json:"algorithm"`
		ProtectionLevel                string          `json:"protectionLevel"`
		ImportJob                      string          `json:"importJob"`
		ExternalProtectionLevelOptions json.RawMessage `json:"externalProtectionLevelOptions"`
	}
	if err := h.s.call(ctx, http.MethodGet, h.version, nil, &v); err != nil {
		return nil, err
	}
	switch {
	case v.Name != h.version:
		return refuse("KMS described another key version")
	case v.State != "ENABLED":
		return refuse("the key version is %s", word(v.State))
	case v.Algorithm != "RSA_SIGN_PKCS1_2048_SHA256":
		return refuse("algorithm %s (a channel key is RSA_SIGN_PKCS1_2048_SHA256)", word(v.Algorithm))
	case !h.protected(v.ProtectionLevel):
		return refuse("protection level %s (require_hsm accepts HSM and HSM_SINGLE_TENANT only)", word(v.ProtectionLevel))
	case v.ImportJob != "":
		return refuse("the key version was imported")
	case len(v.ExternalProtectionLevelOptions) > 0 && string(v.ExternalProtectionLevelOptions) != "null":
		return refuse("the key version is external")
	}
	var k struct {
		Name       string            `json:"name"`
		Purpose    string            `json:"purpose"`
		Labels     map[string]string `json:"labels"`
		ImportOnly bool              `json:"importOnly"`
	}
	if err := h.s.call(ctx, http.MethodGet, h.key, nil, &k); err != nil {
		return nil, err
	}
	if k.Name != h.key {
		return refuse("KMS described another key")
	}
	if k.Purpose != "ASYMMETRIC_SIGN" {
		return refuse("key purpose %s (only ASYMMETRIC_SIGN)", word(k.Purpose))
	}
	if k.ImportOnly {
		return refuse("the key takes imported versions only")
	}
	if k.Labels[PurposeLabel] != h.purpose {
		return refuse("the key lacks the label %s=%s", PurposeLabel, h.purpose)
	}
	var pk struct {
		Name            string `json:"name"`
		PEM             string `json:"pem"`
		PEMCRC32C       string `json:"pemCrc32c"`
		Algorithm       string `json:"algorithm"`
		ProtectionLevel string `json:"protectionLevel"`
	}
	if err := h.s.call(ctx, http.MethodGet, h.version+"/publicKey", nil, &pk); err != nil {
		return nil, err
	}
	if pk.Name != h.version || pk.Algorithm != v.Algorithm || pk.ProtectionLevel != v.ProtectionLevel {
		return refuse("KMS returned another version's public key")
	}
	if !crcMatches(pk.PEMCRC32C, []byte(pk.PEM)) {
		return refuse("the public key's CRC32C does not match")
	}
	blk, _ := pem.Decode([]byte(pk.PEM))
	if blk == nil {
		return refuse("the public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(blk.Bytes)
	pub, ok := parsed.(*rsa.PublicKey)
	if err != nil || !ok {
		return refuse("the public key is not RSA")
	}
	if h.pub != nil && !h.pub.Equal(pub) {
		return refuse("the version's public key changed")
	}
	return pub, nil
}

// Sign implements keysource.Handle: the digest with its CRC32C; a CRC mismatch is retried once
// (each try is a call of up to 3 attempts, all bounded by ctx).
func (h *handle) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if len(digest) != 32 {
		return nil, "", errors.New("gcpkms: the digest must be 32 bytes")
	}
	req := map[string]any{"digest": map[string]any{"sha256": base64.StdEncoding.EncodeToString(digest)},
		"digestCrc32c": strconv.FormatUint(uint64(crc(digest)), 10)}
	var lastErr error
	for range 2 {
		var out struct {
			Signature            string `json:"signature"`
			SignatureCRC32C      string `json:"signatureCrc32c"`
			VerifiedDigestCRC32C bool   `json:"verifiedDigestCrc32c"`
			Name                 string `json:"name"`
			ProtectionLevel      string `json:"protectionLevel"`
		}
		if err := h.s.call(ctx, http.MethodPost, h.version+":asymmetricSign", req, &out); err != nil {
			return nil, "", err
		}
		sig, err := base64.StdEncoding.DecodeString(out.Signature)
		switch {
		case err != nil:
			lastErr = errors.New("gcpkms: the signature is not base64")
		case !out.VerifiedDigestCRC32C:
			lastErr = errors.New("gcpkms: KMS did not verify the digest's CRC32C")
		case !crcMatches(out.SignatureCRC32C, sig):
			lastErr = errors.New("gcpkms: the signature's CRC32C does not match")
		case !h.protected(out.ProtectionLevel):
			return nil, "", fmt.Errorf("gcpkms: signed at protection level %s", word(out.ProtectionLevel))
		default:
			return sig, out.Name, nil
		}
	}
	return nil, "", lastErr
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// crcMatches reports whether the decimal CRC32C s is b's.
func crcMatches(s string, b []byte) bool {
	n, err := strconv.ParseUint(s, 10, 32)
	return err == nil && uint32(n) == crc(b)
}

var wordRe = regexp.MustCompile(`^[A-Z0-9_]{1,40}$`)

// word is a server's enum value, kept only in its expected shape.
func word(s string) string {
	if wordRe.MatchString(s) {
		return s
	}
	return "?"
}

// call makes one REST call with at most 3 attempts on 429 and 5xx; out is the JSON answer.
func (s *Source) call(ctx context.Context, method, name string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	tok, err := gcpcloud.Token(ctx, s.ts)
	switch {
	case errors.Is(err, gcpcloud.ErrPrincipal):
		return fmt.Errorf("gcpkms: %w", gcpcloud.ErrPrincipal)
	case ctx.Err() != nil:
		return describeCtx(ctx)
	case err != nil:
		return errors.New("gcpkms: the Google credentials could not be obtained")
	}
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			d := time.Duration(1<<attempt)*100*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond
			select {
			case <-ctx.Done():
				return describeCtx(ctx)
			case <-time.After(d):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, s.base+name, bytes.NewReader(body))
		if err != nil {
			return errors.New("gcpkms: an invalid request")
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return describeCtx(ctx)
			}
			lastErr = errors.New("gcpkms: Cloud KMS could not be reached")
			continue
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil { // the answer broke off: a transient failure, or the deadline
			if ctx.Err() != nil {
				return describeCtx(ctx)
			}
			lastErr = errors.New("gcpkms: Cloud KMS's answer broke off")
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK && rerr == nil:
			if err := json.Unmarshal(b, out); err != nil {
				return errors.New("gcpkms: Cloud KMS answered something else than JSON")
			}
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = describe(resp.StatusCode, b)
			continue
		}
		return describe(resp.StatusCode, b)
	}
	return lastErr
}

func describeCtx(ctx context.Context) error {
	return fmt.Errorf("gcpkms: Cloud KMS did not answer in time: %w", ctx.Err())
}

// describe keeps the HTTP status and Google's status word (PERMISSION_DENIED); never a message.
func describe(status int, body []byte) error {
	var e struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return fmt.Errorf("gcpkms: Cloud KMS answered %d %s", status, word(e.Error.Status))
}
