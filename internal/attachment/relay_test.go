package attachment_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0045 at the feed: a feed taken through a relay (via), the relay the feed route answers, the
// document quota holding a cursor, and a chain issuer → relaying tenant → own kista.

const relayURL = "https://relay.example/api/v1/tenants/org"

var downstream = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/kc|own-kista", Tenant: "acme"}

// feedGrant grants one actor feed on the channel prod, beside the test's other rights.
type feedGrant struct {
	authz.Authorizer
	who string
}

func (g feedGrant) Allow(ctx context.Context, a authz.Actor, verb authz.Verb, res authz.Resource) error {
	if verb == authz.VerbFeed && a.String() == g.who && res.Channel == "prod" {
		return nil
	}
	return g.Authorizer.Allow(ctx, a, verb, res)
}

// reportJSON is a report about the bodies, at an issue.
func reportJSON(issue int, bodies ...string) []byte {
	var listed []any
	for _, b := range bodies {
		listed = append(listed, map[string]any{"hash": b, "read": true})
	}
	b, _ := json.Marshal(map[string]any{"envelope": "kista.report.v1", "report": "rep-1", "issue": issue, "issued_at": "2026-11-16T10:00:00Z",
		"depth": "static", "subject": map[string]any{"bodies": listed}, "attention": []any{map[string]any{"kind": "network", "count": issue}},
		"kinds_evaluated": 4, "html": "<h1>Report</h1><p>issue " + fmt.Sprint(issue) + "</p>"})
	return b
}

func asReport(st *attachment.Statement) {
	st.Kind, st.MediaType, st.Title, st.Audience = store.KindReport, attachment.Report, "Report", "administrators"
}

func viaFeed(sp *attachment.FeedSpec) { sp.Via, sp.Credential = relayURL, "relay" }

// newViaSubscriber is a subscriber whose stand-in source plays a relay of the issuer srcURL.
func newViaSubscriber(t *testing.T) *subscriber {
	s := newSubscriber(t)
	s.src.via = relayURL
	s.src.notes = map[int64]string{}
	s.src.highWater = map[string]int64{}
	return s
}

