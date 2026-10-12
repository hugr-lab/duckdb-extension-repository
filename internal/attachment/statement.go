package attachment

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Signed statements (spec 0011 phase 1b): what a source says about a document, as a JSON object
// canonicalised by RFC 8785 (JCS) and signed RSA PKCS#1 v1.5 over SHA-256 of the domain separator
// followed by it. A subscriber verifies the exact bytes it received and never re-canonicalises.

// StatementDomain separates a statement's digest from every other kista signature.
const StatementDomain = "kista-statement-v1\x00"

// MaxStatement is a statement's largest encoding.
const MaxStatement = 16 << 10

// ErrStatement is a statement that does not verify or does not parse.
var ErrStatement = errors.New("attachment: the statement")

// Statement is a statement's fields. A withdrawal carries the document's identity only.
type Statement struct {
	Issuer      string          `json:"issuer"`
	Key         string          `json:"key"`
	IssuerID    string          `json:"issuer_id"`
	Seq         int64           `json:"seq"`
	BodyHash    string          `json:"body_hash"`
	Kind        string          `json:"kind,omitempty"`
	MediaType   string          `json:"media_type,omitempty"`
	ContentHash string          `json:"content_hash,omitempty"`
	Size        int64           `json:"size,omitempty"`
	Title       string          `json:"title,omitempty"`
	Summary     json.RawMessage `json:"summary,omitempty"`
	IssuedAt    string          `json:"issued_at"`
	ValidUntil  string          `json:"valid_until,omitempty"`
	Supersedes  string          `json:"supersedes,omitempty"`
	Origin      string          `json:"origin,omitempty"`
	Audience    string          `json:"audience,omitempty"`
	Withdrawn   bool            `json:"withdrawn,omitempty"`
}

// Timestamp is a statement's time format.
func Timestamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// Canonical encodes a statement by JCS.
func (s Statement) Canonical() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Canonicalize re-encodes a JSON text by RFC 8785: object members sorted by their names' UTF-16
// code units, no whitespace, strings in the shortest escaping. Numbers must be integers within
// ±2^53 (a statement carries no others), so their form is the plain decimal.
func Canonicalize(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, errors.New("trailing data")
	}
	var b bytes.Buffer
	if err := canonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func canonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil || n > 1<<53 || n < -(1<<53) {
			return fmt.Errorf("the number %s is not a safe integer", x)
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case string:
		quote(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			quote(b, k)
			b.WriteByte(':')
			if err := canonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("a %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// quote writes a string as ECMAScript's JSON.stringify does (RFC 8785 §3.2.2.2).
func quote(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// digest is what a statement's signature signs.
func digest(stmt []byte) [sha256.Size]byte {
	return sha256.Sum256(append([]byte(StatementDomain), stmt...))
}

// Sign signs a statement's canonical bytes with an attestation key's signer.
func Sign(ctx context.Context, sg signer.Signer, stmt []byte) ([]byte, error) {
	return sg.Sign(ctx, extfile.BodyHash(digest(stmt)))
}

// Verify checks a statement's signature against the pinned keys (by fingerprint) and parses it: the
// key that verifies must be the one the statement names. Unknown fields are refused, every field is
// bounded, and the bytes must be the statement's canonical form.
func Verify(stmt, sig []byte, pinned map[string]*rsa.PublicKey) (Statement, error) {
	if len(stmt) == 0 || len(stmt) > MaxStatement || !utf8.Valid(stmt) {
		return Statement{}, fmt.Errorf("%w: its size or encoding", ErrStatement)
	}
	var s Statement
	d := json.NewDecoder(bytes.NewReader(stmt))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil || d.More() {
		return Statement{}, fmt.Errorf("%w: it does not parse", ErrStatement)
	}
	pub, ok := pinned[s.Key]
	if !ok {
		return Statement{}, fmt.Errorf("%w: key %s is not pinned", ErrStatement, clipID(s.Key))
	}
	h := digest(stmt)
	if len(sig) != extfile.SignatureSize || rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) != nil {
		return Statement{}, fmt.Errorf("%w: the signature does not verify", ErrStatement)
	}
	if again, err := s.Canonical(); err != nil || !bytes.Equal(again, stmt) {
		return Statement{}, fmt.Errorf("%w: it is not in its canonical form", ErrStatement)
	}
	if err := s.check(); err != nil {
		return Statement{}, fmt.Errorf("%w: %w", ErrStatement, err)
	}
	return s, nil
}

func clipID(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// check bounds a verified statement's fields; what a subscriber stores is checked again at intake.
func (s Statement) check() error {
	switch {
	case s.Issuer == "" || len(s.Issuer) > 400:
		return errors.New("issuer")
	case s.IssuerID == "" || len(s.IssuerID) > 64:
		return errors.New("issuer_id")
	case s.Seq <= 0 || s.Seq > math.MaxInt64/2:
		return errors.New("seq")
	case !hexHash(s.BodyHash):
		return errors.New("body_hash")
	case len(s.Supersedes) > 64:
		return errors.New("supersedes")
	}
	if _, err := time.Parse(time.RFC3339, s.IssuedAt); err != nil {
		return errors.New("issued_at")
	}
	if s.Withdrawn {
		if s.Kind != "" || s.ContentHash != "" || s.Size != 0 || len(s.Summary) != 0 {
			return errors.New("a withdrawal carries no document")
		}
		return nil
	}
	if s.ValidUntil != "" {
		if _, err := time.Parse(time.RFC3339, s.ValidUntil); err != nil {
			return errors.New("valid_until")
		}
	}
	switch {
	case !hexHash(s.ContentHash):
		return errors.New("content_hash")
	case s.Size <= 0:
		return errors.New("size")
	case s.Audience != "administrators" && s.Audience != "readers":
		return errors.New("audience")
	case s.Origin != "tenant" && (len(s.Origin) <= len("publisher:") || s.Origin[:len("publisher:")] != "publisher:" || len(s.Origin) > 80):
		return errors.New("origin")
	}
	return nil
}
