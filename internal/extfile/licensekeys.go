package extfile

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

// The licence key block (spec 0021): the keys an extension's build gave the kista SDK
// (kista_sdk_license_keys), written into the binary as one constant byte array:
//
//	"KISTA-LICENSE-KEYS/1\0"   the magic
//	count                      1 byte, 1 to MaxLicenseKeys
//	per key: length            2 bytes, big-endian, 1 to MaxLicenseKeySize
//	         the key           one SPKI PEM block ("PUBLIC KEY", canonical), length bytes
//	"\0KISTA-END\0"            the end marker
const (
	// MaxLicenseKeys is the most keys a block holds.
	MaxLicenseKeys = 4
	// MaxLicenseKeySize is the largest key, in bytes of PEM.
	MaxLicenseKeySize = 2048
	// maxLicenseKeyBlocks is the most occurrences of the magic a body may hold; more is malformed.
	maxLicenseKeyBlocks = 16
)

var (
	licenseKeysMagic = []byte("KISTA-LICENSE-KEYS/1\x00")
	licenseKeysEnd   = []byte("\x00KISTA-END\x00")
	maxLicenseBlock  = len(licenseKeysMagic) + 1 + MaxLicenseKeys*(2+MaxLicenseKeySize) + len(licenseKeysEnd)
)

// ErrLicenseKeysMalformed is a body whose licence key blocks do not parse, or differ: it declares no
// keys.
var ErrLicenseKeysMalformed = errors.New("licence keys: malformed")

// LicenseKey is a key from a build's key block: its fingerprint (a licence header's kid) and its PEM,
// as the block holds it.
type LicenseKey struct {
	Kid string
	PEM string
}

// LicenseKeys is what a body declares: its keys, none when it has no block, or Err (wrapping
// ErrLicenseKeysMalformed, with why) and no keys.
type LicenseKeys struct {
	Keys []LicenseKey
	Err  error
}

// EncodeLicenseKeys builds a key block from PEM keys, as kista_sdk_license_keys does: for tests and
// tools. It checks only the limits; the reader checks the keys.
func EncodeLicenseKeys(pems ...string) ([]byte, error) {
	if len(pems) < 1 || len(pems) > MaxLicenseKeys {
		return nil, fmt.Errorf("extfile: %d licence keys, 1 to %d allowed", len(pems), MaxLicenseKeys)
	}
	b := append([]byte(nil), licenseKeysMagic...)
	b = append(b, byte(len(pems)))
	for _, p := range pems {
		if len(p) < 1 || len(p) > MaxLicenseKeySize {
			return nil, fmt.Errorf("extfile: a licence key of %d bytes, 1 to %d allowed", len(p), MaxLicenseKeySize)
		}
		b = append(b, byte(len(p)>>8), byte(len(p)))
		b = append(b, p...)
	}
	return append(b, licenseKeysEnd...), nil
}

// magicScanner is an io.Writer that records where the magic occurs in what is written to it, across
// writes, so the hashing pass finds the blocks.
type magicScanner struct {
	off     int64  // bytes written so far
	tail    []byte // the last len(magic)-1 bytes written
	hits    []int64
	tooMany bool
}

func (s *magicScanner) Write(p []byte) (int, error) {
	m := len(licenseKeysMagic)
	// a magic that starts in the tail and ends in p (one wholly in either is found below or was found)
	if len(s.tail) > 0 {
		buf := append(s.tail[:len(s.tail):len(s.tail)], p[:min(len(p), m-1)]...)
		s.find(buf, s.off-int64(len(s.tail)))
	}
	s.find(p, s.off)
	s.off += int64(len(p))
	if len(p) >= m-1 {
		s.tail = append(s.tail[:0], p[len(p)-(m-1):]...)
	} else {
		s.tail = append(s.tail, p...)
		if len(s.tail) > m-1 {
			s.tail = append(s.tail[:0], s.tail[len(s.tail)-(m-1):]...)
		}
	}
	return len(p), nil
}

