// Package auth verifies Bearer tokens against a tenant's issuer records and computes the caller's
// principals (spec 0006).
package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256" // hashes used by the RS/PS/ES algorithms
	_ "crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// MaxToken is the largest token accepted.
const MaxToken = 16 << 10

// Algorithms an issuer record may accept; never none or HS*.
var Algorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "EdDSA"}

var errMalformed = errors.New("auth: malformed token")

// jws is a parsed compact JWS (not yet verified).
type jws struct {
	alg, kid   string
	claims     map[string]any
	input, sig []byte
}

func b64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=+/") {
		return nil, errMalformed
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// parse splits and decodes a compact JWS, refusing anything else: a JWE, the JSON serialisation,
// duplicate JSON keys, crit, typ other than JWT or at+jwt, a missing kid.
func parse(tok string) (*jws, error) {
	if len(tok) > MaxToken {
		return nil, errMalformed
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errMalformed
	}
	hb, err := b64(parts[0])
	if err != nil {
		return nil, errMalformed
	}
	pb, err := b64(parts[1])
	if err != nil {
		return nil, errMalformed
	}
	sig, err := b64(parts[2])
	if err != nil {
		return nil, errMalformed
	}
	var hdr map[string]any
	if err := strictJSON(hb, &hdr); err != nil {
		return nil, errMalformed
	}
	alg, _ := hdr["alg"].(string)
	kid, _ := hdr["kid"].(string)
	if alg == "" || kid == "" {
		return nil, errMalformed
	}
	if _, ok := hdr["crit"]; ok {
		return nil, errMalformed
	}
	if typ, ok := hdr["typ"]; ok {
		s, _ := typ.(string)
		if !strings.EqualFold(s, "JWT") && !strings.EqualFold(s, "at+jwt") && !strings.EqualFold(s, "application/at+jwt") {
			return nil, errMalformed
		}
	}
	// jku, x5u, jwk and x5c are ignored: keys come only from the issuer record's JWKS
	var claims map[string]any
	if err := strictJSON(pb, &claims); err != nil {
		return nil, errMalformed
	}
	return &jws{alg: alg, kid: kid, claims: claims, input: []byte(parts[0] + "." + parts[1]), sig: sig}, nil
}

// strictJSON decodes an object, refusing duplicate keys at any depth and trailing data.
func strictJSON(b []byte, v any) error {
	if err := noDuplicates(json.NewDecoder(bytes.NewReader(b))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// NoDuplicateKeys refuses JSON with a duplicate object key at any depth (or too deep).
func NoDuplicateKeys(b []byte) error { return noDuplicates(json.NewDecoder(bytes.NewReader(b))) }

func noDuplicates(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	return walk(d, t, 0)
}

func walk(d *json.Decoder, t json.Token, depth int) error {
	if depth > 32 {
		return errors.New("too deep")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			if seen[key] {
				return errors.New("duplicate key")
			}
			seen[key] = true
			v, err := d.Token()
			if err != nil {
				return err
			}
			if err := walk(d, v, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			v, err := d.Token()
			if err != nil {
				return err
			}
			if err := walk(d, v, depth+1); err != nil {
				return err
			}
		}
	}
	_, err := d.Token() // the closing delimiter
	return err
}

// jwk is a parsed public key from a JWKS.
type jwk struct {
	kid, alg string
	rsa      *rsa.PublicKey
	ec       *ecdsa.PublicKey
	ed       ed25519.PublicKey
}

type rawJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// parseJWK parses one key; keys that are unusable for signature verification are refused.
func parseJWK(r rawJWK) (jwk, error) {
	k := jwk{kid: r.Kid, alg: r.Alg}
	if r.Kid == "" || r.Use != "" && r.Use != "sig" || r.D != "" {
		return k, errors.New("not a public signing key with a kid")
	}
	switch r.Kty {
	case "RSA":
		n, err1 := b64(r.N)
		e, err2 := b64(r.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			return k, errors.New("bad RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if bits := pub.N.BitLen(); bits < 2048 || bits > 4096 || pub.E < 3 || pub.E%2 == 0 {
			return k, errors.New("RSA keys are 2048-4096 bits")
		}
		k.rsa = pub
	case "EC":
		var curve elliptic.Curve
		size := 0
		switch r.Crv {
		case "P-256":
			curve, size = elliptic.P256(), 32
		case "P-384":
			curve, size = elliptic.P384(), 48
		default:
			return k, errors.New("unsupported curve")
		}
		x, err1 := b64(r.X)
		y, err2 := b64(r.Y)
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return k, errors.New("bad EC key")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
		if err != nil {
			return k, errors.New("bad EC key")
		}
		k.ec = pub
	case "OKP":
		x, err := b64(r.X)
		if r.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return k, errors.New("bad OKP key")
		}
		k.ed = ed25519.PublicKey(x)
	default:
		return k, errors.New("unsupported key type")
	}
	return k, nil
}

// verify checks the signature with key k for the header's alg: the key's type and curve must be
// the algorithm's, and its alg (if any) the same.
func (t *jws) verify(k jwk) error {
	if k.alg != "" && k.alg != t.alg {
		return errors.New("the key is for another algorithm")
	}
	hash := map[string]crypto.Hash{"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512,
		"PS256": crypto.SHA256, "PS384": crypto.SHA384, "PS512": crypto.SHA512, "ES256": crypto.SHA256, "ES384": crypto.SHA384}
	switch t.alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		if k.rsa == nil {
			return errors.New("not an RSA key")
		}
		h := hash[t.alg].New()
		h.Write(t.input)
		if t.alg[0] == 'R' {
			return rsa.VerifyPKCS1v15(k.rsa, hash[t.alg], h.Sum(nil), t.sig)
		}
		return rsa.VerifyPSS(k.rsa, hash[t.alg], h.Sum(nil), t.sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256", "ES384":
		size := map[string]int{"ES256": 32, "ES384": 48}[t.alg]
		if k.ec == nil || k.ec.Curve.Params().BitSize != size*8 || len(t.sig) != 2*size {
			return errors.New("not a key of the algorithm's curve")
		}
		h := hash[t.alg].New()
		h.Write(t.input)
		r, s := new(big.Int).SetBytes(t.sig[:size]), new(big.Int).SetBytes(t.sig[size:])
		if !ecdsa.Verify(k.ec, h.Sum(nil), r, s) {
			return errors.New("bad signature")
		}
		return nil
	case "EdDSA":
		if k.ed == nil || !ed25519.Verify(k.ed, t.input, t.sig) {
			return errors.New("bad signature")
		}
		return nil
	}
	return fmt.Errorf("algorithm %q", t.alg)
}