func (s *subscriber) cursor(f store.Feed, body string) int64 {
	s.t.Helper()
	cur, err := s.st.FeedCursors(ctx, f.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return cur[body]
}

// A feed's via: where it may be set, and what it is matched against.
func TestViaFeedAdd(t *testing.T) {
	s := newViaSubscriber(t)
	f := s.add("own", viaFeed)
	if f.Via != relayURL || f.URL != srcURL {
		t.Fatalf("the feed: %+v", f)
	}
	if got := s.feed("own"); got.Via != relayURL {
		t.Fatalf("read back: %+v", got)
	}
	for name, mod := range map[string]func(*attachment.FeedSpec){
		"a credential for the issuer's host, not the relay's": func(sp *attachment.FeedSpec) { sp.Via = relayURL },
		"via the issuer's own URL":                            func(sp *attachment.FeedSpec) { sp.Via = srcURL },
		"via not a feed URL":                                  func(sp *attachment.FeedSpec) { sp.Via, sp.Credential = "https://relay.example/", "relay" },
		"via over http": func(sp *attachment.FeedSpec) {
			sp.Via, sp.Credential = strings.Replace(relayURL, "https", "http", 1), "relay"
		},
		"a bundle-mode feed": func(sp *attachment.FeedSpec) {
			sp.Via, sp.Mode, sp.Credential = relayURL, store.FeedBundle, ""
		},
	} {
		sp := attachment.FeedSpec{Name: "bad", URL: srcURL, Credential: "src", Keys: []string{s.src.fp}, Offer: store.OfferChannels, Channels: []string{"prod"}}
		mod(&sp)
		if _, err := s.feeds.Add(ctx, admin, "acme", sp); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// via is normalised as a feed URL is, and a change of the credential is matched against it
	g := s.add("norm", func(sp *attachment.FeedSpec) {
		sp.Via, sp.Credential = "https://RELAY.example:443/api/v1/tenants/org", "relay"
	})
	if g.Via != relayURL {
		t.Fatalf("via as stored: %q", g.Via)
	}
	issuers := "src"
	if _, err := s.feeds.Change(ctx, admin, "acme", "norm", attachment.FeedChange{Credential: &issuers}, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a via feed took a credential for its issuer's host: %v", err)
	}
}

// A via feed against a stand-in relay: it posts the issuer to the relay, takes its keys from the
// answers, fetches content there, and checks every statement and document as a direct feed does.
func TestViaFeed(t *testing.T) {
	t.Run("statements and documents arrive through the relay", func(t *testing.T) {
		s := newViaSubscriber(t)
		s.src.offer(s.body, "p1", reportJSON(1, s.body), asReport, nil)
		s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		f := s.add("own", viaFeed)
		res := s.run("own")
		if res.Error != "" || res.Counts["received"] != 2 || res.Counts["refused"] != 0 {
			t.Fatalf("the run: %+v", res)
		}
		docs := s.docs()
		if docs["p1"].Kind != store.KindReport || docs["p1"].Issuer != srcURL || docs["p1"].KeyFP != s.src.fp ||
			!strings.Contains(docs["p1"].Summary, `"issue":1`) || docs["r1"].Kind != store.KindReview {
			t.Fatalf("stored: %+v", docs)
		}
		if s.feed("own").Status != store.FeedOK || s.cursor(f, s.body) != 2 {
			t.Fatalf("the feed: %+v", s.feed("own"))
		}
	})

	t.Run("an altered statement holds its body", func(t *testing.T) {
		s := newViaSubscriber(t)
		s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		two := s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil)
		s.src.offer(s.body, "r3", []byte(reviewDoc), nil, nil)
		s.src.spoil(two)
		f := s.add("own", viaFeed)
		res := s.run("own")
		if res.Counts["received"] != 1 || reasons(res)["signature"] != 1 || s.cursor(f, s.body) != 1 || len(s.docs()) != 1 {
			t.Fatalf("the run: %+v, cursor %d", res, s.cursor(f, s.body))
		}
	})

	t.Run("altered content is refused", func(t *testing.T) {
		s := newViaSubscriber(t)
		s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		s.src.docs["r1"] = []byte(strings.Replace(reviewDoc, "approved", "rejected", 1)) // the same size
		s.add("own", viaFeed)
		res := s.run("own")
		if res.Counts["received"] != 0 || reasons(res)["content"] != 1 || len(s.docs()) != 0 {
			t.Fatalf("the run: %+v", res)
		}
	})

	t.Run("a report whose body list leaves the body out is refused", func(t *testing.T) {
		s := newViaSubscriber(t)
		s.src.offer(s.body, "p1", reportJSON(1, s.other), asReport, nil) // re-bound to another body
		s.add("own", viaFeed)
		res := s.run("own")
		if res.Counts["received"] != 0 || reasons(res)["document"] != 1 || len(s.docs()) != 0 {
			t.Fatalf("the run: %+v", res)
		}
	})

	t.Run("none of the pinned keys: failing, and no fingerprint of the relay's is shown", func(t *testing.T) {
		s := newViaSubscriber(t)
		s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		pinned := "sha256:" + strings.Repeat("7", 64)
		s.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Keys = []string{pinned} })
		res := s.run("own")
		const want = "none of the pinned keys is in what the relay serves; take the issuer's current fingerprint from the issuer"
		got := s.feed("own")
		if !strings.Contains(res.Error, want) || got.Status != store.FeedFailing || !strings.Contains(got.LastError, want) || len(s.docs()) != 0 {
			t.Fatalf("the run: %+v; the feed: %s %q", res, got.Status, got.LastError)
		}
		hex := strings.TrimPrefix(s.src.fp, "sha256:")
		for _, text := range []string{res.Error, got.LastError, got.LastRun} {
			if strings.Contains(text, hex) || strings.Contains(text, hex[:16]) {
				t.Fatalf("the relay's fingerprint is shown: %s", text)
			}
		}
		// the same when the relay's keys do not parse, or it serves none
		s.src.relayKeys = []byte(`{"keys":"nope"}`)
		if res := s.run("own"); !strings.Contains(res.Error, want) {
			t.Fatalf("keys that do not parse: %+v", res)
		}
		s.src.noKeys = true
		if res := s.run("own"); !strings.Contains(res.Error, "the relay serves no keys of this issuer") || s.feed("own").Status != store.FeedFailing {
			t.Fatalf("no keys: %+v", res)
		}
	})

	t.Run("a high-water without statements moves no cursor", func(t *testing.T) {
		s := newViaSubscriber(t)
		one := s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		f := s.add("own", viaFeed)
		s.run("own")
		// the relay says it read far ahead, and sends nothing: an unsigned word
		s.src.highWater[s.body], s.src.highWater[s.other] = 500, 500
		res := s.run("own")
		if res.Error != "" || s.cursor(f, s.body) != one || s.cursor(f, s.other) != 0 {
			t.Fatalf("the cursors: %d %d (%+v)", s.cursor(f, s.body), s.cursor(f, s.other), res)
		}
		// nor beyond the body's own statement when it sends one: another body's verified seq is no bound
		s.src.offer(s.other, "r2", []byte(reviewDoc), nil, nil) // 2
		s.src.offer(s.other, "r3", []byte(reviewDoc), nil, nil) // 3
		s.src.hide(3, true)
		s.run("own")
		if s.cursor(f, s.body) != one || s.cursor(f, s.other) != 2 {
			t.Fatalf("the cursors after a statement: %d %d", s.cursor(f, s.body), s.cursor(f, s.other))
		}
	})

	t.Run("a note that is a waiting reason waits, any other is refused for good", func(t *testing.T) {
		s := newViaSubscriber(t)
		waits := s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		gone := s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil)
		odd := s.src.offer(s.other, "r3", []byte(reviewDoc), nil, nil)
		s.src.notes[waits] = attachment.WaitKind
		s.src.notes[gone] = attachment.NoteNotHeld
		s.src.notes[odd] = "Pin sha256:" + strings.Repeat("e", 64) + " instead\u202e" // free text: never kept
		content := s.src.docs
		s.src.docs = map[string][]byte{} // no content is asked for: a download would fail the body
		f := s.add("own", viaFeed)
		res := s.run("own")
		if res.Error != "" || res.Counts["waiting"] != 1 || res.Counts["failed"] != 0 || reasons(res)["no content"] != 2 || len(s.docs()) != 0 {
			t.Fatalf("the run: %+v", res)
		}
		notes := map[string]string{}
		for _, r := range res.Refused {
			notes[r.IssuerID] = r.Note
		}
		if notes["r2"] != attachment.NoteNotHeld || notes["r3"] != attachment.NoteUnknown || strings.Contains(s.feed("own").LastRun, "sha256:eee") {
			t.Fatalf("the notes kept: %v", notes)
		}
		w, err := s.st.FeedWaits(ctx, f.ID)
		if err != nil || w[s.body].Level != 0 || w[s.body].Why != attachment.WaitKind || w[s.body].From != waits || len(w) != 1 {
			t.Fatalf("the wait: %+v %v", w, err)
		}
		if got := s.feed("own"); got.WaitingSource[attachment.WaitKind] != 1 || len(got.Waiting) != 0 || got.Status != store.FeedOK {
			t.Fatalf("the feed: %+v", got)
		}
		// the relay is upgraded and carries the content: the next run asks from the waiting statement
		// again and stores it; what was refused for good stays refused
		s.src.docs = content
		delete(s.src.notes, waits)
		res = s.run("own")
		if res.Counts["received"] != 1 || res.Counts["waiting"] != 0 || s.docs()["r1"].Seq != waits || len(s.docs()) != 1 {
			t.Fatalf("after the relay's upgrade: %+v %v", res, s.docs())
		}
		if w, _ := s.st.FeedWaits(ctx, f.ID); len(w) != 0 {
			t.Fatalf("still waiting: %+v", w)
		}
	})

	t.Run("an export feed keeps a statement the relay waits on under the relay's reason", func(t *testing.T) {
		s := newViaSubscriber(t)
		one := s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
		s.src.notes[one] = attachment.WaitDocument
		f := s.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Export = true })
		if res := s.run("own"); res.Counts["waiting"] != 1 {
			t.Fatalf("the run: %+v", res)
		}
		kept, err := s.st.FeedRefusals(ctx, f.ID, s.body, 0, 100)
		if err != nil || len(kept) != 1 || kept[0].Reason != attachment.WaitDocument || kept[0].IssuerID != "r1" {
			t.Fatalf("kept: %+v %v", kept, err)
		}
		if got := s.feed("own"); got.KeysFileAt.IsZero() {
			t.Fatal("a via feed with export kept no keys file")
		}
	})
}

