// Package vault is the HashiCorp Vault / OpenBao Transit key source (spec 0004). Transit keys are
// software keys in Vault's barrier; the source is accepted outside development only with an explicit
// software_keys acknowledgment (config validation).
package vault

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
)

// Requester is the narrow Vault API the source uses (vaultapi.Client; fakes in tests).
type Requester interface {
	Do(ctx context.Context, method, path string, body, out any) error
}

// Source is one Transit mount.
type Source struct {
	api   Requester
	mount string
}

// New returns a source over a Vault client and a Transit mount.
func New(api Requester, mount string) *Source {
	return &Source{api: api, mount: strings.Trim(mount, "/")}
}

// Kind implements keysource.Backend.
func (s *Source) Kind() string { return "vault" }

var keyRef = regexp.MustCompile(`^([A-Za-z0-9_-]{1,128}):v([1-9][0-9]{0,8})$`)

// ParseKey parses "<key>:v<N>". v0 or a missing version would mean "latest" in Vault, so the
// version is required, positive and without leading zeros.
func (s *Source) ParseKey(ref string) (keysource.Key, error) {
	m := keyRef.FindStringSubmatch(ref)
	if m == nil {
		return keysource.Key{}, fmt.Errorf("vault key %q is not <key>:v<N> (N ≥ 1)", ref)
	}
	return keysource.Key{Name: m[1], Version: m[2]}, nil
}

type keyInfo struct {
	Type                 string                `json:"type"`
	SupportsSigning      bool                  `json:"supports_signing"`
	Exportable           bool                  `json:"exportable"`
	AllowPlaintextBackup bool                  `json:"allow_plaintext_backup"`
	ImportedKey          *bool                 `json:"imported_key"` // required: absent cannot be confirmed
	SoftDeleted          bool                  `json:"soft_deleted"` // OpenBao only
	LatestVersion        int                   `json:"latest_version"`
	MinEncryptionVersion int                   `json:"min_encryption_version"`
	Keys                 map[string]keyVersion `json:"keys"`
}

type keyVersion struct {
	PublicKey string `json:"public_key"`
}

// handle is an opened Transit key version.
type handle struct {
	s       *Source
	name    string
	version int
	pub     *rsa.PublicKey
}

// Open implements keysource.Backend.
func (s *Source) Open(ctx context.Context, k keysource.Key) (keysource.Handle, error) {
	v, _ := strconv.Atoi(k.Version)
	h := &handle{s: s, name: k.Name, version: v}
	pub, err := h.check(ctx)
	if err != nil {
		return nil, err
	}
	h.pub = pub
	if err := s.signOnly(ctx, k.Name, v); err != nil {
		return nil, err
	}
	return h, nil
}

// check reads the key and enforces the contract; it returns the pinned version's public key.
func (h *handle) check(ctx context.Context) (*rsa.PublicKey, error) {
	var out struct {
		Data keyInfo `json:"data"`
	}
	if err := h.s.api.Do(ctx, http.MethodGet, h.s.mount+"/keys/"+h.name, nil, &out); err != nil {
		return nil, describe(err)
	}
	d := out.Data
	switch {
	case d.Type != "rsa-2048":
		return nil, fmt.Errorf("%w: type %q, want rsa-2048", keysource.ErrKey, d.Type)
	case !d.SupportsSigning:
		return nil, fmt.Errorf("%w: the key cannot sign", keysource.ErrKey)
	case d.Exportable || d.AllowPlaintextBackup:
		return nil, fmt.Errorf("%w: the key is exportable or allows plaintext backup", keysource.ErrKey)
	case d.ImportedKey == nil:
		return nil, fmt.Errorf("%w: the server does not report imported_key, so non-imported material cannot be confirmed", keysource.ErrKey)
	case *d.ImportedKey:
		return nil, fmt.Errorf("%w: imported key material", keysource.ErrKey)
	case d.SoftDeleted:
		return nil, fmt.Errorf("%w: the key is soft-deleted", keysource.ErrKey)
	case h.version > d.LatestVersion || h.version < d.MinEncryptionVersion:
		return nil, fmt.Errorf("%w: version %d is outside the signable range", keysource.ErrKey, h.version)
	}
	kv, ok := d.Keys[strconv.Itoa(h.version)]
	if !ok || kv.PublicKey == "" {
		return nil, fmt.Errorf("%w: version %d is not available", keysource.ErrKey, h.version)
	}
	block, _ := pem.Decode([]byte(kv.PublicKey))
	if block == nil {
		return nil, fmt.Errorf("%w: unreadable public key", keysource.ErrKey)
	}
	pk, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable public key", keysource.ErrKey)
	}
	pub, ok := pk.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an RSA key", keysource.ErrKey)
	}
	if h.pub != nil && !h.pub.Equal(pub) {
		return nil, fmt.Errorf("%w: the version's public key changed", keysource.ErrKey)
	}
	return pub, nil
}

