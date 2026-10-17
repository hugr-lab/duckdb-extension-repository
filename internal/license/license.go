// Package license is spec 0012's licence: the format (an RS256 JWS with typ kista-license+jwt),
// its strict parser and the reference verifier the C++ SDK is tested against (testdata/vectors.json).
// kista issues nothing: it verifies, stores and serves licences an issuer (Enterest) signed.
package license

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// Type is the header's typ: the domain separator that keeps any other JWT signed by the same key
// from being a licence.
const Type = "kista-license+jwt"

// Alg is the only signature algorithm: RSASSA-PKCS1-v1_5 with SHA-256, over RSA-2048.
const Alg = "RS256"

// MaxSize bounds a licence in its compact form.
const MaxSize = 16 << 10

// Skew is how far clocks may disagree when a licence's issue time is checked.
const Skew = 300 * time.Second

// DefaultGrace is a licence's grace when it names none; MaxGrace bounds the one it names.
const (
	DefaultGrace = 7 * 24 * time.Hour
	MaxGrace     = 90 * 24 * time.Hour
)

// Status is what a licence means for an extension at a time (spec 0012: the first row that applies).
type Status string

const (
	Invalid     Status = "invalid"
	NotCovered  Status = "not_covered"
	NotYetValid Status = "not_yet_valid"
	Valid       Status = "valid"
	Grace       Status = "grace"
	Expired     Status = "expired"
)

// The rules a licence can break: an Error's Rule, and a vector's. They are stable codes, so the C++
// SDK's tests check that a token is refused for the same reason.
const (
	RuleSize          = "size"           // longer than MaxSize
	RuleCompact       = "compact"        // not three parts
	RuleBase64        = "base64"         // a part is not canonical base64url without padding
	RuleJSONSyntax    = "json_syntax"    // not JSON, or not the expected JSON value
	RuleJSONString    = "json_string"    // invalid UTF-8, an unpaired surrogate, a control character, U+0000
	RuleJSONNumber    = "json_number"    // not an integer within ±(2^53-1), or a leading zero
	RuleJSONDuplicate = "json_duplicate" // a member name twice in an object
	RuleJSONDepth     = "json_depth"     // nested deeper than 32 levels
	RuleHeader        = "header"         // not exactly alg RS256, typ kista-license+jwt and kid
	RuleKid           = "kid"            // kid is not sha256: and 64 lowercase hex digits
	RulePayload       = "payload"        // the payload is not an object
	RuleV             = "v"
	RuleIss           = "iss"
	RuleJti           = "jti"
	RuleSub           = "sub"
	RuleHolder        = "holder"
	RuleExt           = "ext"
	RulePlan          = "plan"
	RuleVersions      = "versions"
	RuleLimits        = "limits"
	RuleIat           = "iat"
	RuleExp           = "exp"
	RuleGrace         = "grace"
	RuleKeyUntrusted  = "kid_untrusted" // the key is not among the trusted ones
	RuleSignature     = "signature"     // the signature does not verify
	RuleExtMismatch   = "ext_mismatch"  // a licence for another extension
)

// Error is a licence refused for a rule.
type Error struct {
	Rule string
	Msg  string
}

func (e *Error) Error() string { return "license: " + e.Msg }

// Is matches an Error of the same rule: errors.Is(err, ErrSignature).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Msg == "" && t.Rule == e.Rule
}

// Sentinels for the rules a caller acts on; any other rule means the token is malformed.
var (
	ErrUntrustedKey   = &Error{Rule: RuleKeyUntrusted}
	ErrSignature      = &Error{Rule: RuleSignature}
	ErrWrongExtension = &Error{Rule: RuleExtMismatch}
)

func refuse(rule, format string, args ...any) error {
	return &Error{Rule: rule, Msg: fmt.Sprintf(format, args...)}
}

// Value is one of a licence's limits: a bool, an integer or a string.
type Value struct {
	v any // bool, int64 or string
}

// Bool, Int and Str return the value as that type, and whether it is one.
func (v Value) Bool() (bool, bool)  { b, ok := v.v.(bool); return b, ok }
func (v Value) Int() (int64, bool)  { n, ok := v.v.(int64); return n, ok }
func (v Value) Str() (string, bool) { s, ok := v.v.(string); return s, ok }

// MarshalJSON writes the value as the JSON scalar it is.
func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.v) }

// UnmarshalJSON reads a bool, an integer or a string, by the licence's rules.
func (v *Value) UnmarshalJSON(b []byte) error {
	p, err := parseJSON(b)
	if err != nil {
		return err
	}
	return v.set("value", p)
}

