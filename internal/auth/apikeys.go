package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// API keys (spec 0008 phase 2): kista_<prefix>_<secret>, the prefix 8 random hex characters
// (shown in lists, never used for lookup), the secret 32 random bytes in lowercase base32. Only the
// SHA-256 of the whole key is stored; the secret is random, so a slow hash adds nothing.

const apiKeyPrefix = "kista_"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IsAPIKey reports whether a Bearer is an API key (looked up only on the routes publishers use).
func IsAPIKey(tok string) bool { return strings.HasPrefix(tok, apiKeyPrefix) }

// NewAPIKey makes a key: the key itself (shown once), its prefix and its hash.
func NewAPIKey() (key, prefix, hash string, err error) {
	var p [4]byte
	var s [32]byte
	if _, err := rand.Read(p[:]); err != nil {
		return "", "", "", err
	}
	if _, err := rand.Read(s[:]); err != nil {
		return "", "", "", err
	}
	prefix = hex.EncodeToString(p[:])
	key = apiKeyPrefix + prefix + "_" + strings.ToLower(b32.EncodeToString(s[:]))
	return key, prefix, APIKeyHash(key), nil
}

// APIKeyHash is the stored form of a key.
func APIKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// WellFormedAPIKey reports whether a Bearer has the shape of a key kista issues (others are not
// looked up at all).
func WellFormedAPIKey(tok string) bool {
	rest, ok := strings.CutPrefix(tok, apiKeyPrefix)
	if !ok || len(rest) != 8+1+52 || rest[8] != '_' {
		return false
	}
	if _, err := hex.DecodeString(rest[:8]); err != nil {
		return false
	}
	_, err := b32.DecodeString(strings.ToUpper(rest[9:]))
	return err == nil && strings.ToLower(rest[9:]) == rest[9:]
}