// signOnly checks, through the token's own capabilities, that kista can only read the key and sign
// with it: RSA Transit keys always report encryption support, so metadata cannot prove it. Every path
// must be in the answer as exactly ["deny"] (the key itself exactly ["read"]); anything missing or
// unexpected fails closed.
func (s *Source) signOnly(ctx context.Context, name string, version int) error {
	m, k, v := s.mount, name, strconv.Itoa(version)
	denied := []string{
		m + "/encrypt/" + k, m + "/decrypt/" + k, m + "/rewrap/" + k,
		m + "/datakey/plaintext/" + k, m + "/datakey/wrapped/" + k,
		m + "/backup/" + k, m + "/restore/" + k,
		m + "/keys/" + k + "/config", m + "/keys/" + k + "/rotate", m + "/keys/" + k + "/trim",
	}
	for _, typ := range []string{"encryption-key", "signing-key", "hmac-key"} {
		denied = append(denied, m+"/export/"+typ+"/"+k, m+"/export/"+typ+"/"+k+"/"+v, m+"/export/"+typ+"/"+k+"/latest")
	}
	keyPath := m + "/keys/" + k
	paths := append([]string{keyPath}, denied...)
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := s.api.Do(ctx, http.MethodPost, "sys/capabilities-self", map[string]any{"paths": paths}, &out); err != nil {
		return describe(err)
	}
	caps := func(p string) ([]string, bool) {
		raw, ok := out.Data[p]
		if !ok {
			return nil, false
		}
		var c []string
		if json.Unmarshal(raw, &c) != nil || len(c) == 0 {
			return nil, false
		}
		return c, true
	}
	if c, ok := caps(keyPath); !ok || len(c) != 1 || c[0] != "read" {
		return fmt.Errorf("%w: kista's token must have exactly read on %s, has %v", keysource.ErrKey, keyPath, c)
	}
	for _, p := range denied {
		c, ok := caps(p)
		if !ok {
			return fmt.Errorf("%w: the server did not report kista's capabilities on %s", keysource.ErrKey, p)
		}
		if len(c) != 1 || c[0] != "deny" {
			return fmt.Errorf("%w: kista's token may %v on %s; it must only read the key and sign", keysource.ErrKey, c, p)
		}
	}
	return nil
}

func (h *handle) Public() *rsa.PublicKey { return h.pub }

// Identity is "<key>:v<N>", the form the sign answer is checked against.
func (h *handle) Identity() string { return h.name + ":v" + strconv.Itoa(h.version) }

// Check implements keysource.Handle.
func (h *handle) Check(ctx context.Context) error {
	if _, err := h.check(ctx); err != nil {
		return err
	}
	return h.s.signOnly(ctx, h.name, h.version)
}

// Sign implements keysource.Handle: PKCS#1 v1.5 over the prehashed SHA-256 digest at the pinned
// version. The /sha2-256 path keeps the DigestInfo (never "none").
func (h *handle) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if len(digest) != 32 {
		return nil, "", errors.New("vault: the digest must be 32 bytes")
	}
	var out struct {
		Data struct {
			Signature  string `json:"signature"`
			KeyVersion int    `json:"key_version"`
		} `json:"data"`
	}
	body := map[string]any{
		"input":               base64.StdEncoding.EncodeToString(digest),
		"prehashed":           true,
		"signature_algorithm": "pkcs1v15",
		"key_version":         h.version,
	}
	if err := h.s.api.Do(ctx, http.MethodPost, h.s.mount+"/sign/"+h.name+"/sha2-256", body, &out); err != nil {
		return nil, "", describe(err)
	}
	prefix := "vault:v" + strconv.Itoa(out.Data.KeyVersion) + ":"
	if !strings.HasPrefix(out.Data.Signature, prefix) {
		return nil, "", errors.New("vault: unexpected signature format")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(out.Data.Signature, prefix))
	if err != nil {
		return nil, "", errors.New("vault: unexpected signature encoding")
	}
	return sig, h.name + ":v" + strconv.Itoa(out.Data.KeyVersion), nil
}

// describe reduces Vault errors to the status (never Vault's message or a body); a transport failure
// is "did not answer".
func describe(err error) error {
	var ve *vaultapi.Error
	if errors.As(err, &ve) {
		return fmt.Errorf("vault answered %d", ve.Status)
	}
	if errors.Is(err, vaultapi.ErrUnreachable) || errors.Is(err, context.DeadlineExceeded) {
		return vaultapi.ErrUnreachable
	}
	return err
}