// The document quota holds a body's cursor, it does not refuse (spec 0045): the run ends failing
// (limits) and the document is taken once there is room. The heads limit still refuses.
func TestFeedQuotaHolds(t *testing.T) {
	s := newSubscriber(t)
	wasMax, wasHeads := store.MaxTenantAttachments, store.MaxAttachmentHeads
	defer func() { store.MaxTenantAttachments, store.MaxAttachmentHeads = wasMax, wasHeads }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)  // 1
	s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil)  // 2: over the quota
	s.src.offer(s.body, "r3", []byte(reviewDoc), nil, nil)  // 3: behind it
	s.src.offer(s.other, "r4", []byte(reviewDoc), nil, nil) // 4: over the quota too
	f := s.add("src", nil)
	res := s.run("src")
	if res.Counts["received"] != 1 || res.Counts["held"] != 2 || res.Counts["refused"] != 0 || res.Error == "" {
		t.Fatalf("the run at the quota: %+v", res)
	}
	if got := s.feed("src"); got.Status != store.FeedFailing || s.cursor(f, s.body) != 1 || s.cursor(f, s.other) != 0 {
		t.Fatalf("the feed: %s, cursors %d %d", got.Status, s.cursor(f, s.body), s.cursor(f, s.other))
	}
	// the object of the document that did not fit is not left behind
	tn, _ := s.st.GetTenant(ctx, "acme")
	if all, _ := s.st.BodyAttachments(ctx, tn.ID, s.body); len(all) != 1 {
		t.Fatalf("records: %d", len(all))
	}
	// room: the same documents arrive, none was lost
	store.MaxTenantAttachments = wasMax
	store.MaxAttachmentHeads = 3
	res = s.run("src")
	if res.Error != "" || res.Counts["received"] != 3 || s.feed("src").Status != store.FeedOK || len(s.docs()) != 4 {
		t.Fatalf("the run with room: %+v", res)
	}
	// the heads limit is per issuer and refuses for good, as before
	s.src.offer(s.body, "r5", []byte(reviewDoc), nil, nil) // a fourth head of the kind on the body
	res = s.run("src")
	if res.Error != "" || reasons(res)["limits"] != 1 || res.Counts["held"] != 0 || s.cursor(f, s.body) != 5 {
		t.Fatalf("the run at the heads limit: %+v", res)
	}
}