func (s *magicScanner) find(b []byte, base int64) {
	for i := 0; ; {
		j := bytes.Index(b[i:], licenseKeysMagic)
		if j < 0 {
			return
		}
		if len(s.hits) == maxLicenseKeyBlocks {
			s.tooMany = true
			return
		}
		s.hits = append(s.hits, base+int64(i+j))
		i += j + 1
	}
}

// keys reads the blocks the scan found from body, of size n.
func (s *magicScanner) keys(body io.ReaderAt, n int64) (LicenseKeys, error) {
	if len(s.hits) == 0 {
		return LicenseKeys{}, nil
	}
	if s.tooMany {
		return malformed("more than %d blocks", maxLicenseKeyBlocks), nil
	}
	var first []byte
	var keys []LicenseKey
	for _, off := range s.hits {
		buf := make([]byte, min(int64(maxLicenseBlock), n-off))
		if err := readAt(body, buf, off); err != nil {
			return LicenseKeys{}, fmt.Errorf("extfile: reading the licence keys: %w", err)
		}
		k, size, err := parseLicenseKeys(buf)
		if err != nil {
			return LicenseKeys{Err: err}, nil
		}
		if first == nil {
			first, keys = buf[:size], k
		} else if !bytes.Equal(first, buf[:size]) {
			return malformed("blocks that differ"), nil
		}
	}
	return LicenseKeys{Keys: keys}, nil
}

func malformed(format string, args ...any) LicenseKeys {
	return LicenseKeys{Err: fmt.Errorf("%w: "+format, append([]any{ErrLicenseKeysMalformed}, args...)...)}
}

// parseLicenseKeys parses the block at the start of b, strictly, and returns its keys and its size.
func parseLicenseKeys(b []byte) ([]LicenseKey, int, error) {
	bad := func(format string, args ...any) ([]LicenseKey, int, error) {
		return nil, 0, malformed(format, args...).Err
	}
	p := len(licenseKeysMagic)
	if len(b) < p+1 {
		return bad("truncated")
	}
	count := int(b[p])
	p++
	if count < 1 || count > MaxLicenseKeys {
		return bad("%d keys, 1 to %d allowed", count, MaxLicenseKeys)
	}
	keys := make([]LicenseKey, 0, count)
	seen := map[string]bool{}
	for i := range count {
		if len(b) < p+2 {
			return bad("truncated")
		}
		size := int(b[p])<<8 | int(b[p+1])
		p += 2
		if size < 1 || size > MaxLicenseKeySize {
			return bad("key %d is %d bytes, 1 to %d allowed", i+1, size, MaxLicenseKeySize)
		}
		if len(b) < p+size {
			return bad("truncated")
		}
		k, err := parseLicenseKey(b[p : p+size])
		if err != nil {
			return bad("key %d: %v", i+1, err)
		}
		if seen[k.Kid] {
			return bad("key %s appears twice", k.Kid)
		}
		seen[k.Kid] = true
		keys = append(keys, k)
		p += size
	}
	if len(b) < p+len(licenseKeysEnd) || !bytes.Equal(b[p:p+len(licenseKeysEnd)], licenseKeysEnd) {
		return bad("no end marker after the last key")
	}
	return keys, p + len(licenseKeysEnd), nil
}

// parseLicenseKey checks a key: exactly one PUBLIC KEY PEM block in the canonical encoding (no
// headers, 64-column base64, LF line ends, no other bytes), whose DER re-encodes identically,
// RSA-2048 with e = 65537.
func parseLicenseKey(b []byte) (LicenseKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 {
		return LicenseKey{}, errors.New("not one PUBLIC KEY PEM block")
	}
	if !bytes.Equal(pem.EncodeToMemory(block), b) {
		return LicenseKey{}, errors.New("not the canonical PEM encoding, or bytes around it")
	}
	pub, err := parseSPKI(block.Bytes)
	if err != nil {
		return LicenseKey{}, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil || !bytes.Equal(der, block.Bytes) {
		return LicenseKey{}, errors.New("the DER does not re-encode identically")
	}
	return LicenseKey{Kid: Fingerprint(pub), PEM: string(b)}, nil
}
