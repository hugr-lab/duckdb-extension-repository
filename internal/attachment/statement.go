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
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	// V is the format's major (spec 0046): absent is 1, and this release writes none
	V           int64           `json:"v,omitempty"`
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

// Major is the statement format's major: 1 when v is absent.
func (s Statement) Major() int64 {
	if s.V == 0 {
		return 1
	}
	return s.V
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

// ErrMalformed is an authentic statement (its signature verified) whose frozen or known member has
// the wrong type or is out of its bound (spec 0046): refused for good, never held, since it will
// not get better.
var ErrMalformed = errors.New("attachment: the statement's members")

// Waiting reasons (spec 0046): why a verified statement is passed over until this kista is
// upgraded. An export feed keeps the statement under its reason, and a bundle carries the reason as
// the statement's note.
const (
	WaitNewer    = "newer"            // a statement format above the one this binary reads
	WaitKind     = "unknown kind"     // a kind this binary does not know
	WaitMedia    = "media type"       // a media type the kind does not take here
	WaitDocument = "document version" // a document version this binary does not read
)

// Waits reports whether a word is a waiting reason.
func Waits(word string) bool {
	return word == WaitNewer || word == WaitKind || word == WaitMedia || word == WaitDocument
}

// OlderThanSource says, from what a feed passed over by reason, that this kista is older than the
// feed's source (spec 0046), or nothing: for the administrators' view.
func OlderThanSource(waiting map[string]int) string {
	var why []string
	for w, n := range waiting {
		if n > 0 && w != "" {
			why = append(why, "`"+w+"`")
		}
	}
	if len(why) == 0 {
		return ""
	}
	sort.Strings(why)
	return "This kista is older than its source: documents wait for an upgrade (" + strings.Join(why, ", ") + ")."
}

// ReaderLevel is what this binary reads of a feed (spec 0046): raised whenever the kinds, the media
// types, a readable document version or the statement format's major grow, so that a feed takes
// again what an older binary passed over.
const ReaderLevel = 1

var (
	memberRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	// KindRE is a kind's grammar: what a statement and a plan may name, known here or not.
	KindRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
)

// MemberNames checks the member-name rule of kista's signed and bundled JSON (spec 0046): every
// member name at every depth is [a-z0-9_]{1,64}, and none is twice in its object. Go's decoder
// matches names without regard to case (Unicode folding too: the Kelvin sign for k, the long s) and
// keeps the last duplicate silently; under this rule no two names can mean one field, so two
// readers never read the same bytes differently and unknown members can be ignored.
func MemberNames(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	type frame struct {
		obj  bool
		keys map[string]bool
		key  bool // the next token in this object is a key
	}
	var stack []*frame
	for {
		t, err := d.Token()
		if err == io.EOF {
			if len(stack) > 0 {
				return errors.New("the document ends early")
			}
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if _, delim := t.(json.Delim); !delim && top != nil && top.obj && top.key {
			k, _ := t.(string)
			if !memberRE.MatchString(k) {
				return fmt.Errorf("the key %q is not lower-case ASCII", k)
			}
			if top.keys[k] {
				return fmt.Errorf("the key %q twice in one object", k)
			}
			top.keys[k] = true
			top.key = false
			continue
		}
		switch t {
		case json.Delim('{'):
			stack = append(stack, &frame{obj: true, keys: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].key = true // a value ended: a key comes next
		}
	}
}

// The members a statement's reader knows, with their JSON types: s string, i integer, b boolean.
// The frozen ones have the same name, type and meaning in every major of the format (spec 0046), so
// a reader can file a statement it cannot read; the others are major 1's.
var (
	frozenMembers = map[string]byte{"v": 'i', "issuer": 's', "key": 's', "issuer_id": 's', "seq": 'i', "body_hash": 's',
		"withdrawn": 'b', "audience": 's'}
	knownMembers = map[string]byte{"kind": 's', "media_type": 's', "content_hash": 's', "size": 'i', "title": 's', "issued_at": 's',
		"valid_until": 's', "supersedes": 's', "origin": 's'} // and summary, of any type
)

// typed checks the members of m that the table names against their types.
func typed(m map[string]any, table map[string]byte) error {
	for name, want := range table {
		v, ok := m[name]
		if !ok {
			continue
		}
		switch v.(type) {
		case string:
			ok = want == 's'
		case json.Number:
			ok = want == 'i'
		case bool:
			ok = want == 'b'
		default:
			ok = false
		}
		if !ok {
			return errors.New(name)
		}
	}
	return nil
}

// Verify checks a statement against the pinned keys (by fingerprint) and reads it (spec 0046), in
// order: its size and encoding; the member-name rule; the key it names is pinned; the signature
// over the received bytes; the bytes are their own canonical form (so no fraction or exponent in any
// member). A failure there is ErrStatement: nothing of the statement is trusted. Then its members
// are read: unknown ones are ignored; a frozen or known one of the wrong type or out of its bound is
// ErrMalformed, with the statement's issuer, body and seq when they can be read (the statement is
// authentic: its reader files it and moves on). A statement of a major above 1 answers its frozen
// members only: nothing else of it is read.
func Verify(stmt, sig []byte, pinned map[string]*rsa.PublicKey) (Statement, error) {
	if len(stmt) == 0 || len(stmt) > MaxStatement || !utf8.Valid(stmt) {
		return Statement{}, fmt.Errorf("%w: its size or encoding", ErrStatement)
	}
	if err := MemberNames(stmt); err != nil {
		return Statement{}, fmt.Errorf("%w: it does not parse, or %w", ErrStatement, err)
	}
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(stmt))
	d.UseNumber()
	if err := d.Decode(&m); err != nil || m == nil {
		return Statement{}, fmt.Errorf("%w: it is not a JSON object", ErrStatement)
	}
	key, _ := m["key"].(string)
	pub, ok := pinned[key]
	if !ok {
		return Statement{}, fmt.Errorf("%w: key %s is not pinned", ErrStatement, clipID(key))
	}
	h := digest(stmt)
	if len(sig) != extfile.SignatureSize || rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) != nil {
		return Statement{}, fmt.Errorf("%w: the signature does not verify", ErrStatement)
	}
	if again, err := Canonicalize(stmt); err != nil || !bytes.Equal(again, stmt) {
		return Statement{}, fmt.Errorf("%w: it is not in its canonical form", ErrStatement)
	}
	// authentic from here on
	var filed Statement // what files it, when it cannot be read
	malformed := func(why string) (Statement, error) {
		return filed, fmt.Errorf("%w: %s", ErrMalformed, why)
	}
	if err := typed(m, frozenMembers); err != nil {
		if typed(m, map[string]byte{"issuer": 's', "issuer_id": 's', "seq": 'i', "body_hash": 's'}) == nil {
			filed = fileable(frozen(m))
		}
		return malformed(err.Error())
	}
	s := frozen(m)
	filed = fileable(s)
	if _, ok := m["v"]; ok && s.V < 1 {
		return malformed("v")
	}
	if s.Major() == 1 {
		if err := typed(m, knownMembers); err != nil {
			return malformed(err.Error())
		}
		s = Statement{}
		if err := json.Unmarshal(stmt, &s); err != nil {
			return malformed("its members")
		}
	}
	if err := s.check(); err != nil {
		return malformed(err.Error())
	}
	return s, nil
}

// frozen reads a statement's frozen members, their types checked.
func frozen(m map[string]any) Statement {
	str := func(name string) string { v, _ := m[name].(string); return v }
	num := func(name string) int64 {
		n, _ := m[name].(json.Number)
		v, _ := strconv.ParseInt(n.String(), 10, 64)
		return v
	}
	withdrawn, _ := m["withdrawn"].(bool)
	return Statement{V: num("v"), Issuer: str("issuer"), Key: str("key"), IssuerID: str("issuer_id"), Seq: num("seq"),
		BodyHash: str("body_hash"), Audience: str("audience"), Withdrawn: withdrawn}
}

// fileable is what files a statement that cannot be read: its issuer, document, body and seq, when
// each is within its bound; none otherwise.
func fileable(s Statement) Statement {
	if s.checkFrozen() != nil {
		return Statement{}
	}
	return Statement{Issuer: s.Issuer, IssuerID: s.IssuerID, Seq: s.Seq, BodyHash: s.BodyHash}
}

func clipID(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// checkFrozen bounds the members that file a statement, in every major.
func (s Statement) checkFrozen() error {
	switch {
	case s.Issuer == "" || len(s.Issuer) > 400:
		return errors.New("issuer")
	case s.IssuerID == "" || len(s.IssuerID) > 64:
		return errors.New("issuer_id")
	case s.Seq <= 0 || s.Seq > math.MaxInt64/2:
		return errors.New("seq")
	case !hexHash(s.BodyHash):
		return errors.New("body_hash")
	}
	return nil
}

// check bounds a verified statement's members; what a subscriber stores is checked again at intake.
// Of a major above 1 only the frozen members are bounded: the rest is not read.
func (s Statement) check() error {
	if err := s.checkFrozen(); err != nil {
		return err
	}
	if s.Major() > 1 {
		return nil
	}
	if len(s.Supersedes) > 64 {
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
	case !KindRE.MatchString(s.Kind):
		return errors.New("kind")
	case s.MediaType == "" || len(s.MediaType) > 200:
		return errors.New("media_type")
	case !hexHash(s.ContentHash):
		return errors.New("content_hash")
	case s.Size <= 0:
		return errors.New("size")
	case len(s.Origin) > 80 || !printable(s.Origin):
		// who stands behind the document at its source: tenant or publisher:<name> today, stored and
		// shown as text whatever a later source writes (spec 0046)
		return errors.New("origin")
	}
	// audience: readers, or anything else (not readers): frozen, never refused
	return nil
}