// relayOf is tenant acme of a subscriber as a relay: an export feed of the issuer, and a holder of
// feed on prod.
func relayOf(s *subscriber) {
	s.svc.Authz = feedGrant{s.svc.Authz, downstream.String()}
}

func decoded(t *testing.T, w attachment.WireStatement) attachment.Statement {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(w.Statement)
	if err != nil {
		t.Fatal(err)
	}
	var st attachment.Statement
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// The relay the feed route answers: its scope, equal answers for what it does not hand on, content
// only for the kinds an export feed takes, the notes, verification before sending, and paging.
func TestRelay(t *testing.T) {
	s := newSubscriber(t)
	relayOf(s)
	src := s.src
	src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)                                                                                        // 1
	src.offer(s.body, "p1", reportJSON(1, s.body), asReport, nil)                                                                               // 2
	src.offer(s.body, "s1", []byte(sbom), func(st *attachment.Statement) { st.Kind, st.MediaType = store.KindSBOM, attachment.CycloneDX }, nil) // 3
	src.offer(s.other, "r2", []byte(reviewDoc), nil, nil)                                                                                       // 4
	src.offer(s.body, "a1", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil)       // 5: waits here
	s.add("src", func(sp *attachment.FeedSpec) {
		sp.Export, sp.Kinds = true, []string{store.KindReview, store.KindReport}
	})
	// a second feed of the issuer, not for export, takes every kind: it stores the SBOM
	s.add("all", nil)
	if res := s.run("src"); res.Error != "" || res.Counts["received"] != 3 || reasons(res)["kind"] != 2 {
		t.Fatalf("the export feed's run: %+v", res)
	}
	if res := s.run("all"); res.Error != "" || res.Counts["received"] != 1 || res.Counts["waiting"] != 1 {
		t.Fatalf("the other feed's run: %+v", res)
	}
	ask := func(a authz.Actor, issuer string, bodies ...attachment.FeedBody) (attachment.FeedAnswer, error) {
		return s.svc.Feed(ctx, a, "acme", bodies, issuer)
	}
	both := []attachment.FeedBody{{BodyHash: s.body}, {BodyHash: s.other}}

	ans, err := ask(downstream, srcURL, both...)
	if err != nil || ans.More || !bytes.Equal(ans.Keys, src.keysFile()) {
		t.Fatalf("the answer: %+v %v", ans, err)
	}
	type sent struct {
		id   string
		seq  int64
		note string
	}
	var got []sent
	for i, w := range ans.Statements {
		st := decoded(t, w)
		got = append(got, sent{st.IssuerID, st.Seq, w.Note})
		// the issuer's bytes, unchanged
		for _, o := range src.stmts {
			if o.seq == st.Seq && (base64.StdEncoding.EncodeToString(o.stmt) != w.Statement || base64.StdEncoding.EncodeToString(o.sig) != w.Signature) {
				t.Errorf("statement %d is not the issuer's bytes", i)
			}
		}
	}
	// the one feed that hands the issuer on left the SBOM and audit out by its kinds: they are
	// answered as it has them, not held, though another feed of the issuer stored the SBOM
	want := []sent{{"r1", 1, ""}, {"p1", 2, ""}, {"s1", 3, attachment.NoteNotHeld}, {"a1", 5, attachment.NoteNotHeld}, {"r2", 4, ""}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sent:\n got %v\nwant %v", got, want)
	}
	if fmt.Sprint(ans.Bodies) != fmt.Sprint([]attachment.HighWater{{BodyHash: s.body, Seq: 5}, {BodyHash: s.other, Seq: 4}}) {
		t.Fatalf("read through: %v", ans.Bodies)
	}
	// from a cursor on
	if ans, _ := ask(downstream, srcURL, attachment.FeedBody{BodyHash: s.body, AfterSeq: 2}); len(ans.Statements) != 2 || decoded(t, ans.Statements[0]).Seq != 3 {
		t.Fatalf("after 2: %+v", ans)
	}

	// content only for kinds the export feed of the issuer takes: the SBOM (held, stored by the feed
	// that is not for export) and audit go without content
	ans, _ = ask(downstream, srcURL, attachment.FeedBody{BodyHash: s.body})
	notes := map[string]string{}
	for _, w := range ans.Statements {
		notes[decoded(t, w).IssuerID] = w.Note
	}
	if len(ans.Statements) != 4 || notes["r1"] != "" || notes["p1"] != "" || notes["s1"] != attachment.NoteNotHeld || notes["a1"] != attachment.NoteNotHeld {
		t.Fatalf("notes by kind: %v", notes)
	}
	content := func(a authz.Actor, id, issuer string) ([]byte, error) {
		var out []byte
		err := s.svc.FeedDocument(ctx, a, "acme", id, issuer, func(_ store.Attachment, b []byte) error { out = bytes.Clone(b); return nil })
		return out, err
	}
	if b, err := content(downstream, "r1", srcURL); err != nil || string(b) != reviewDoc {
		t.Fatalf("a review's content: %q %v", b, err)
	}
	if b, err := content(downstream, "p1", srcURL); err != nil || !bytes.Equal(b, reportJSON(1, s.body)) {
		t.Fatalf("a report's content: %v", err)
	}
	other := "https://src.example/api/v1/tenants/other"
	for name, c := range map[string]struct{ id, issuer string }{
		"a kind the export feed does not take": {"s1", srcURL},
		"no such document":                     {"nope", srcURL},
		"an issuer not handed on":              {"r1", other},
		"the tenant's own route":               {"r1", ""},
	} {
		if _, err := content(downstream, c.id, c.issuer); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := content(nobody, "r1", srcURL); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("content without feed: %v", err)
	}
	if _, err := content(downstream, "r1", "https://SRC.example/api/v1/tenants/newer"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("content for an issuer not in its normal form: %v", err)
	}

	// scope and equal answers: a body out of scope answers as one without documents; an issuer the
	// tenant does not hand on (none, or a feed without export) answers as no issuer at all would
	text := func(a attachment.FeedAnswer, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(a)
		return string(b)
	}
	none := text(ask(downstream, srcURL, attachment.FeedBody{BodyHash: s.body, AfterSeq: 99}))
	if out := text(ask(downstream, srcURL, attachment.FeedBody{BodyHash: strings.Repeat("e", 64)})); out != none || !strings.Contains(none, `"statements":[]`) ||
		!strings.Contains(none, `"keys":`) {
		t.Fatalf("a body out of scope:\n%s\n%s", out, none)
	}
	s.add("plain", func(sp *attachment.FeedSpec) { sp.URL = other })
	const empty = `{"statements":[],"bodies":[],"more":false}`
	for name, issuer := range map[string]string{"no feed of it": "https://elsewhere.example/api/v1/tenants/x", "a feed without export": other} {
		if out := text(ask(downstream, issuer, both...)); out != empty {
			t.Errorf("%s: %s", name, out)
		}
	}
	for name, issuer := range map[string]string{"not https": "http://src.example/api/v1/tenants/newer", "not normalised": "https://src.example:443/api/v1/tenants/newer",
		"no tenant path": "https://src.example/", "a query": srcURL + "?x=1"} {
		if _, err := ask(downstream, issuer, both...); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("issuer %s: %v", name, err)
		}
	}
	if _, err := ask(nobody, srcURL, both...); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("without feed: %v", err)
	}
	// the tenant's own statements are never answered to a request that names an issuer, nor the
	// issuer's to one that names none
	if out := text(ask(downstream, "", both...)); out != empty {
		t.Errorf("the tenant's own feed: %s", out)
	}

	// a withdrawal goes without a note, and its document's content is gone; a document removed at
	// the relay goes without content
	src.offer(s.other, "r2", nil, withdraw, nil) // 6
	s.run("src")
	if err := s.svc.Remove(ctx, admin, "acme", pkgtype.Of("tresor"), s.docs()["r1"].ID); err != nil {
		t.Fatal(err)
	}
	ans, _ = ask(downstream, srcURL, both...)
	notes = map[string]string{}
	for _, w := range ans.Statements {
		st := decoded(t, w)
		notes[fmt.Sprintf("%s/%d", st.IssuerID, st.Seq)] = w.Note
	}
	if n, ok := notes["r2/6"]; !ok || n != "" || notes["r1/1"] != attachment.NoteNotHeld {
		t.Fatalf("a withdrawal and a removal: %v", notes)
	}
	for _, id := range []string{"r1", "r2"} {
		if _, err := content(downstream, id, srcURL); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("content of %s: %v", id, err)
		}
	}

	// verified before it is sent: stored bytes altered at the relay end the body's answer below them
	p1 := s.docs()["p1"]
	if err := store.ExecRaw(ctx, s.st, "UPDATE attachments SET statement = ? WHERE id = ?", strings.Replace(p1.Statement, `"title":"Report"`, `"title":"Retitled"`, 1), p1.ID); err != nil {
		t.Fatal(err)
	}
	ans, _ = ask(downstream, srcURL, attachment.FeedBody{BodyHash: s.body})
	if len(ans.Statements) != 1 || decoded(t, ans.Statements[0]).Seq != 1 || len(ans.Bodies) != 1 || ans.Bodies[0].Seq != 1 {
		t.Fatalf("after an alteration at the relay: %+v", ans)
	}
}

