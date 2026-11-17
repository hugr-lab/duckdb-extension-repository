package attachment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The report kind (spec 0045): one JSON object holding a few summary members kista reads and the
// issuer's page (html), which kista never parses. The members read are the kind's floor, frozen for
// every envelope version: name, type, bound and meaning. Nothing else of the document is read and no
// version is interpreted, so a report of a later envelope is stored and shown exactly as v1 is, and a
// report never waits on a document version (spec 0046).

// Bounds of a report's read members: the frozen floor, the same in every envelope version.
const (
	reportMaxNumber    = 1 << 31 // envelope's N, issue, kinds_evaluated, an attention count
	reportMaxID        = 128
	reportMaxWord      = 32
	reportMaxBodies    = 1000
	reportMaxAttention = 32
	reportMaxSections  = 16
)

var (
	reportEnvelopeRE = regexp.MustCompile(`^kista\.report\.v([1-9][0-9]{0,9})$`)
	reportIDRE       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	reportWordRE     = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)
)

// members reads a JSON object's members by their exact names, each as its raw value. Go's decoder
// matches names without regard to case and keeps the last of two silently, so a document could say
// one thing to this reader and another to the next: a member named in read that the object carries
// twice, in any letter case, is refused. Members not named in read are carried unread.
func members(raw json.RawMessage, read ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	out := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		for _, r := range read {
			if !strings.EqualFold(name, r) {
				continue
			}
			if seen[r] {
				return nil, fmt.Errorf("the member %s twice", r)
			}
			seen[r] = true
			if name == r {
				out[r] = v
			}
		}
	}
	return out, nil
}

// reportInt reads an integer of 0 to 2^31 written without a fraction or an exponent.
func reportInt(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if s == "" || len(s) > 10 || strings.Trim(s, "0123456789") != "" || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n <= reportMaxNumber
}

// reportString reads a JSON string.
func reportString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// reportList reads a JSON array of at most max objects, each member of read a string matching re or,
// for the member named count, an integer of 0 to 2^31. It answers the entries with those members
// only, in the order read names them.
func reportList(raw json.RawMessage, max int, count string, read ...string) ([]map[string]any, bool) {
	var entries []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > max {
		return nil, false
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		m, err := members(e, read...)
		if err != nil {
			return nil, false
		}
		entry := map[string]any{}
		for _, name := range read {
			if name == count {
				n, ok := reportInt(m[name])
				if !ok {
					return nil, false
				}
				entry[name] = n
				continue
			}
			s, ok := reportString(m[name])
			if !ok || !reportWordRE.MatchString(s) {
				return nil, false
			}
			entry[name] = s
		}
		out = append(out, entry)
	}
	return out, true
}

// report reads a report attached to, or received for, the body bodyHash and computes its summary:
// the read members without html, with bodies (their number) and read (this body's) in place of
// subject. envelope, report, issue, issued_at, depth, subject.bodies and html are strict: a
// document that breaks one is refused, and so is one whose subject.bodies does not list the body. A
// summary list that breaks its rule (attention, sections, kinds_evaluated) does not lose the report:
// that member is left out of the summary, which is marked incomplete. The bounds make the largest
// summary fit maxSummary, and every string has a grammar: none needs scrubbing.
func report(content []byte, bodyHash string) (map[string]any, error) {
	bad := func(why string) (map[string]any, error) {
		return nil, fmt.Errorf("%w: a report's %s", ErrDocument, why)
	}
	m, err := members(content, "envelope", "report", "issue", "issued_at", "depth", "subject", "attention", "kinds_evaluated", "sections", "html")
	if err != nil {
		return nil, fmt.Errorf("%w: not a report: %v", ErrDocument, err)
	}
	envelope, ok := reportString(m["envelope"])
	if g := reportEnvelopeRE.FindStringSubmatch(envelope); !ok || g == nil {
		return bad("envelope is kista.report.v<N>")
	} else if n, err := strconv.ParseInt(g[1], 10, 64); err != nil || n > reportMaxNumber {
		return bad("envelope is kista.report.v<N>, N at most 2^31")
	}
	id, ok := reportString(m["report"])
	if !ok || !reportIDRE.MatchString(id) {
		return bad(fmt.Sprintf("report is 1 to %d bytes of [A-Za-z0-9._:-]", reportMaxID))
	}
	issue, ok := reportInt(m["issue"])
	if !ok {
		return bad("issue is an integer of 0 to 2^31")
	}
	issuedAt, ok := reportString(m["issued_at"])
	at, err := time.Parse(time.RFC3339, issuedAt)
	if !ok || err != nil {
		return bad("issued_at is RFC 3339")
	}
	depth, ok := reportString(m["depth"])
	if !ok || !reportWordRE.MatchString(depth) {
		return bad(fmt.Sprintf("depth is 1 to %d bytes of [a-z0-9_.-]", reportMaxWord))
	}
	if h := m["html"]; len(h) == 0 || h[0] != '"' {
		return bad("html is a string") // checked as JSON already; never parsed, never read further
	}
	subject, err := members(m["subject"], "bodies")
	if err != nil {
		return bad("subject.bodies is 1 to 1000 of {hash, read}")
	}
	var bodies []json.RawMessage
	if b := subject["bodies"]; len(b) == 0 || b[0] != '[' || json.Unmarshal(b, &bodies) != nil || len(bodies) == 0 || len(bodies) > reportMaxBodies {
		return bad(fmt.Sprintf("subject.bodies is 1 to %d of {hash, read}", reportMaxBodies))
	}
	listed, read := false, true
	for _, raw := range bodies {
		b, err := members(raw, "hash", "read")
		if err != nil {
			return bad("subject.bodies entry is {hash, read}")
		}
		hash, ok := reportString(b["hash"])
		r := string(b["read"])
		if !ok || !hexHash(hash) || r != "true" && r != "false" {
			return bad("subject.bodies entry is {hash: 64 lowercase hex, read: a boolean}")
		}
		if hash == bodyHash {
			// a body listed twice was read only if every entry says so
			listed, read = true, read && r == "true"
		}
	}
	if !listed {
		return bad("subject.bodies does not list the body it is attached to")
	}
	sum := map[string]any{"envelope": envelope, "report": id, "issue": issue, "issued_at": at.UTC().Format(time.RFC3339), "depth": depth,
		"bodies": len(bodies), "read": read}
	incomplete := false
	if raw, ok := m["attention"]; !ok {
		sum["attention"] = []map[string]any{}
	} else if list, ok := reportList(raw, reportMaxAttention, "count", "kind", "count"); ok {
		sum["attention"] = list
	} else {
		incomplete = true
	}
	if raw, ok := m["kinds_evaluated"]; ok {
		if n, ok := reportInt(raw); ok {
			sum["kinds_evaluated"] = n
		} else {
			incomplete = true
		}
	}
	if raw, ok := m["sections"]; ok {
		if list, ok := reportList(raw, reportMaxSections, "", "name", "status"); ok {
			sum["sections"] = list
		} else {
			incomplete = true
		}
	}
	if incomplete {
		sum["incomplete"] = true
	}
	return sum, nil
}
