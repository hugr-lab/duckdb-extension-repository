package attachment

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Handing received documents on (specs 0014 and 0045): what a tenant holds from an issuer, as the
// issuer signed it, read the same way for a bundle's export (internal/upstream) and for the relay
// the feed route answers online.

// Held is a statement a tenant holds from an issuer about a body: the stored bytes and signature of
// a document it holds or removed, a statement an export feed kept about a document not held, or one
// it refused for good or left waiting.
type Held struct {
	Seq       int64
	Statement string
	Signature string // base64
	// Doc is the document, when the tenant holds it live; nil for every statement without content here
	Doc *store.Attachment
	// Why a statement has no content here: a document removed here, one never held (a kept
	// statement), or a refusal with its kept reason (a waiting reason for a statement that waits)
	Removed, NotHeld bool
	Refused          string
}

// HeldStatements reads what a tenant holds from an issuer for a body, with seq in (after, upTo], in
// seq order: its documents of the issuer (whichever feed stored them, tombstones too), the
// statements its export feeds kept about documents not held, and the refusals feedID kept, among
// them the statements that wait (spec 0046). At one seq the first of these wins.
func HeldStatements(ctx context.Context, st *store.Store, tenantID, issuer, feedID, body string, after, upTo int64) ([]Held, error) {
	bySeq := map[int64]Held{}
	refusals, err := st.FeedRefusals(ctx, feedID, body, after, upTo)
	if err != nil {
		return nil, err
	}
	for _, r := range refusals {
		bySeq[r.Seq] = Held{Seq: r.Seq, Statement: r.Statement, Signature: r.Signature, Refused: r.Reason}
	}
	kept, err := st.FeedKeptOf(ctx, tenantID, issuer, body, after, upTo)
	if err != nil {
		return nil, err
	}
	for _, k := range kept {
		bySeq[k.Seq] = Held{Seq: k.Seq, Statement: k.Statement, Signature: k.Signature, NotHeld: true}
	}
	docs, err := st.FeedDocumentsOf(ctx, tenantID, issuer, body, after, upTo)
	if err != nil {
		return nil, err
	}
	for i := range docs {
		d := &docs[i]
		h := Held{Seq: d.Seq, Statement: d.Statement, Signature: d.Signature}
		if d.RemovedAt.IsZero() {
			h.Doc = d
		} else {
			h.Removed = true
		}
		bySeq[d.Seq] = h
	}
	out := make([]Held, 0, len(bySeq))
	for _, h := range bySeq {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b Held) int { return int(a.Seq - b.Seq) })
	return out, nil
}

// RelayStoppedText is what a feed's administrators are told while its relayed answers end early
// (spec 0045), with when one last did.
const RelayStoppedText = "Handing on stops early: a relayed answer ended at a statement that does not verify with this feed's pinned keys, " +
	"or the issuer's keys file leaves no room for a statement in an answer (attachments.feed_answer_max); " +
	"kistas that take this tenant's feed receive nothing past it"

// RelayStopped says, for a feed's administrators, that the answers it relays end early (spec 0045),
// and when one last did. "" when none has since its pins or its export last changed.
func RelayStopped(f store.Feed) string {
	if f.RelayStoppedAt.IsZero() {
		return ""
	}
	return RelayStoppedText + " (last at " + f.RelayStoppedAt.UTC().Format(time.RFC3339) + ")."
}

// NoteNotHeld is the relay's note on a statement it sends without content and does not wait on: a
// document removed or refused at the relay, or of a kind its export feed does not take (spec 0045).
const NoteNotHeld = "not held by the relay"

// MaxNote bounds the unsigned note on a statement without content, in a bundle and from a relay.
const MaxNote = 1000