// A relay's answer holds at most attachments.feed_answer_max bytes, the keys file included, then
// more; a via feed pages through it.
func TestRelayPaging(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.svc.FeedAnswerMax = attachment.MinFeedAnswer
	pad := func(m map[string]any) { m["pad"] = strings.Repeat("x", 9000) }
	const n = 30
	for i := range n {
		a.src.offer([]string{a.body, a.other}[i%2], fmt.Sprintf("r%02d", i), []byte(reviewDoc), nil, pad)
	}
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	if res := a.run("src"); res.Error != "" || res.Counts["received"] != n {
		t.Fatalf("the relay's own run: %+v", res)
	}
	ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}, {BodyHash: a.other}}, srcURL)
	raw, _ := json.Marshal(ans)
	if err != nil || !ans.More || len(ans.Statements) == 0 || len(ans.Statements) >= n || len(raw) > attachment.MinFeedAnswer || len(raw) < attachment.MinFeedAnswer/2 {
		t.Fatalf("the first page: %d statements, %d bytes, more %v, %v", len(ans.Statements), len(raw), ans.More, err)
	}
	// a via feed takes every page
	b := newSubscriber(t)
	b.feeds.Fetch = &throughRelay{t: t, relay: a.svc}
	b.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Keys = []string{a.src.fp} })
	if res := b.run("own"); res.Error != "" || res.Counts["received"] != n || len(b.docs()) != n {
		t.Fatalf("the via feed's run: %+v", res)
	}
}