func (v *Value) set(name string, p value) error {
	switch p.kind {
	case kindBool:
		v.v = p.b
	case kindInt:
		v.v = p.n
	case kindString:
		if len(p.s) > 256 {
			return refuse(RuleLimits, "limit %q is longer than 256 bytes", name)
		}
		v.v = p.s
	default:
		return refuse(RuleLimits, "limit %q is not a bool, an integer or a string", name)
	}
	return nil
}

// Limit is a named limit, in the licence's order.
type Limit struct {
	Name  string `json:"name"`
	Value Value  `json:"value"`
}

// Versions is a licence's version range: Min inclusive, Below exclusive; either may be empty.
type Versions struct {
	Min   string `json:"min,omitempty"`
	Below string `json:"below,omitempty"`
}

// Claims are a licence's contents, named as kista's API names them (the payload's iss is issuer,
// jti id, sub holder_id, ext extension; the header's kid is key). Times are seconds since the epoch;
// Grace is in seconds, the default filled in.
type Claims struct {
	Key       string    `json:"key"`
	Issuer    string    `json:"issuer"`
	ID        string    `json:"id"`
	HolderID  string    `json:"holder_id"`
	Holder    string    `json:"holder,omitempty"`
	Extension string    `json:"extension"`
	Versions  *Versions `json:"versions,omitempty"`
	Plan      string    `json:"plan"`
	Limits    []Limit   `json:"limits,omitempty"`
	IssuedAt  int64     `json:"issued_at"`
	ExpiresAt int64     `json:"expires_at"`
	Grace     int64     `json:"grace"`
}

// GraceUntil is the end of the licence's grace, in seconds since the epoch.
func (c *Claims) GraceUntil() int64 { return c.ExpiresAt + c.Grace }

// Token is a parsed licence: its claims as read, not yet verified. Parse is also how a stored,
// already verified licence's claims are read back.
type Token struct {
	Claims
	signed    []byte // header.payload, as received
	signature []byte
}

var (
	jtiRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	extRe   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	planRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	limitRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	versRe  = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})(\.(0|[1-9][0-9]{0,8})){0,3}$`)
	kidRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	issRe   = regexp.MustCompile(`^https://[\x21-\x7e]+$`)
	b64Re   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Parse reads a licence by every rule of spec 0012 that needs no key: the compact form, the header,
// the payload's members and their rules. It checks neither the signature nor the time.
func Parse(token string) (*Token, error) {
	if len(token) > MaxSize {
		return nil, refuse(RuleSize, "longer than %d bytes", MaxSize)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, refuse(RuleCompact, "not a compact JWS (three parts)")
	}
	raw := make([][]byte, 3)
	for i, p := range parts {
		// the alphabet first: Go's decoder skips newlines
		b, err := base64.RawURLEncoding.Strict().DecodeString(p)
		if !b64Re.MatchString(p) || err != nil {
			return nil, refuse(RuleBase64, "part %d is not canonical base64url without padding", i+1)
		}
		raw[i] = b
	}
	t := &Token{signed: []byte(parts[0] + "." + parts[1]), signature: raw[2]}
	if err := t.header(raw[0]); err != nil {
		return nil, err
	}
	if err := t.payload(raw[1]); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Token) header(b []byte) error {
	h, err := parseJSON(b)
	if err != nil {
		return err
	}
	if h.kind != kindObject || len(h.members) != 3 {
		return refuse(RuleHeader, "the header must have exactly alg, typ and kid")
	}
	alg, err := str(h, "alg", RuleHeader, true)
	if err != nil {
		return err
	}
	typ, err := str(h, "typ", RuleHeader, true)
	if err != nil {
		return err
	}
	kid, err := str(h, "kid", RuleHeader, true)
	if err != nil {
		return err
	}
	if alg != Alg {
		return refuse(RuleHeader, "alg %q, want %s", alg, Alg)
	}
	if typ != Type {
		return refuse(RuleHeader, "typ %q, want %s", typ, Type)
	}
	if !kidRe.MatchString(kid) {
		return refuse(RuleKid, "kid is not sha256: and 64 lowercase hex digits")
	}
	t.Key = kid
	return nil
}