// Text strips control and format characters from s and bounds it to n bytes: an exporting side's
// note is shown and stored as data.
func Text(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// NormalFeedURL normalises a feed URL (spec 0011): https://<host>[:port][/<path>]/api/v1/tenants/<t>,
// no user info, query, fragment or escapes, the host in lower case, the default port dropped.
func NormalFeedURL(raw string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: feed URL %q: %s", store.ErrInvalid, raw, why)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return bad("not an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.RawPath != "" {
		return bad("no user info, query, fragment or escapes")
	}
	if u.Scheme != "https" {
		return bad("not https")
	}
	if !feedPath.MatchString(u.Path) || slices.ContainsFunc(strings.Split(u.Path, "/"), func(seg string) bool { return seg == "." || seg == ".." }) {
		return bad("not …/api/v1/tenants/<tenant>")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return bad("its port")
		}
		if n == 443 { // the default port is dropped: one feed, one form (its statements' issuer)
			u.Host = u.Hostname()
			if strings.Contains(u.Host, ":") {
				u.Host = "[" + u.Host + "]"
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// handedOn is the tenant's feed that hands an issuer's documents on (spec 0045): its online feed
// of the issuer with export set, once it kept a keys file. A tenant hands on at most one feed per
// issuer (the store refuses export on a second), so there is none to choose between. false: the
// tenant does not relay the issuer.
func (s *Service) handedOn(ctx context.Context, tenantID, issuer string) (store.Feed, bool, error) {
	var feed store.Feed
	found := false
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		all, err := tx.ListFeeds(ctx, tenantID)
		found = false
		for _, f := range all {
			if f.URL == issuer && f.Mode == store.FeedOnline && f.Export && !f.KeysFileAt.IsZero() {
				feed, found = f, true
				break
			}
		}
		return err
	})
	return feed, found, err
}

// takes reports whether a feed takes a kind (none named: every kind).
func takes(f store.Feed, kind string) bool {
	return len(f.Kinds) == 0 || slices.Contains(f.Kinds, kind)
}

// issuerParam checks the issuer a relay's caller names: a feed URL in its normalised form.
func issuerParam(issuer string) error {
	if u, err := NormalFeedURL(issuer); err != nil || u != issuer {
		return fmt.Errorf("%w: issuer is a feed URL in its normalised form (https://<host>/api/v1/tenants/<tenant>)", store.ErrInvalid)
	}
	return nil
}

// relayOne is the room one statement takes in an answer at most (its bytes and signature in base64,
// a note, its body's high-water): an answer always has room for the keys file and one statement.
const relayOne = 24 << 10

// relay answers a subscriber's bodies with what the tenant holds from another issuer (spec 0045):
// only for an issuer of which the tenant has an online export feed with a keys file (any other
// issuer answers as bodies without documents, with no keys), and only for the bodies in the caller's
// scope. Per body, from the caller's cursor to that feed's, in seq order: the issuer's stored bytes
// and signatures, unchanged. A statement goes without content, with a note, when the relay holds no
// content for it or its kind is one the feed does not take: the waiting reason for one the relay
// waits on, else NoteNotHeld. Each statement is verified against the feed's own pinned keys before
// it is sent: a body's answer ends just below one that does not verify, and the feed's record then
// says so to its administrators. The relay applies no kind filter of its own and signs nothing.
// keys is the issuer's keys file as the feed last fetched it. The answer holds at most the
// configured bytes as the API writes them, the keys file included, then more. The reads are not one
// snapshot: a body whose cursor changed meanwhile, and every body when the feed's export setting
// did, is answered nothing, as a bundle's export fails such a feed.
func (s *Service) relay(ctx context.Context, t store.Tenant, issuer string, bodies []FeedBody, inScope map[string]bool) (FeedAnswer, error) {
	out := FeedAnswer{Statements: []WireStatement{}, Bodies: []HighWater{}}
	feed, ok, err := s.handedOn(ctx, t.ID, issuer)
	if err != nil || !ok {
		return out, err
	}
	var keys []byte
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		keys, _, err = tx.FeedKeysFile(ctx, feed.ID)
		return err
	}); err != nil {
		return out, err
	}
	listed, err := ParseKeysFile(keys)
	if len(keys) == 0 || err != nil {
		return out, nil // dropped meanwhile (export switched off): not handed on
	}
	// the keys file as the API writes it (compact, HTML-escaped): what the answer's bound counts. One
	// that leaves no room for a statement cannot be served within the bound: not handed on
	wire, err := json.Marshal(json.RawMessage(keys))
	if err != nil || len(wire)+relayOne > s.feedAnswerMax() {
		s.relayStopped(ctx, t, feed, "attachment: an issuer's keys file leaves no room for a statement in a feed answer (attachments.feed_answer_max): it is not handed on",
			"issuer", issuer, "bytes", len(wire))
		return out, nil
	}
	var asked []string
	for _, b := range bodies {
		if inScope[b.BodyHash] {
			asked = append(asked, b.BodyHash)
		}
	}
	cursors, err := s.Store.FeedCursorsOf(ctx, feed.ID, asked)
	if err != nil {
		return out, err
	}
	pins := map[string]*rsa.PublicKey{}
	for fp, k := range listed {
		if slices.Contains(feed.Keys, fp) {
			pins[fp] = k
		}
	}
	type part struct { // one body's part of the answer
		body  string
		stmts []WireStatement
		high  int64
	}
	var parts []part
	var stopped []any // where an answer first ended at a statement that does not verify
	size := len(wire) + 64
	for _, b := range bodies {
		upTo := cursors[b.BodyHash]
		if !inScope[b.BodyHash] || upTo <= b.AfterSeq {
			continue
		}
		held, err := HeldStatements(ctx, s.Store, t.ID, issuer, feed.ID, b.BodyHash, b.AfterSeq, upTo)
		if err != nil {
			return out, err
		}
		p := part{body: b.BodyHash, high: b.AfterSeq}
		full := false
		for _, h := range held {
			stmt := []byte(h.Statement)
			sig, err := base64.StdEncoding.DecodeString(h.Signature)
			var st Statement
			malformed := false
			if err == nil {
				st, err = Verify(stmt, sig, pins)
				malformed = errors.Is(err, ErrMalformed) && h.Doc == nil
			}
			if err != nil && !malformed || st.Issuer != issuer || st.BodyHash != b.BodyHash || st.Seq != h.Seq {
				// as an unverified statement holds a cursor: nothing of the body goes past it
				if stopped == nil {
					stopped = []any{"body", b.BodyHash, "seq", h.Seq}
				}
				break
			}
			w := WireStatement{Statement: base64.StdEncoding.EncodeToString(stmt), Signature: h.Signature}
			switch {
			case malformed:
				w.Note = NoteNotHeld
			case st.Withdrawn:
			case Waits(h.Refused):
				w.Note = h.Refused
			case h.Doc == nil || !takes(feed, st.Kind) || h.Doc.ContentHash != st.ContentHash || h.Doc.Size != st.Size:
				w.Note = NoteNotHeld
			}
			n := len(w.Statement) + len(w.Signature) + len(w.Note) + 60
			if size+n+100 > s.feedAnswerMax() {
				full = true
				break
			}
			size += n
			p.stmts = append(p.stmts, w)
			p.high = h.Seq
		}
		if p.high > b.AfterSeq {
			parts = append(parts, p)
			size += 100
		}
		if full {
			out.More = true
			break
		}
	}
	if s.afterRelayRead != nil {
		s.afterRelayRead()
	}
	if stopped != nil {
		s.relayStopped(ctx, t, feed, "attachment: a relayed answer ends at a statement that does not verify under the feed's pinned keys", stopped...)
	}
	// the reads above are not one snapshot: a change of export (what the feed kept dropped, its
	// cursors reset) or the collector (a body's rows gone with its cursor) meanwhile would leave an
	// answer that hides statements. Such a body is answered nothing; the subscriber asks again
	now, ok, err := s.handedOn(ctx, t.ID, issuer)
	if err != nil {
		return out, err
	}
	if !ok {
		return FeedAnswer{Statements: []WireStatement{}, Bodies: []HighWater{}}, nil
	}
	out.Keys = wire
	if now.ID != feed.ID || now.ExportEpoch != feed.ExportEpoch {
		return out, nil
	}
	again, err := s.Store.FeedCursorsOf(ctx, feed.ID, asked)
	if err != nil {
		return out, err
	}
	for _, p := range parts {
		if again[p.body] < cursors[p.body] {
			continue
		}
		out.Statements = append(out.Statements, p.stmts...)
		out.Bodies = append(out.Bodies, HighWater{BodyHash: p.body, Seq: p.high})
	}
	return out, nil
}