// throughRelay is the network between an own kista and the relaying tenant acme of another store,
// without one: it calls the relay's feed service as the holder of feed.
type throughRelay struct {
	t      *testing.T
	relay  *attachment.Service
	denied bool // the relaying tenant is suspended, or the grant is gone: a missing route
}

func (n *throughRelay) Get(_ context.Context, raw string) ([]byte, http.Header, error) {
	n.t.Errorf("an own kista fetched %s", raw)
	return nil, nil, egress.ErrNotFound
}

func (n *throughRelay) PostJSON(_ context.Context, raw string, _ http.Header, body []byte, _ int64) ([]byte, error) {
	var req struct {
		Bodies []attachment.FeedBody `json:"bodies"`
		Issuer string                `json:"issuer"`
	}
	if err := json.Unmarshal(body, &req); err != nil || raw != relayURL+"/feed" {
		n.t.Errorf("an own kista posted to %s: %v", raw, err)
	}
	if n.denied {
		return nil, &egress.StatusError{Status: http.StatusNotFound, Host: "relay.example"}
	}
	ans, err := n.relay.Feed(ctx, downstream, "acme", req.Bodies, req.Issuer)
	if err != nil {
		return nil, err
	}
	return json.Marshal(ans)
}

func (n *throughRelay) Download(_ context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, relayURL+"/feed/documents/") {
		n.t.Errorf("an own kista downloads from %s", raw)
		return egress.Download{}, egress.ErrNotFound
	}
	var size int64
	err = n.relay.FeedDocument(ctx, downstream, "acme", u.Path[strings.LastIndex(u.Path, "/")+1:], u.Query().Get("issuer"), func(_ store.Attachment, b []byte) error {
		if int64(len(b)) > o.MaxBytes {
			return egress.ErrTooLarge
		}
		size = int64(len(b))
		_, err := w.Write(b)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		err = egress.ErrNotFound
	}
	return egress.Download{Size: size}, err
}

