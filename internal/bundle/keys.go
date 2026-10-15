// Package bundle is spec 0014's format: the plan a site writes, the manifest a connected kista
// exports, the bundle key that signs it, and the tar that carries them with the extension files.
// It reads and writes; what a bundle may release is the import's business.
package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// signContext is prefixed to the manifest's bytes before signing: a bundle key's signature can
// never be mistaken for one over anything else.
const signContext = "kista-bundle-v1\x00"

// ErrSignature is a manifest whose signature does not verify with the bundle's key.
var ErrSignature = errors.New("bundle: the manifest's signature does not verify")

// GenerateKey makes a bundle key: the private key as PKCS#8 PEM, and its fingerprint.
func GenerateKey() (privatePEM []byte, fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), Fingerprint(pub), nil
}

// ParsePrivateKey reads a bundle key: PKCS#8 PEM, Ed25519 only.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "PRIVATE KEY" {
		return nil, errors.New("bundle: a bundle key is a PKCS#8 PEM \"PRIVATE KEY\"")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("bundle: the bundle key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("bundle: a bundle key is Ed25519")
	}
	return priv, nil
}

// MarshalPublicKey returns a bundle key's public key as SPKI PEM, the bundle's kista-bundle.pub.
func MarshalPublicKey(pub ed25519.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(pub) // never fails for Ed25519
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// ParsePublicKey reads kista-bundle.pub: one SPKI PEM block, Ed25519 only, nothing after it.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	b, rest := pem.Decode(data)
	if b == nil || b.Type != "PUBLIC KEY" || len(trimSpace(rest)) != 0 {
		return nil, errors.New("bundle: kista-bundle.pub is one PEM \"PUBLIC KEY\"")
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("bundle: kista-bundle.pub: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("bundle: kista-bundle.pub is not an Ed25519 key")
	}
	return pub, nil
}

// Fingerprint is "sha256:" and the hex SHA-256 of the key's SPKI DER, re-marshalled from the parsed
// key (never the PEM's bytes), as kista fingerprints every key.
func Fingerprint(pub ed25519.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Sign signs a manifest's exact bytes: 64 raw bytes, kista-bundle.sig.
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	return ed25519.Sign(priv, signed(manifest))
}

// Verify checks a manifest's signature.
func Verify(pub ed25519.PublicKey, manifest, sig []byte) error {
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, signed(manifest), sig) {
		return ErrSignature
	}
	return nil
}

func signed(manifest []byte) []byte {
	return append([]byte(signContext), manifest...)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}
