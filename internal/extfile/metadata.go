// Package extfile reads, hashes, verifies and re-signs DuckDB extension files, and assembles the gzip
// form kista serves.
//
// An extension file is a body followed by a 256-byte signature. The body ends with a 256-byte metadata
// block, so DuckDB reads the last 512 bytes of a file as its footer (FOOTER_SIZE in
// src/include/duckdb/main/extension.hpp).
package extfile

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	// SignatureSize is the size of the trailing RSA-2048 signature.
	SignatureSize = 256
	// MetadataSize is the size of the metadata block that ends the body.
	MetadataSize = 256
	// FooterSize is the metadata block plus the signature: the least a file can hold.
	FooterSize = MetadataSize + SignatureSize

	fieldSize  = 32
	fieldCount = 8
	magic      = "4"
)

// MetadataPrefix is the wasm custom-section header that DuckDB's append_metadata.cmake writes in front
// of the metadata block of every extension, native ones included. DuckDB does not read it.
var MetadataPrefix = []byte("\x00\x93\x04\x10duckdb_signature\x80\x04")

// ABIType is the extension's ABI, from metadata field 4.
type ABIType int

const (
	ABICPP             ABIType = iota // CPP, or an empty field
	ABICStruct                        // C_STRUCT: field 2 is a minimum C API version
	ABICStructUnstable                // C_STRUCT_UNSTABLE: field 2 is an exact DuckDB version
)

func (a ABIType) String() string {
	switch a {
	case ABICPP:
		return "CPP"
	case ABICStruct:
		return "C_STRUCT"
	case ABICStructUnstable:
		return "C_STRUCT_UNSTABLE"
	}
	return fmt.Sprintf("ABIType(%d)", int(a))
}

// Metadata is the parsed metadata block.
type Metadata struct {
	Platform         string
	DuckDBVersion    string // CPP and C_STRUCT_UNSTABLE: the exact DuckDB version (tag or source id)
	CAPIVersion      string // C_STRUCT: the minimum C API version
	ExtensionVersion string
	ABI              ABIType
}

// ErrMalformed marks input that is not a valid DuckDB extension.
var ErrMalformed = errors.New("extfile: not a valid DuckDB extension")

// ParseMetadata parses a metadata block the way DuckDB's ParseExtensionMetaData does: eight 32-byte
// zero-padded fields, stored in reverse order. It is stricter than DuckDB on purpose, because these
// strings later become URL path segments:
//   - the magic must be exactly "4";
//   - a used field must match [A-Za-z0-9._-]{1,32} before its padding, the extension version may be
//     empty, and every byte after the first zero byte must be zero;
//   - an unknown ABI is an error.
func ParseMetadata(block [MetadataSize]byte) (Metadata, error) {
	var fields [fieldCount]string
	for i := range fieldCount {
		// field i (after reversal) is stored at position fieldCount-1-i
		raw := block[(fieldCount-1-i)*fieldSize : (fieldCount-i)*fieldSize]
		s, err := field(raw)
		if err != nil && i <= 4 {
			return Metadata{}, fmt.Errorf("%w: metadata field %d: %v", ErrMalformed, i, err)
		}
		fields[i] = s
	}
	if fields[0] != magic {
		return Metadata{}, fmt.Errorf("%w: metadata magic %q", ErrMalformed, fields[0])
	}
	m := Metadata{Platform: fields[1], ExtensionVersion: fields[3]}
	if m.Platform == "" {
		return Metadata{}, fmt.Errorf("%w: empty platform", ErrMalformed)
	}
	switch fields[4] {
	case "CPP", "":
		m.ABI, m.DuckDBVersion = ABICPP, fields[2]
	case "C_STRUCT":
		m.ABI, m.CAPIVersion = ABICStruct, fields[2]
	case "C_STRUCT_UNSTABLE":
		m.ABI, m.DuckDBVersion = ABICStructUnstable, fields[2]
	default:
		return Metadata{}, fmt.Errorf("%w: unknown ABI type %q", ErrMalformed, fields[4])
	}
	if fields[2] == "" {
		return Metadata{}, fmt.Errorf("%w: empty DuckDB / C API version", ErrMalformed)
	}
	return m, nil
}

// field returns a zero-padded field's value. The value is the bytes before the first zero byte; the
// rest must be zero, and the value must be a safe path segment (or empty).
func field(raw []byte) (string, error) {
	n := bytes.IndexByte(raw, 0)
	if n < 0 {
		n = len(raw)
	}
	for _, b := range raw[n:] {
		if b != 0 {
			return "", errors.New("non-zero byte after padding")
		}
	}
	v := raw[:n]
	for _, b := range v {
		if !safeByte(b) {
			return "", fmt.Errorf("byte %#x is not allowed", b)
		}
	}
	if s := string(v); s == "." || s == ".." {
		return "", fmt.Errorf("%q is not allowed", s)
	}
	return string(v), nil
}

func safeByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}

// EncodeMetadata is the inverse of ParseMetadata, for tests and for publishing (spec 0006). Fields
// are not validated beyond their length.
func EncodeMetadata(m Metadata) ([MetadataSize]byte, error) {
	var block [MetadataSize]byte
	version := m.DuckDBVersion
	if m.ABI == ABICStruct {
		version = m.CAPIVersion
	}
	fields := [fieldCount]string{magic, m.Platform, version, m.ExtensionVersion, m.ABI.String()}
	for i, f := range fields {
		if len(f) > fieldSize {
			return block, fmt.Errorf("extfile: metadata field %d is longer than %d bytes", i, fieldSize)
		}
		copy(block[(fieldCount-1-i)*fieldSize:], f)
	}
	return block, nil
}