func (t *Token) payload(b []byte) error {
	p, err := parseJSON(b)
	if err != nil {
		return err
	}
	if p.kind != kindObject {
		return refuse(RulePayload, "the payload is not an object")
	}
	v, err := integer(p, "v", RuleV, true)
	if err != nil {
		return err
	}
	if v != 1 {
		return refuse(RuleV, "format version %d, want 1", v)
	}
	c := &t.Claims
	if c.Issuer, err = str(p, "iss", RuleIss, true); err != nil {
		return err
	}
	if len(c.Issuer) > 256 || !issRe.MatchString(c.Issuer) {
		return refuse(RuleIss, "iss is not https:// and printable ASCII, at most 256 bytes")
	}
	if c.ID, err = str(p, "jti", RuleJti, true); err != nil {
		return err
	}
	if !jtiRe.MatchString(c.ID) {
		return refuse(RuleJti, "jti is not [A-Za-z0-9_-]{1,64}")
	}
	if c.HolderID, err = str(p, "sub", RuleSub, true); err != nil {
		return err
	}
	if c.HolderID == "" || len(c.HolderID) > 128 {
		return refuse(RuleSub, "sub must be 1 to 128 bytes")
	}
	if c.Holder, err = str(p, "holder", RuleHolder, false); err != nil {
		return err
	}
	if len(c.Holder) > 200 {
		return refuse(RuleHolder, "holder is longer than 200 bytes")
	}
	if c.Extension, err = str(p, "ext", RuleExt, true); err != nil {
		return err
	}
	if !extRe.MatchString(c.Extension) {
		return refuse(RuleExt, "ext is not an extension name")
	}
	if c.Plan, err = str(p, "plan", RulePlan, true); err != nil {
		return err
	}
	if !planRe.MatchString(c.Plan) {
		return refuse(RulePlan, "plan is not [a-z0-9][a-z0-9_-]{0,31}")
	}
	if err := t.versions(p); err != nil {
		return err
	}
	if err := t.limits(p); err != nil {
		return err
	}
	if c.IssuedAt, err = integer(p, "iat", RuleIat, true); err != nil {
		return err
	}
	if c.IssuedAt < 0 {
		return refuse(RuleIat, "iat is negative")
	}
	if c.ExpiresAt, err = integer(p, "exp", RuleExp, true); err != nil {
		return err
	}
	if c.ExpiresAt <= c.IssuedAt {
		return refuse(RuleExp, "exp is not after iat")
	}
	c.Grace = int64(DefaultGrace / time.Second)
	if _, ok := p.get("grace"); ok {
		if c.Grace, err = integer(p, "grace", RuleGrace, true); err != nil {
			return err
		}
		if c.Grace < 0 || c.Grace > int64(MaxGrace/time.Second) {
			return refuse(RuleGrace, "grace must be 0 to %d seconds", int64(MaxGrace/time.Second))
		}
	}
	if c.ExpiresAt+c.Grace > maxSafe {
		return refuse(RuleExp, "exp + grace is beyond 2^53-1")
	}
	return nil
}

func (t *Token) versions(p value) error {
	v, ok := p.get("versions")
	if !ok {
		return nil
	}
	if v.kind != kindObject {
		return refuse(RuleVersions, "versions is not an object")
	}
	var r Versions
	n := 0
	for _, f := range []struct {
		name string
		to   *string
	}{{"min", &r.Min}, {"below", &r.Below}} {
		if _, ok := v.get(f.name); !ok {
			continue
		}
		n++
		s, err := str(v, f.name, RuleVersions, true)
		if err != nil {
			return err
		}
		if !versRe.MatchString(s) {
			return refuse(RuleVersions, "version %q is not a version (v1.2.3)", s)
		}
		*f.to = s
	}
	if n == 0 {
		return refuse(RuleVersions, "versions names neither min nor below")
	}
	t.Versions = &r
	return nil
}

func (t *Token) limits(p value) error {
	l, ok := p.get("limits")
	if !ok {
		return nil
	}
	if l.kind != kindObject {
		return refuse(RuleLimits, "limits is not an object")
	}
	if len(l.members) > 64 {
		return refuse(RuleLimits, "more than 64 limits")
	}
	for _, m := range l.members {
		if !limitRe.MatchString(m.name) {
			return refuse(RuleLimits, "limit name %q is not [a-z][a-z0-9_]{0,63}", m.name)
		}
		lim := Limit{Name: m.name}
		if err := lim.Value.set(m.name, m.v); err != nil {
			return err
		}
		t.Limits = append(t.Limits, lim)
	}
	return nil
}

