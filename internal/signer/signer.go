// Package signer signs DuckDB extension body hashes.
//
// A Signer signs only an extfile.BodyHash: the API cannot be used to sign arbitrary data, so a key
// configured as a channel key is never an oracle for anything else. Every implementation checks
// each signature it produces against its own public key before returning it.
package signer

import (
	"context"
	"crypto"
	"crypto/rsa"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// Signer signs body hashes of DuckDB extensions: RSA-2048, PKCS#1 v1.5, SHA-256 DigestInfo, with the
// composite hash as the precomputed digest (never hashed again).
type Signer interface {
	Sign(ctx context.Context, h extfile.BodyHash) ([]byte, error)
	Public() *rsa.PublicKey
	// ID is a stable reference for config and audit, never key material.
	ID() string
}

// checkSignature is the self-check every implementation runs on its result.
func checkSignature(pub *rsa.PublicKey, h extfile.BodyHash, sig []byte) error {
	if len(sig) != extfile.SignatureSize {
		return fmt.Errorf("signer: signature is %d bytes, want %d", len(sig), extfile.SignatureSize)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		return fmt.Errorf("signer: produced a signature that does not verify: %w", err)
	}
	return nil
}