// relayStopped records on a feed that a relayed answer ended early, and logs why: both at most
// once a minute for the feed, whatever the requests (the record's conditional update is the limit).
func (s *Service) relayStopped(ctx context.Context, t store.Tenant, f store.Feed, msg string, args ...any) {
	wrote, err := s.Store.MarkFeedRelayStopped(ctx, f)
	switch {
	case err != nil:
		s.warn("attachment: recording that a relayed answer ended early", "tenant", t.Name, "feed", f.Name, "error", err)
	case wrote:
		s.warn(msg, append([]any{"tenant", t.Name, "feed", f.Name}, args...)...)
	}
}

// warn logs a warning when the service has a log.
func (s *Service) warn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}

// relayDocument hands a document the tenant holds from another issuer, and its content, to use
// (spec 0045): a live, unwithdrawn document of an issuer the tenant hands on, of a kind an export
// feed of that issuer takes, about a body in the caller's scope; any other answers like a missing
// one.
func (s *Service) relayDocument(ctx context.Context, t store.Tenant, channels []string, issuer, issuerID string, use func(store.Attachment, []byte) error) error {
	missing := fmt.Errorf("%w: document %s", store.ErrNotFound, issuerID)
	feed, ok, err := s.handedOn(ctx, t.ID, issuer)
	if err != nil {
		return err
	}
	if !ok {
		return missing
	}
	var rec store.Attachment
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		rec, err = tx.FeedAttachment(ctx, t.ID, issuer, issuerID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return missing
	}
	if err != nil {
		return err
	}
	if rec.Origin != store.AttachedByFeed || !rec.RemovedAt.IsZero() || !rec.WithdrawnAt.IsZero() || !takes(feed, rec.Kind) {
		return missing
	}
	inScope, err := s.Store.ReleasedIn(ctx, t.ID, channels, []string{rec.BodyHash})
	if err != nil {
		return err
	}
	if !inScope[rec.BodyHash] {
		return missing
	}
	return s.Blob.ReadAttachment(ctx, t.StorageDomain, rec.ID, rec.ContentHash, rec.Size, func(b []byte) error { return use(rec, b) })
}
