package attachment_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0046 at the feed: what a kista does with statements of a source newer than it.

const srcURL = "https://src.example/api/v1/tenants/newer"

const reviewDoc = `{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest",` +
	`"capabilities":[],"network":[],"files":[],"processes":[],"licences":["MIT"],"findings":[],"text":"# Fine"}`

type rsaSigner struct{ k *rsa.PrivateKey }

func (s rsaSigner) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.k, crypto.SHA256, h[:])
}
func (s rsaSigner) Public() *rsa.PublicKey { return &s.k.PublicKey }
func (s rsaSigner) ID() string             { return "test" }

// newerSource stands in for a source newer than this kista: it serves its keys file, answers the
// feed route by the request's cursors, and hands out documents, with no network. It is the feed
// client's Fetcher.
type newerSource struct {
	t     *testing.T
	key   *rsa.PrivateKey
	fp    string
	seq   int64
	stmts []sourceStatement
	docs  map[string][]byte // by issuer id
	down  map[string]bool   // documents that fail to download for now
	// onKeys runs as the keys file is fetched: a run's first request
	onKeys func()

	// as a relay (spec 0045): via is the relaying tenant's URL; set, the stand-in answers only a
	// request there that names the issuer, with the issuer's keys file in each answer, and fails the
	// test when the issuer's own URL is contacted
	via       string
	notes     map[int64]string // seq: the relay's note on a statement it sends without content
	relayKeys []byte           // the keys member, instead of the keys file (nil: the keys file)
	noKeys    bool             // the answer carries no keys
	highWater map[string]int64 // body: an unsigned high-water beyond the statements sent
	posts     int
}

// spoil breaks a statement's signature.
func (s *newerSource) spoil(seq int64) {
	for i := range s.stmts {
		if s.stmts[i].seq == seq {
			s.stmts[i].sig = make([]byte, len(s.stmts[i].sig))
		}
	}
}

// wire is the source's statements of a body numbered in (after, upTo], as a bundle carries them.
func (s *newerSource) wire(body string, after, upTo int64) []attachment.BundleStatement {
	var out []attachment.BundleStatement
	for _, st := range s.stmts {
		if st.body == body && st.seq > after && st.seq <= upTo {
			out = append(out, attachment.BundleStatement{Statement: st.stmt, Signature: st.sig})
		}
	}
	return out
}

func (s *newerSource) keysFile() []byte {
	pemStr, _ := extfile.MarshalPublicKeyPEM(&s.key.PublicKey)
	b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": s.fp, "public_key": pemStr}}, "since": "a later release's member"})
	return b
}

type sourceStatement struct {
	seq       int64
	body      string
	stmt, sig []byte
	hidden    bool // not offered for now
}

// hide stops, or starts again, offering a statement.
func (s *newerSource) hide(seq int64, hidden bool) {
	for i := range s.stmts {
		if s.stmts[i].seq == seq {
			s.stmts[i].hidden = hidden
		}
	}
}

func newSource(t *testing.T) *newerSource {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &newerSource{t: t, key: k, fp: extfile.Fingerprint(&k.PublicKey), docs: map[string][]byte{}}
}

// offer signs the next statement about a document of body: a review unless mod changes it. edit, if
// set, rewrites the canonical JSON before it is signed (members this binary's writer does not know).
func (s *newerSource) offer(body, id string, content []byte, mod func(*attachment.Statement), edit func(map[string]any)) int64 {
	s.t.Helper()
	s.seq++
	st := attachment.Statement{Issuer: srcURL, Key: s.fp, IssuerID: id, Seq: s.seq, BodyHash: body, Kind: store.KindReview,
		MediaType: attachment.Review, ContentHash: blob.ContentHash(content), Size: int64(len(content)), Title: "Review",
		IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "readers"}
	if mod != nil {
		mod(&st)
	}
	b, err := st.Canonical()
	if err != nil {
		s.t.Fatal(err)
	}
	if edit != nil {
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			s.t.Fatal(err)
		}
		edit(m)
		raw, _ := json.Marshal(m)
		if b, err = attachment.Canonicalize(raw); err != nil {
			s.t.Fatal(err)
		}
	}
	sig, err := attachment.Sign(context.Background(), rsaSigner{s.key}, b)
	if err != nil {
		s.t.Fatal(err)
	}
	s.stmts = append(s.stmts, sourceStatement{seq: s.seq, body: body, stmt: b, sig: sig})
	if content != nil {
		s.docs[id] = content
	}
	return s.seq
}

