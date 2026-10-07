package extfile

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// KeyBits is the only RSA key size DuckDB accepts: it requires a 256-byte signature.
const KeyBits = 2048

var oidRSAEncryption = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}

// ParsePublicKey parses a public key in one of the two forms DuckDB accepts
// (src/main/extension/extension_repository_manager.cpp, ToCompactPublicKey): an SPKI PEM block
// ("-----BEGIN PUBLIC KEY-----") or the base64 of SPKI DER. Any other PEM type, PKCS#1
// "RSA PUBLIC KEY" included, is refused as DuckDB refuses it, and so are PEM headers. It is stricter
// than DuckDB in a few ways, so that what kista accepts DuckDB accepts too: exactly one PEM block
// with nothing before or after it, no whitespace inside the base64 form, and NULL algorithm
// parameters. The key must be RSA with the rsaEncryption OID, 2048 bits and e = 65537.
func ParsePublicKey(s string) (*rsa.PublicKey, error) {
	s = strings.TrimSpace(s)
	var der []byte
	if strings.HasPrefix(s, "-----BEGIN") {
		block, rest := pem.Decode([]byte(s))
		if block == nil {
			return nil, errors.New("extfile: invalid PEM")
		}
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("extfile: PEM type %q: DuckDB accepts only SPKI (PUBLIC KEY)", block.Type)
		}
		if len(block.Headers) != 0 {
			return nil, errors.New("extfile: PEM headers are not accepted by DuckDB")
		}
		if len(strings.TrimSpace(string(rest))) != 0 {
			return nil, errors.New("extfile: data after the PEM block")
		}
		der = block.Bytes
	} else {
		if strings.ContainsAny(s, " \t\r\n") {
			return nil, errors.New("extfile: whitespace inside the base64 key")
		}
		var err error
		der, err = base64.StdEncoding.Strict().DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("extfile: key is neither SPKI PEM nor base64 DER: %w", err)
		}
	}
	return parseSPKI(der)
}

func parseSPKI(der []byte) (*rsa.PublicKey, error) {
	var spki struct {
		Algorithm struct {
			Algorithm  asn1.ObjectIdentifier
			Parameters asn1.RawValue `asn1:"optional"`
		}
		PublicKey asn1.BitString
	}
	rest, err := asn1.Unmarshal(der, &spki)
	if err != nil || len(rest) != 0 {
		return nil, errors.New("extfile: invalid SPKI DER")
	}
	if !spki.Algorithm.Algorithm.Equal(oidRSAEncryption) {
		return nil, fmt.Errorf("extfile: key algorithm %v is not rsaEncryption", spki.Algorithm.Algorithm)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("extfile: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("extfile: not an RSA key")
	}
	if err := CheckKey(rsaPub); err != nil {
		return nil, err
	}
	return rsaPub, nil
}

// CheckKey checks that a key is one DuckDB can verify with: RSA-2048, e = 65537.
func CheckKey(pub *rsa.PublicKey) error {
	if pub == nil || pub.N == nil {
		return errors.New("extfile: no key")
	}
	if pub.N.BitLen() != KeyBits {
		return fmt.Errorf("extfile: RSA key is %d bits, DuckDB requires %d", pub.N.BitLen(), KeyBits)
	}
	if pub.E != 65537 {
		return fmt.Errorf("extfile: RSA exponent %d, expected 65537", pub.E)
	}
	return nil
}

// MarshalPublicKeyPEM returns the key as SPKI PEM, a form DuckDB accepts in signature_keys and
// USING PUBLIC KEY.
func MarshalPublicKeyPEM(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// MarshalPublicKeyBase64 returns the key as base64 SPKI DER, the other form DuckDB accepts.
func MarshalPublicKeyBase64(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// Fingerprint returns "sha256:" and the lowercase hex SHA-256 of the key's SPKI DER: the string
// CREATE EXTENSION REPOSITORY reports (extension_repository_manager.cpp, GetPublicKeyFingerprint).
// It returns "" for a key that fails CheckKey.
func Fingerprint(pub *rsa.PublicKey) string {
	if CheckKey(pub) != nil {
		return ""
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DedupKeys removes keys that have the same fingerprint, keeping the first, and drops keys that
// fail CheckKey.
func DedupKeys(keys []*rsa.PublicKey) []*rsa.PublicKey {
	seen := make(map[string]bool, len(keys))
	out := keys[:0:0]
	for _, k := range keys {
		fp := Fingerprint(k)
		if fp != "" && !seen[fp] {
			seen[fp] = true
			out = append(out, k)
		}
	}
	return out
}

// Verify checks a signature over a body hash against keys, as DuckDB does: RSA PKCS#1 v1.5 with the
// SHA-256 DigestInfo over the 32-byte composite hash (mbedtls_pk_verify with MD_SHA256). It returns
// the fingerprint of the first key that verifies. Keys that fail CheckKey are skipped.
func Verify(h BodyHash, sig []byte, keys []*rsa.PublicKey) (string, bool) {
	if len(sig) != SignatureSize {
		return "", false
	}
	for _, k := range keys {
		if CheckKey(k) != nil {
			continue
		}
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig) == nil {
			return Fingerprint(k), true
		}
	}
	return "", false
}