// str reads a string member; absent is "" unless required.
func str(o value, name, rule string, required bool) (string, error) {
	v, ok := o.get(name)
	if !ok {
		if required {
			return "", refuse(rule, "%s is missing", name)
		}
		return "", nil
	}
	if v.kind != kindString {
		return "", refuse(rule, "%s is not a string", name)
	}
	return v.s, nil
}

func integer(o value, name, rule string, required bool) (int64, error) {
	v, ok := o.get(name)
	if !ok {
		if required {
			return 0, refuse(rule, "%s is missing", name)
		}
		return 0, nil
	}
	if v.kind != kindInt {
		return 0, refuse(rule, "%s is not an integer", name)
	}
	return v.n, nil
}

// Keys are the licence keys a verifier trusts, by fingerprint (extfile.Fingerprint, the form of a
// header's kid).
type Keys map[string]*rsa.PublicKey

// ParseKeys reads public keys in the forms DuckDB accepts (extfile.ParsePublicKey: SPKI PEM or
// base64 DER), each RSA-2048 with e = 65537: the only keys the SDK can verify with, since DuckDB's
// mbedtls wrapper takes 256-byte signatures only. The same key twice is one key.
func ParseKeys(keys []string) (Keys, error) {
	ks := Keys{}
	for i, k := range keys {
		pub, err := extfile.ParsePublicKey(k)
		if err != nil {
			return nil, fmt.Errorf("license: key %d: %w", i+1, err)
		}
		ks[extfile.Fingerprint(pub)] = pub
	}
	return ks, nil
}

// Verify checks the signature with the key the header names, which must be among keys.
func (t *Token) Verify(keys Keys) error {
	pub, ok := keys[t.Key]
	if !ok {
		return refuse(RuleKeyUntrusted, "key %s is not trusted", t.Key)
	}
	// a 256-byte signature: Keys may hold a key ParseKeys would refuse
	if len(t.signature) != extfile.KeyBits/8 || extfile.CheckKey(pub) != nil {
		return refuse(RuleSignature, "the signature is not 256 bytes by an RSA-2048 key")
	}
	sum := sha256.Sum256(t.signed)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], t.signature); err != nil {
		return refuse(RuleSignature, "the signature does not verify")
	}
	return nil
}

// Status is what a verified licence means for an extension at a version and a time: a licence for
// another extension is Invalid (ErrWrongExtension); an empty version skips the range (kista stores
// a licence for a name, not a version).
func (c *Claims) Status(ext, version string, now time.Time) (Status, error) {
	if c.Extension != ext {
		return Invalid, refuse(RuleExtMismatch, "for %s, not %s", c.Extension, ext)
	}
	if version != "" && !covers(c.Versions, version) {
		return NotCovered, nil
	}
	return c.StatusAt(now), nil
}

// Verify is the whole check the vectors describe: Parse, Token.Verify and Claims.Status. A refused
// licence is Invalid with the *Error and no claims.
func Verify(token string, keys Keys, ext, version string, now time.Time) (*Claims, Status, error) {
	t, err := Parse(token)
	if err != nil {
		return nil, Invalid, err
	}
	if err := t.Verify(keys); err != nil {
		return nil, Invalid, err
	}
	st, err := t.Status(ext, version, now)
	if err != nil {
		return nil, Invalid, err
	}
	c := t.Claims
	return &c, st, nil
}

// StatusAt is the time rows of the status table: not yet valid, valid, grace or expired.
func (c *Claims) StatusAt(now time.Time) Status {
	s := now.Unix()
	switch {
	case s+int64(Skew/time.Second) < c.IssuedAt:
		return NotYetValid
	case s < c.ExpiresAt:
		return Valid
	case s < c.GraceUntil():
		return Grace
	}
	return Expired
}

// covers reports whether a range admits an extension version. A version that is not dotted
// integers (a commit hash, a leading zero) is outside every range; a range whose min is not below
// its below admits nothing.
func covers(r *Versions, version string) bool {
	if r == nil {
		return true
	}
	if !versRe.MatchString(version) {
		return false
	}
	if r.Min != "" && compareVersions(version, r.Min) < 0 {
		return false
	}
	if r.Below != "" && compareVersions(version, r.Below) >= 0 {
		return false
	}
	return true
}

// compareVersions compares two versions that match versRe, component by component, missing ones
// as zero.
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 4; i++ {
		var x, y int64
		if i < len(pa) {
			x, _ = strconv.ParseInt(pa[i], 10, 64)
		}
		if i < len(pb) {
			y, _ = strconv.ParseInt(pb[i], 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