func (n *throughRelay) PortAllowed(uint16) bool { return true }

// A chain (spec 0045): a stand-in issuer, the relaying tenant A that takes its feed with export, and
// an own kista B that takes A's feed for the issuer and never contacts the issuer.
func TestRelayChain(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	issuer := a.src
	a.add("issuer", func(sp *attachment.FeedSpec) { sp.Export = true })
	b := newSubscriber(t)
	net := &throughRelay{t: t, relay: a.svc}
	b.feeds.Fetch = net // any request to the issuer's host fails the test
	fb := b.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Keys, sp.Export = []string{issuer.fp}, true })
	both := func() (attachment.RunResult, attachment.RunResult) {
		t.Helper()
		ra := a.run("issuer")
		if ra.Error != "" {
			t.Fatalf("A's run: %+v", ra)
		}
		return ra, b.run("own")
	}

	// a report reaches A, and B through A
	one := issuer.offer(a.body, "p1", reportJSON(1, a.body, a.other), asReport, nil)
	if _, rb := both(); rb.Error != "" || rb.Counts["received"] != 1 {
		t.Fatalf("B's run: %+v", rb)
	}
	atA, atB := a.docs()["p1"], b.docs()["p1"]
	if atB.Kind != store.KindReport || atB.Issuer != srcURL || atB.KeyFP != issuer.fp || atB.Statement != atA.Statement || atB.Signature != atA.Signature ||
		atB.ContentHash != atA.ContentHash || atB.Visibility != store.VisibleAdministrators || atB.Seq != one || !strings.Contains(atB.Summary, `"read":true`) {
		t.Fatalf("the report at B: %+v", atB)
	}

	// a correction supersedes: one head at B
	issuer.offer(a.body, "p2", reportJSON(2, a.body, a.other), func(st *attachment.Statement) { asReport(st); st.Supersedes = "p1" }, nil)
	if _, rb := both(); rb.Counts["received"] != 1 || rb.Counts["superseded"] != 1 {
		t.Fatalf("the correction at B: %+v", rb)
	}
	tn, _ := b.st.GetTenant(ctx, "acme")
	all, _ := b.st.BodyAttachments(ctx, tn.ID, b.body)
	if heads := attachment.Heads(all); len(all) != 2 || len(heads) != 1 || heads[0].IssuerID != "p2" || !strings.Contains(heads[0].Summary, `"issue":2`) {
		t.Fatalf("the heads at B: %+v", heads)
	}

	// B hands on in turn: a chain has any length
	relayOf(b)
	if got := b.feed("own"); got.KeysFileAt.IsZero() {
		t.Fatal("B kept no keys file")
	}
	onward, err := b.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: b.body}}, srcURL)
	if err != nil || len(onward.Statements) != 2 || len(onward.Keys) == 0 || onward.Statements[1].Note != "" {
		t.Fatalf("B's own relay: %+v %v", onward, err)
	}

	// A's stored bytes altered: nothing of it reaches B
	issuer.offer(a.other, "p3", reportJSON(1, a.other), asReport, nil)
	if ra := a.run("issuer"); ra.Counts["received"] != 1 {
		t.Fatalf("A's run: %+v", ra)
	}
	p3 := a.docs()["p3"]
	if err := store.ExecRaw(ctx, a.st, "UPDATE attachments SET statement = ? WHERE id = ?", strings.Replace(p3.Statement, a.other, a.body, 1), p3.ID); err != nil {
		t.Fatal(err)
	}
	if rb := b.run("own"); rb.Counts["received"] != 0 || len(b.docs()) != 2 || b.cursor(fb, b.other) != 0 {
		t.Fatalf("B took altered bytes: %+v", rb)
	}
	if err := store.ExecRaw(ctx, a.st, "UPDATE attachments SET statement = ? WHERE id = ?", p3.Statement, p3.ID); err != nil {
		t.Fatal(err)
	}
	if rb := b.run("own"); rb.Counts["received"] != 1 {
		t.Fatalf("B's run once A's bytes are the issuer's again: %+v", rb)
	}

	// the issuer rotates its key: A pins the new one and goes on; B is failing, keeps what it holds,
	// and proceeds once the fingerprint taken from the issuer is pinned
	old := issuer.fp
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer.key, issuer.fp = k, extfile.Fingerprint(&k.PublicKey)
	pins := []string{old, issuer.fp}
	if _, err := a.feeds.Change(ctx, admin, "acme", "issuer", attachment.FeedChange{Keys: &pins}, 0); err != nil {
		t.Fatal(err)
	}
	issuer.offer(a.other, "p4", reportJSON(2, a.other), func(st *attachment.Statement) { asReport(st); st.Supersedes = "p3" }, nil)
	_, rb := both()
	if got := b.feed("own"); rb.Counts["received"] != 0 || got.Status != store.FeedFailing || len(b.docs()) != 3 ||
		!strings.Contains(rb.Error, "take the issuer's current fingerprint from the issuer") || strings.Contains(rb.Error, strings.TrimPrefix(issuer.fp, "sha256:")) {
		t.Fatalf("B after the rotation: %+v, %s", rb, got.Status)
	}
	if _, err := b.feeds.Change(ctx, admin, "acme", "own", attachment.FeedChange{Keys: &pins}, 0); err != nil {
		t.Fatal(err)
	}
	if rb := b.run("own"); rb.Error != "" || rb.Counts["received"] != 1 || b.feed("own").Status != store.FeedOK || b.docs()["p4"].KeyFP != issuer.fp {
		t.Fatalf("B once the new key is pinned: %+v", rb)
	}

	// A suspended (its feed routes answer as a missing route): B is unauthorized, and keeps its documents
	net.denied = true
	if rb := b.run("own"); b.feed("own").Status != store.FeedUnauthorized || len(b.docs()) != 4 {
		t.Fatalf("B while A is suspended: %+v, %s", rb, b.feed("own").Status)
	}
}

