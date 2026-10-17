package license

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// maxSafe is the largest integer a licence may carry (2^53-1): every JSON reader holds it exactly.
const maxSafe = 1<<53 - 1

// maxDepth bounds nesting, so a hostile token cannot exhaust the stack: the top-level value is
// level 1, and no array or object is deeper than level 32.
const maxDepth = 32

type kind uint8

const (
	kindNull kind = iota
	kindBool
	kindInt
	kindString
	kindArray
	kindObject
)

// value is a parsed JSON value. Objects keep their members in order.
type value struct {
	kind    kind
	b       bool
	n       int64
	s       string
	arr     []value
	members []member
}

type member struct {
	name string
	v    value
}

// get returns an object's member by name.
func (v value) get(name string) (value, bool) {
	for _, m := range v.members {
		if m.name == name {
			return m.v, true
		}
	}
	return value{}, false
}

// parseJSON parses one JSON text by spec 0012's rules, stricter than encoding/json: strict UTF-8
// (invalid bytes and unpaired surrogate escapes are refused, never replaced), no U+0000 in a string
// (a C string would end there), no duplicate member names (compared after unescaping), integers
// only (no fraction, no exponent) within ±(2^53-1), RFC 8259 whitespace only, at most 32 levels.
// Its errors are *Error with a json_ rule.
func parseJSON(b []byte) (value, error) {
	p := &parser{b: b}
	p.space()
	v, err := p.value(1)
	if err != nil {
		return value{}, err
	}
	p.space()
	if p.i != len(p.b) {
		return value{}, p.fail(RuleJSONSyntax, "data after the JSON value")
	}
	return v, nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) fail(rule, msg string) error {
	return &Error{Rule: rule, Msg: fmt.Sprintf("json: %s at byte %d", msg, p.i)}
}

func (p *parser) space() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) literal(word string) bool {
	if len(p.b)-p.i >= len(word) && string(p.b[p.i:p.i+len(word)]) == word {
		p.i += len(word)
		return true
	}
	return false
}

// value parses a value at a level (the top-level value's is 1).
func (p *parser) value(depth int) (value, error) {
	if p.i >= len(p.b) {
		return value{}, p.fail(RuleJSONSyntax, "unexpected end")
	}
	switch c := p.b[p.i]; {
	case (c == '{' || c == '[') && depth > maxDepth:
		return value{}, p.fail(RuleJSONDepth, "nested deeper than 32 levels")
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		return value{kind: kindString, s: s}, err
	case c == '-' || (c >= '0' && c <= '9'):
		n, err := p.number()
		return value{kind: kindInt, n: n}, err
	case p.literal("true"):
		return value{kind: kindBool, b: true}, nil
	case p.literal("false"):
		return value{kind: kindBool}, nil
	case p.literal("null"):
		return value{kind: kindNull}, nil
	}
	return value{}, p.fail(RuleJSONSyntax, "unexpected character")
}

func (p *parser) object(depth int) (value, error) {
	p.i++ // {
	v := value{kind: kindObject}
	seen := map[string]bool{}
	p.space()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return v, nil
	}
	for {
		p.space()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return value{}, p.fail(RuleJSONSyntax, "expected a member name")
		}
		name, err := p.str()
		if err != nil {
			return value{}, err
		}
		if seen[name] {
			return value{}, p.fail(RuleJSONDuplicate, fmt.Sprintf("duplicate member %q", name))
		}
		seen[name] = true
		p.space()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return value{}, p.fail(RuleJSONSyntax, "expected ':'")
		}
		p.i++
		p.space()
		mv, err := p.value(depth + 1)
		if err != nil {
			return value{}, err
		}
		v.members = append(v.members, member{name: name, v: mv})
		p.space()
		if p.i >= len(p.b) {
			return value{}, p.fail(RuleJSONSyntax, "unexpected end")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return v, nil
		default:
			return value{}, p.fail(RuleJSONSyntax, "expected ',' or '}'")
		}
	}
}

func (p *parser) array(depth int) (value, error) {
	p.i++ // [
	v := value{kind: kindArray}
	p.space()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return v, nil
	}
	for {
		p.space()
		e, err := p.value(depth + 1)
		if err != nil {
			return value{}, err
		}
		v.arr = append(v.arr, e)
		p.space()
		if p.i >= len(p.b) {
			return value{}, p.fail(RuleJSONSyntax, "unexpected end")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return v, nil
		default:
			return value{}, p.fail(RuleJSONSyntax, "expected ',' or ']'")
		}
	}
}

func (p *parser) number() (int64, error) {
	start := p.i
	neg := false
	if p.b[p.i] == '-' {
		neg = true
		p.i++
	}
	if p.i >= len(p.b) || p.b[p.i] < '0' || p.b[p.i] > '9' {
		return 0, p.fail(RuleJSONNumber, "invalid number")
	}
	if p.b[p.i] == '0' && p.i+1 < len(p.b) && p.b[p.i+1] >= '0' && p.b[p.i+1] <= '9' {
		return 0, p.fail(RuleJSONNumber, "leading zero")
	}
	var n int64
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		n = n*10 + int64(p.b[p.i]-'0')
		if n > maxSafe {
			return 0, p.fail(RuleJSONNumber, "integer out of range")
		}
		p.i++
	}
	if p.i < len(p.b) && (p.b[p.i] == '.' || p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		return 0, p.fail(RuleJSONNumber, "not an integer")
	}
	if neg {
		if p.i-start == 2 && n == 0 {
			return 0, nil // -0 is 0
		}
		n = -n
	}
	return n, nil
}

func (p *parser) str() (string, error) {
	p.i++ // "
	var out []byte
	for {
		if p.i >= len(p.b) {
			return "", p.fail(RuleJSONString, "unterminated string")
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return string(out), nil
		case c < 0x20:
			return "", p.fail(RuleJSONString, "control character in a string")
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			if r == 0 {
				return "", p.fail(RuleJSONString, "U+0000 in a string")
			}
			out = utf8.AppendRune(out, r)
		case c < utf8.RuneSelf:
			out = append(out, c)
			p.i++
		default:
			r, size := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && size <= 1 {
				return "", p.fail(RuleJSONString, "invalid UTF-8")
			}
			out = append(out, p.b[p.i:p.i+size]...)
			p.i += size
		}
	}
}

func (p *parser) escape() (rune, error) {
	p.i++ // backslash
	if p.i >= len(p.b) {
		return 0, p.fail(RuleJSONString, "unterminated escape")
	}
	c := p.b[p.i]
	p.i++
	switch c {
	case '"', '\\', '/':
		return rune(c), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if !utf16.IsSurrogate(r) {
			return r, nil
		}
		if r >= 0xDC00 || p.i+1 >= len(p.b) || p.b[p.i] != '\\' || p.b[p.i+1] != 'u' {
			return 0, p.fail(RuleJSONString, "unpaired surrogate")
		}
		p.i += 2
		lo, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if lo < 0xDC00 || lo > 0xDFFF {
			return 0, p.fail(RuleJSONString, "unpaired surrogate")
		}
		return utf16.DecodeRune(r, lo), nil
	}
	return 0, p.fail(RuleJSONString, "invalid escape")
}

func (p *parser) hex4() (rune, error) {
	if len(p.b)-p.i < 4 {
		return 0, p.fail(RuleJSONString, "short \\u escape")
	}
	var r rune
	for _, c := range p.b[p.i : p.i+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, p.fail(RuleJSONString, "invalid \\u escape")
		}
	}
	p.i += 4
	return r, nil
}