func withdraw(st *attachment.Statement) {
	st.Withdrawn, st.Kind, st.MediaType, st.ContentHash, st.Size, st.Title, st.Origin, st.Audience = true, "", "", "", 0, "", "", ""
}

func v2(m map[string]any) { m["v"] = 2 }

func (s *newerSource) Get(_ context.Context, raw string) ([]byte, http.Header, error) {
	if s.via != "" {
		s.t.Errorf("a via feed fetched %s: it reads no keys route, and never contacts its issuer", raw)
		return nil, nil, egress.ErrNotFound
	}
	if !strings.HasSuffix(raw, "/attestation-keys") {
		return nil, nil, egress.ErrNotFound
	}
	if s.onKeys != nil {
		s.onKeys()
	}
	return s.keysFile(), nil, nil
}

func (s *newerSource) PostJSON(_ context.Context, raw string, _ http.Header, body []byte, _ int64) ([]byte, error) {
	var req struct {
		Bodies []attachment.FeedBody `json:"bodies"`
		Issuer string                `json:"issuer"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	s.posts++
	ans := attachment.FeedAnswer{Statements: []attachment.WireStatement{}, Bodies: []attachment.HighWater{}}
	if s.via != "" {
		if raw != s.via+"/feed" || req.Issuer != srcURL {
			s.t.Errorf("a via feed posted to %s naming the issuer %q", raw, req.Issuer)
		}
		if !s.noKeys {
			if ans.Keys = s.relayKeys; ans.Keys == nil {
				ans.Keys = s.keysFile()
			}
		}
	} else if req.Issuer != "" || !strings.HasPrefix(raw, srcURL) {
		s.t.Errorf("a feed without via posted to %s naming the issuer %q", raw, req.Issuer)
	}
	for _, b := range req.Bodies {
		high := b.AfterSeq
		for _, st := range s.stmts {
			if st.body != b.BodyHash || st.seq <= b.AfterSeq || st.hidden {
				continue
			}
			ans.Statements = append(ans.Statements, attachment.WireStatement{Statement: base64.StdEncoding.EncodeToString(st.stmt),
				Signature: base64.StdEncoding.EncodeToString(st.sig), Note: s.notes[st.seq]})
			high = st.seq
		}
		high = max(high, s.highWater[b.BodyHash])
		if high > b.AfterSeq {
			ans.Bodies = append(ans.Bodies, attachment.HighWater{BodyHash: b.BodyHash, Seq: high})
		}
	}
	return json.Marshal(ans)
}

func (s *newerSource) Download(_ context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error) {
	if s.via != "" {
		var ok bool
		if raw, ok = strings.CutSuffix(raw, "?issuer="+url.QueryEscape(srcURL)); !ok || !strings.HasPrefix(raw, s.via+"/feed/documents/") {
			s.t.Errorf("a via feed downloads from %s", raw)
		}
	} else if !strings.HasPrefix(raw, srcURL+"/feed/documents/") {
		s.t.Errorf("a feed without via downloads from %s", raw)
	}
	doc, ok := s.docs[raw[strings.LastIndex(raw, "/")+1:]]
	switch {
	case !ok:
		return egress.Download{}, egress.ErrNotFound
	case s.down[raw[strings.LastIndex(raw, "/")+1:]]:
		return egress.Download{}, errors.New("the source is busy")
	case int64(len(doc)) > o.MaxBytes:
		return egress.Download{}, egress.ErrTooLarge
	}
	_, err := w.Write(doc)
	return egress.Download{Size: int64(len(doc))}, err
}

func (s *newerSource) PortAllowed(uint16) bool { return true }

// subscriber is the test tenant's feed client over a source.
type subscriber struct {
	*env
	t     *testing.T
	feeds *attachment.Feeds
	src   *newerSource
}

func newSubscriber(t *testing.T) *subscriber {
	t.Helper()
	e := newEnv(t)
	src := newSource(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("T\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "src", Tenants: []string{"acme"}, Prefixes: []string{"https://src.example/"},
		Kind: "token_file", TokenFile: token}, {Name: "relay", Tenants: []string{"acme"}, Prefixes: []string{"https://relay.example/"},
		Kind: "token_file", TokenFile: token}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	return &subscriber{env: e, t: t, src: src, feeds: &attachment.Feeds{Store: e.st, Authz: e.svc.Authz, Attachments: e.svc, Fetch: src,
		Credentials: creds, Log: slog.New(slog.DiscardHandler)}}
}

func (s *subscriber) add(name string, mod func(*attachment.FeedSpec)) store.Feed {
	s.t.Helper()
	sp := attachment.FeedSpec{Name: name, URL: srcURL, Credential: "src", Keys: []string{s.src.fp}, Readers: true,
		Offer: store.OfferChannels, Channels: []string{"prod"}}
	if mod != nil {
		mod(&sp)
	}
	f, err := s.feeds.Add(ctx, admin, "acme", sp)
	if err != nil {
		s.t.Fatal(err)
	}
	return f
}

func (s *subscriber) run(name string) attachment.RunResult {
	s.t.Helper()
	f, err := s.feeds.Sync(ctx, admin, "acme", name)
	if err != nil {
		s.t.Fatal(err)
	}
	res, err := s.feeds.RunFeed(ctx, f.ID, func(context.Context) error { return nil })
	if err != nil {
		s.t.Fatal(err)
	}
	return res
}

func (s *subscriber) feed(name string) store.Feed {
	s.t.Helper()
	f, err := s.feeds.Get(ctx, admin, "acme", name)
	if err != nil {
		s.t.Fatal(err)
	}
	return f
}

// docs are the tenant's documents by issuer id, tombstones left out.
func (s *subscriber) docs() map[string]store.Attachment {
	s.t.Helper()
	tn, _ := s.st.GetTenant(ctx, "acme")
	out := map[string]store.Attachment{}
	for _, body := range []string{s.body, s.other} {
		as, err := s.st.BodyAttachments(ctx, tn.ID, body)
		if err != nil {
			s.t.Fatal(err)
		}
		for _, a := range as {
			if a.RemovedAt.IsZero() {
				out[a.IssuerID] = a
			}
		}
	}
	return out
}

func reasons(res attachment.RunResult) map[string]int {
	out := map[string]int{}
	for _, r := range res.Refused {
		out[r.Reason]++
	}
	return out
}

// A statement this kista cannot read waits between two it can: both are stored, the run is ok, the
// cursor is past all three, and the wait is counted for the administrators with its reason. Each
// way a statement may be unreadable waits under its own word; a withdrawal in a newer format is
// applied; an authentic statement with a member out of its bounds is refused for good, not held.
func TestFeedWaits(t *testing.T) {
	s := newSubscriber(t)
	src := s.src
	doc := []byte(reviewDoc)
	src.offer(s.body, "first", doc, nil, nil)
	src.offer(s.body, "report", []byte(`{"envelope":1}`), func(st *attachment.Statement) {
		st.Kind, st.MediaType = "audit", "application/vnd.kista.audit.v1+json"
	}, nil)
	src.offer(s.body, "last", doc, nil, nil)
	f := s.add("src", nil)
	res := s.run("src")
	if res.Error != "" || res.Counts["received"] != 2 || res.Counts["waiting"] != 1 || res.Counts["refused"] != 0 || len(res.Refused) != 0 {
		t.Fatalf("the run: %+v", res)
	}
	docs := s.docs()
	if len(docs) != 2 || docs["first"].Visibility != store.VisibleReaders || docs["last"].Seq != 3 {
		t.Fatalf("stored: %+v", docs)
	}
	got := s.feed("src")
	if got.Status != store.FeedOK || got.Waiting[attachment.WaitKind] != 1 || len(got.Waiting) != 1 {
		t.Fatalf("the feed: status %s, waiting %v", got.Status, got.Waiting)
	}
	if line := attachment.OlderThanSource(got.Waiting); line != "This kista is older than its source: documents wait for an upgrade (`unknown kind`)." {
		t.Fatalf("the administrators' line: %q", line)
	}
	if cur, _ := s.st.FeedCursors(ctx, f.ID); cur[s.body] != 3 {
		t.Fatalf("the cursor: %v", cur)
	}
	w, _ := s.st.FeedWaits(ctx, f.ID)
	if x := w[s.body]; x.From != 2 || x.Count != 1 || x.Why != attachment.WaitKind || x.Level != attachment.ReaderLevel {
		t.Fatalf("the waiting state: %+v", w)
	}
	// nothing more arrives: the next run asks from the cursor and takes nothing again
	if res := s.run("src"); res.Counts["received"] != 0 || res.Counts["waiting"] != 0 || res.Error != "" {
		t.Fatalf("the next run: %+v", res)
	}

	// each way of being unreadable, on the other body
	src.offer(s.other, "newer", doc, nil, v2) // seq 4: a newer format
	src.offer(s.other, "media", doc, func(st *attachment.Statement) { st.MediaType = "application/vnd.kista.review.v2+json" }, nil)
	cdx2 := []byte(`{"bomFormat":"CycloneDX","specVersion":"2.0","components":[]}`)
	src.offer(s.other, "sbom2", cdx2, func(st *attachment.Statement) { st.Kind, st.MediaType = store.KindSBOM, attachment.CycloneDX }, nil)
	// seq 7: authentic, a known member of the wrong type: refused for good, and the body goes on
	src.offer(s.other, "odd", doc, nil, func(m map[string]any) { m["size"] = "big" })
	// seq 8: an extra member, an audience and an origin of a later release: stored, for administrators
	src.offer(s.other, "extra", doc, func(st *attachment.Statement) { st.Audience, st.Origin = "partners", "distributor:acme" },
		func(m map[string]any) { m["partners"] = []any{"x", map[string]any{"tier": 2}} })
	// seq 9: a review with a member a later analyser added: read at the feed, its members ignored
	later := []byte(strings.Replace(reviewDoc, `"text":`, `"confidence":"high","text":`, 1))
	src.offer(s.other, "later", later, nil, nil)
	res = s.run("src")
	if res.Error != "" || res.Counts["waiting"] != 3 || res.Counts["received"] != 2 || reasons(res)["statement"] != 1 || res.Counts["refused"] != 1 {
		t.Fatalf("the second run: %+v", res)
	}
	docs = s.docs()
	if x := docs["extra"]; x.Visibility != store.VisibleAdministrators || x.SourceOrigin != "distributor:acme" {
		t.Fatalf("a later release's audience and origin: %+v", x)
	}
	if _, ok := docs["later"]; !ok || len(docs) != 4 {
		t.Fatalf("stored: %v", docs)
	}
	got = s.feed("src")
	if got.Status != store.FeedOK || got.Waiting[attachment.WaitDocument] != 3 || got.Waiting[attachment.WaitKind] != 1 {
		// the reason shown for a body is its last one: three waits on the other body, the last for its document's version
		t.Fatalf("the feed after the second run: status %s, waiting %v", got.Status, got.Waiting)
	}
	w, _ = s.st.FeedWaits(ctx, f.ID)
	if x := w[s.other]; x.From != 4 || x.Count != 3 || x.Level != attachment.ReaderLevel {
		t.Fatalf("the other body's waiting state: %+v", x)
	}
	if cur, _ := s.st.FeedCursors(ctx, f.ID); cur[s.other] != 9 {
		t.Fatalf("the cursors: %v", cur)
	}

	// the attach route stays strict: the same review is refused there
	if _, err := s.svc.Attach(ctx, admin, "acme", pkgtype.Of("tresor"), attachment.AttachInput{BodyHash: s.body, Kind: store.KindReview,
		MediaType: attachment.Review, Title: "Review", Content: later}); !errors.Is(err, attachment.ErrDocument) {
		t.Fatalf("a review with an unknown member attached here: %v", err)
	}
	// and a document version it does not read is refused there, not left waiting
	if _, err := s.svc.Attach(ctx, admin, "acme", pkgtype.Of("tresor"), attachment.AttachInput{BodyHash: s.body, Kind: store.KindSBOM,
		MediaType: attachment.CycloneDX, Title: "SBOM", Content: cdx2}); !errors.Is(err, attachment.ErrDocumentVersion) {
		t.Fatalf("CycloneDX 2.0 attached here: %v", err)
	}

	// a withdrawal in a newer format is applied: it can only remove
	src.offer(s.body, "first", nil, withdraw, v2)
	if res := s.run("src"); res.Counts["withdrawn"] != 1 || res.Counts["waiting"] != 0 {
		t.Fatalf("a v2 withdrawal: %+v", res)
	}
	if x := s.docs()["first"]; x.WithdrawnAt.IsZero() {
		t.Fatalf("the document is not withdrawn: %+v", x)
	}
}

// The feed's own kind filter comes before waiting: a kind the administrator left out is refused
// for good, known to this binary or not.
func TestFeedFilterBeforeWaiting(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "report", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil)
	s.src.offer(s.body, "review", []byte(reviewDoc), nil, nil)
	f := s.add("src", func(sp *attachment.FeedSpec) { sp.Kinds = []string{store.KindReview} })
	res := s.run("src")
	if res.Counts["waiting"] != 0 || reasons(res)["kind"] != 1 || res.Counts["received"] != 1 {
		t.Fatalf("the run: %+v", res)
	}
	if w, _ := s.st.FeedWaits(ctx, f.ID); len(w) != 0 {
		t.Fatalf("a filtered kind waits: %+v", w)
	}
	// the filter stays a closed vocabulary: a typo, or a kind of a later release, fails
	kinds := []string{"audit"}
	if _, err := s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Kinds: &kinds}, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a feed's kinds take a kind this binary does not know: %v", err)
	}
}

// A statement in a newer format about a document already held waits; it narrows the document to
// administrators first only when its audience is not readers, its seq is higher and its body is the
// document's. The document is never widened back.
func TestFeedNewerAudience(t *testing.T) {
	s := newSubscriber(t)
	src := s.src
	doc := []byte(reviewDoc)
	admins := func(st *attachment.Statement) { st.Audience = "administrators" }
	src.offer(s.body, "a", doc, nil, nil)    // 1
	src.offer(s.body, "b", doc, nil, nil)    // 2
	src.offer(s.body, "c", doc, nil, nil)    // 3
	src.offer(s.body, "a", doc, admins, v2)  // 4: not for readers, newer, the same body: narrows
	src.offer(s.body, "b", doc, nil, v2)     // 5: for readers still: nothing
	src.offer(s.other, "c", doc, admins, v2) // 6: another body than the document's: nothing
	s.add("src", nil)
	res := s.run("src")
	if res.Counts["received"] != 3 || res.Counts["waiting"] != 3 || res.Counts["narrowed"] != 1 || res.Error != "" {
		t.Fatalf("the run: %+v", res)
	}
	docs := s.docs()
	for id, want := range map[string]string{"a": store.VisibleAdministrators, "b": store.VisibleReaders, "c": store.VisibleReaders} {
		if docs[id].Visibility != want {
			t.Errorf("document %s is for %s, want %s", id, docs[id].Visibility, want)
		}
	}
	if docs["a"].Seq != 1 { // nothing of the newer statement is stored
		t.Errorf("the narrowed document's statement: seq %d", docs["a"].Seq)
	}

	// a seq that is not higher: document d is held at seq 8 by this feed (statement 7 was not
	// offered then); a second feed of the source, reading from the start, meets 7 about it
	seven := src.offer(s.body, "d", doc, admins, v2) // 7
	src.hide(seven, true)
	src.offer(s.body, "d", doc, nil, nil) // 8
	if res := s.run("src"); res.Counts["received"] != 1 {
		t.Fatalf("document d: %+v", res)
	}
	src.hide(seven, false)
	s.add("again", nil)
	res = s.run("again")
	if res.Counts["waiting"] != 4 || res.Counts["received"] != 0 || res.Error != "" {
		t.Fatalf("the second feed's run: %+v", res)
	}
	docs = s.docs()
	if docs["d"].Visibility != store.VisibleReaders {
		t.Errorf("an older statement in a newer format narrowed document d")
	}
	if docs["a"].Visibility != store.VisibleAdministrators { // statement 1, replayed by the second feed, widens nothing
		t.Errorf("document a was widened back")
	}
	if res.Counts["narrowed"] != 0 { // statement 4 again, at the second feed: narrowed once
		t.Errorf("a replay narrowed again: %+v", res.Counts)
	}

	// the narrowing holds: the document's audience is administrators whatever its stored statement
	// says, and an administrator does not widen it, as one never widens a document its source keeps
	// to administrators
	readers := store.VisibleReaders
	a := docs["a"]
	if !a.Narrowed || attachment.Audience(a) != store.VisibleAdministrators || attachment.Audience(docs["b"]) != store.VisibleReaders {
		t.Fatalf("the narrowed document: narrowed %v, audience %q", a.Narrowed, attachment.Audience(a))
	}
	if _, err := s.svc.SetFlags(ctx, admin, "acme", pkgtype.Of("tresor"), a.ID, &readers, nil, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("an administrator widened a narrowed document: %v", err)
	}
	// a statement this kista reads, newer than the document's, says who may see it again: the
	// mark goes, the visibility stays the administrators' to widen
	src.offer(s.body, "a", doc, nil, nil) // 9, for readers
	if res := s.run("src"); res.Counts["updated"] != 1 {
		t.Fatalf("a readable newer statement: %+v", res)
	}
	a = s.docs()["a"]
	if a.Narrowed || a.Seq != 9 || attachment.Audience(a) != store.VisibleReaders || a.Visibility != store.VisibleAdministrators {
		t.Fatalf("after a readable newer statement: %+v", a)
	}
	if got, err := s.svc.SetFlags(ctx, admin, "acme", pkgtype.Of("tresor"), a.ID, &readers, nil, 0); err != nil || got.Visibility != store.VisibleReaders {
		t.Fatalf("widening it then: %+v %v", got, err)
	}
}

// Retake: a binary of a higher reader level takes again, with no reset, what a lower one passed
// over, and judges the rewound range again (an earlier size refusal too); the same or a lower
// level retakes nothing; a second wait keeps the row's lower level; a reset deletes the waiting
// state with the cursors; an export feed's kept rows of the rewound range go, and what still waits
// is kept again.
func TestFeedRetake(t *testing.T) {
	s := newSubscriber(t)
	src := s.src
	cdx2 := []byte(`{"bomFormat":"CycloneDX","specVersion":"2.0","components":[{"type":"library"}]}`)
	sbom := func(st *attachment.Statement) { st.Kind, st.MediaType = store.KindSBOM, attachment.CycloneDX }
	big := []byte(strings.Replace(reviewDoc, "# Fine", "# Fine"+strings.Repeat(" and long", 100), 1))
	s.svc.MaxSize = 512
	src.offer(s.body, "ok", []byte(reviewDoc), nil, nil)                                                                                      // 1
	src.offer(s.body, "sbom2", cdx2, sbom, nil)                                                                                               // 2: a document version this binary does not read
	src.offer(s.body, "big", big, nil, nil)                                                                                                   // 3: over the site's size
	src.offer(s.body, "report", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil) // 4
	f := s.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	res := s.run("src")
	if res.Counts["received"] != 1 || res.Counts["waiting"] != 2 || reasons(res)["size"] != 1 || res.Error != "" {
		t.Fatalf("the first run: %+v", res)
	}
	kept := func() map[int64]string {
		t.Helper()
		rs, err := s.st.FeedRefusals(ctx, f.ID, s.body, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, r := range rs {
			out[r.Seq] = r.Reason
		}
		return out
	}
	// the export feed keeps the waiting statements as it keeps the refusal, each under its reason
	if k := kept(); len(k) != 3 || k[2] != attachment.WaitDocument || k[3] != "size" || k[4] != attachment.WaitKind {
		t.Fatalf("kept for export: %v", k)
	}
	wait := func() store.FeedWait {
		t.Helper()
		w, err := s.st.FeedWaits(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		return w[s.body]
	}
	if w := wait(); w.From != 2 || w.Count != 2 || w.Level != attachment.ReaderLevel {
		t.Fatalf("the waiting state: %+v", w)
	}

	// the same binary again: nothing is taken again, though the site's size was raised meanwhile
	s.svc.MaxSize = 0
	if res := s.run("src"); res.Counts["received"] != 0 || res.Counts["waiting"] != 0 || wait().Count != 2 {
		t.Fatalf("a run at the same level: %+v", res)
	}
	if len(s.docs()) != 1 {
		t.Fatalf("stored before the upgrade: %v", s.docs())
	}

	// an upgraded binary: it reads CycloneDX 2.0 (a floor stands in for its reader) at a higher
	// level. With no reset the document is stored, the size refusal of the rewound range is judged
	// again and passes now, and the report waits again, at the new level
	defer attachment.DeclareFloor(store.KindSBOM, func(string, string, []byte) (map[string]any, error) {
		return map[string]any{"format": "cyclonedx 2.0"}, nil
	})()
	s.feeds.SetReaderLevel(attachment.ReaderLevel + 1)
	res = s.run("src")
	if res.Counts["received"] != 2 || res.Counts["waiting"] != 1 || res.Counts["refused"] != 0 || res.Error != "" {
		t.Fatalf("the upgraded run: %+v", res)
	}
	docs := s.docs()
	if len(docs) != 3 || docs["sbom2"].Summary != `{"format":"cyclonedx 2.0"}` || docs["big"].Size != int64(len(big)) || docs["ok"].Seq != 1 {
		t.Fatalf("stored after the upgrade: %+v", docs)
	}
	if w := wait(); w.From != 4 || w.Count != 1 || w.Why != attachment.WaitKind || w.Level != attachment.ReaderLevel+1 {
		t.Fatalf("the waiting state after the retake: %+v", w)
	}
	// the kept rows of the rewound range went with the retake; what still waits was kept again
	if k := kept(); len(k) != 1 || k[4] != attachment.WaitKind {
		t.Fatalf("kept for export after the retake: %v", k)
	}
	if cur, _ := s.st.FeedCursors(ctx, f.ID); cur[s.body] != 4 {
		t.Fatalf("the cursor: %v", cur)
	}

	// a still newer binary waits on a later statement: the row keeps the lower level, so the binary
	// between the two takes the range again
	s.feeds.SetReaderLevel(attachment.ReaderLevel + 3)
	src.offer(s.body, "newer", []byte(reviewDoc), nil, v2) // 5
	if res := s.run("src"); res.Counts["waiting"] != 2 {   // the retake of 4 (level 2 < 4), and 5
		t.Fatalf("a newer binary's run: %+v", res)
	}
	if w := wait(); w.From != 4 || w.Count != 2 || w.Level != attachment.ReaderLevel+3 {
		t.Fatalf("after a newer binary's retake: %+v", w)
	}
	// an older replica in the rolling upgrade retakes nothing of the newer one's row, and waits on
	// another statement meanwhile: the row keeps the lower level
	s.feeds.SetReaderLevel(attachment.ReaderLevel + 1)
	src.offer(s.body, "media", []byte(reviewDoc), func(st *attachment.Statement) { st.MediaType = "application/x-later" }, nil) // 6
	if res := s.run("src"); res.Counts["waiting"] != 1 {
		t.Fatalf("the older replica's run: %+v", res)
	}
	if w := wait(); w.From != 4 || w.Count != 3 || w.Level != attachment.ReaderLevel+1 || w.Why != attachment.WaitMedia {
		t.Fatalf("a second wait at a lower level: %+v", w)
	}

	// a reset (export switched off and on) deletes the waiting state with the cursors
	off, on := false, true
	if _, err := s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Export: &off}, 0); err != nil {
		t.Fatal(err)
	}
	if w := wait(); w.Count != 3 {
		t.Fatalf("switching export off reset the cursors: %+v", w)
	}
	if _, err := s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Export: &on}, 0); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.st.FeedWaits(ctx, f.ID); len(w) != 0 {
		t.Fatalf("a reset kept waiting state: %+v", w)
	}
	if got := s.feed("src"); len(got.Waiting) != 0 {
		t.Fatalf("the feed after a reset: %v", got.Waiting)
	}
	// the run after the reset reads the history again: stored documents are no-ops, the waits wait
	res = s.run("src")
	if res.Counts["received"] != 0 || res.Counts["waiting"] != 3 || res.Error != "" {
		t.Fatalf("the run after the reset: %+v", res)
	}
	if w := wait(); w.From != 4 || w.Count != 3 || w.Level != attachment.ReaderLevel+1 {
		t.Fatalf("the waiting state after the reset: %+v", w)
	}
}

// A kind may declare a floor for its document: a version this binary does not read is then stored
// by what the floor reads, not left waiting. No kind of this release declares one.
func TestKindFloor(t *testing.T) {
	cdx2 := []byte(`{"bomFormat":"CycloneDX","specVersion":"2.0"}`)
	if _, err := attachment.SummarizeReceived(store.KindSBOM, attachment.CycloneDX, "", cdx2); !errors.Is(err, attachment.ErrDocumentVersion) {
		t.Fatalf("a version not read: %v", err)
	}
	for doc, mt := range map[string]string{`{"spdxVersion":"SPDX-3.0"}`: attachment.SPDX, `{"version":"3.0.0","runs":[]}`: attachment.SARIF} {
		kind := store.KindSBOM
		if mt == attachment.SARIF {
			kind = store.KindVulnerabilities
		}
		if _, err := attachment.SummarizeReceived(kind, mt, "", []byte(doc)); !errors.Is(err, attachment.ErrDocumentVersion) {
			t.Errorf("%s: %v", doc, err)
		}
	}
	// what is not a document of its media type at all is refused, not a version to wait for
	for doc, mt := range map[string]string{`{"bomFormat":"Other","specVersion":"2.0"}`: attachment.CycloneDX, `{"name":"x"}`: attachment.SPDX,
		`{"runs":[]}`: attachment.SARIF} {
		kind := store.KindSBOM
		if mt == attachment.SARIF {
			kind = store.KindVulnerabilities
		}
		if _, err := attachment.SummarizeReceived(kind, mt, "", []byte(doc)); !errors.Is(err, attachment.ErrDocument) || errors.Is(err, attachment.ErrDocumentVersion) {
			t.Errorf("%s: %v", doc, err)
		}
	}
	undo := attachment.DeclareFloor(store.KindSBOM, func(mt, _ string, b []byte) (map[string]any, error) {
		return map[string]any{"format": "read by its floor", "bytes": len(b)}, nil
	})
	sum, err := attachment.SummarizeReceived(store.KindSBOM, attachment.CycloneDX, "", cdx2)
	if err != nil || !strings.Contains(sum, "read by its floor") {
		t.Fatalf("a declared floor: %q %v", sum, err)
	}
	// the floor is for what is received: an attach here stays refused; a version it reads is read as before
	if _, err := attachment.Summarize(store.KindSBOM, attachment.CycloneDX, "", cdx2); !errors.Is(err, attachment.ErrDocumentVersion) {
		t.Fatalf("a floor at the attach route: %v", err)
	}
	if sum, err := attachment.SummarizeReceived(store.KindSBOM, attachment.CycloneDX, "", []byte(sbom)); err != nil || !strings.Contains(sum, "cyclonedx 1.5") {
		t.Fatalf("a readable version beside a floor: %q %v", sum, err)
	}
	undo()
	if _, err := attachment.SummarizeReceived(store.KindSBOM, attachment.CycloneDX, "", cdx2); !errors.Is(err, attachment.ErrDocumentVersion) {
		t.Fatalf("after the floor is withdrawn: %v", err)
	}
}