// A bundle import holds at the document quota too (spec 0045): the body's cursor stops at what was
// taken, the target ends failing (limits), and a later bundle brings the rest.
func TestBundleQuotaHolds(t *testing.T) {
	s := newSubscriber(t)
	was := store.MaxTenantAttachments
	defer func() { store.MaxTenantAttachments = was }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil) // 1
	s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil) // 2: over the quota
	s.src.offer(s.body, "r3", []byte(reviewDoc), nil, nil) // 3: behind it
	fi, fd := s.bundleImport("air", 0, 3, false, nil)
	res, err := fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() == nil || res.Counts["received"] != 1 || res.Counts["held"] != 1 || res.Counts["refused"] != 0 || res.Error == "" {
		t.Fatalf("the import at the quota: %+v %v %v", res, err, fi.Failed())
	}
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); cur[s.body] != 1 || s.feed("air").Status != store.FeedFailing {
		t.Fatalf("the cursor: %v, %s", cur, s.feed("air").Status)
	}
	store.MaxTenantAttachments = was
	fi, _ = s.bundleImport("air", 1, 3, false, nil)
	res, err = fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() != nil || res.Counts["received"] != 2 || len(s.docs()) != 3 {
		t.Fatalf("the import with room: %+v %v %v", res, err, fi.Failed())
	}
}
