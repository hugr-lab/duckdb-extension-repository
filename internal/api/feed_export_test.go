package api_test

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/pem"
	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// What an export feed keeps for a site's plan (spec 0014 phase 1b): the source's keys file, its
// permanent refusals with their bytes, statements about documents it does not hold (a withdrawal of
// one never stored, a statement about a removed one), a kept withdrawal applied when the document
// arrives through another feed; setting export reads the history again, clearing it drops what was
// kept; past the quota a body's cursor holds and the run fails.
func TestFeedExport(t *testing.T) {
	key := mustKey(t)
	fp := extfile.Fingerprint(&key.PublicKey)
	content := []byte(review)
	cdx := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`)
	sboms := map[string]bool{"an-sbom": true, "late": true, "another-sbom": true}
	var wire []attachment.WireStatement
	keysFile := ""
	var onFeed func() // called as the source answers a feed request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/attestation-keys"):
			_, _ = w.Write([]byte(keysFile))
		case strings.HasSuffix(r.URL.Path, "/feed"):
			if onFeed != nil {
				onFeed()
			}
			_ = json.NewEncoder(w).Encode(attachment.FeedAnswer{Statements: wire, Bodies: []attachment.HighWater{}})
		case strings.Contains(r.URL.Path, "/feed/documents/"):
			if strings.HasSuffix(r.URL.Path, "/bad-content") {
				_, _ = w.Write([]byte(strings.Replace(review, "approved", "rejected", 1)))
				return
			}
			if sboms[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] {
				_, _ = w.Write(cdx)
				return
			}
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	der := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustDER(t, &key.PublicKey)})
	kf, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": fp, "public_key": string(der)}}})
	keysFile = string(kf)
	feedURL := srv.URL + "/api/v1/tenants/src"
	sub := newSubscriber(t, srv, "T")
	body := tresorBody(t, func(p string) map[string]any { return getJSON(t, sub.env, p) })
	tn, _ := sub.st.GetTenant(ctx, "acme")
	stmt := func(seq int64, id string, mod func(*attachment.Statement)) attachment.WireStatement {
		s := attachment.Statement{Issuer: feedURL, Key: fp, IssuerID: id, Seq: seq, BodyHash: body, Kind: store.KindReview,
			MediaType: attachment.Review, ContentHash: blob.ContentHash(content), Size: int64(len(content)), Title: "Review",
			Summary: json.RawMessage(`{"verdict":"approved"}`), IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "readers"}
		if mod != nil {
			mod(&s)
		}
		b, err := s.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := attachment.Sign(ctx, keySigner{key}, b)
		if err != nil {
			t.Fatal(err)
		}
		return attachment.WireStatement{Statement: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(sig)}
	}
	withdrawn := func(s *attachment.Statement) {
		s.Withdrawn, s.Kind, s.MediaType, s.ContentHash, s.Size, s.Title, s.Summary = true, "", "", "", 0, "", nil
	}
	sbom := func(s *attachment.Statement) {
		s.Kind, s.MediaType, s.ContentHash, s.Size, s.Summary = store.KindSBOM, attachment.CycloneDX, blob.ContentHash(cdx), int64(len(cdx)), nil
	}
	wire = []attachment.WireStatement{
		stmt(1, "kept-doc", nil),           // stored
		stmt(2, "an-sbom", sbom),           // refused: a kind not accepted
		stmt(3, "bad-content", nil),        // refused: content
		stmt(4, "never-stored", withdrawn), // a withdrawal of a document never stored: kept
		stmt(5, "late", sbom),              // refused by kind here...
		stmt(6, "late", withdrawn),         // ...then withdrawn: kept
	}
	add := func(name string, sp attachment.FeedSpec) store.Feed {
		t.Helper()
		sp.Name, sp.URL, sp.Keys, sp.Offer, sp.Channels = name, feedURL, []string{fp}, store.OfferChannels, []string{"prod"}
		if sp.Mode == "" {
			sp.Credential = "src"
		}
		f, err := sub.feeds.Add(ctx, admin, "acme", sp)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	reviews := add("reviews", attachment.FeedSpec{Kinds: []string{store.KindReview}, Export: true})
	if res := sub.run(t, "reviews"); res.Error != "" || res.Counts["received"] != 1 || res.Counts["kept"] != 2 {
		t.Fatalf("run: %+v", res)
	}
	var file []byte
	if err := sub.st.InTx(ctx, "", func(tx *store.Tx) (err error) { file, _, err = tx.FeedKeysFile(ctx, reviews.ID); return }); err != nil ||
		string(file) != keysFile {
		t.Fatalf("keys file: %q %v", file, err)
	}
	refused, err := sub.st.FeedRefusals(ctx, reviews.ID, body, 0, 100)
	if err != nil || len(refused) != 3 || refused[0].Reason != "kind" || refused[1].Reason != "content" || refused[2].Seq != 5 ||
		refused[0].Statement == "" || refused[0].Signature != wire[1].Signature {
		t.Fatalf("refusals: %+v %v", refused, err)
	}
	kept, err := sub.st.FeedKeptOf(ctx, tn.ID, feedURL, body, 0, 100)
	if err != nil || len(kept) != 2 || kept[0].IssuerID != "never-stored" || kept[1].IssuerID != "late" || kept[1].Seq != 6 {
		t.Fatalf("kept: %+v %v", kept, err)
	}

	// a second export feed accepting every kind stores "late", and the kept withdrawal applies to it
	add("all", attachment.FeedSpec{Export: true})
	if res := sub.run(t, "all"); res.Error != "" || res.Counts["received"] != 2 {
		t.Fatalf("all: %+v", res)
	}
	var late store.Attachment
	for _, d := range sub.docs(t, body) {
		if d.IssuerID == "late" {
			late = d
		}
	}
	if late.ID == "" || late.WithdrawnAt.IsZero() || late.Seq != 6 {
		t.Fatalf("the kept withdrawal did not apply: %+v", late)
	}
	if kept, _ := sub.st.FeedKeptOf(ctx, tn.ID, feedURL, body, 0, 100); len(kept) != 1 || kept[0].IssuerID != "never-stored" {
		t.Fatalf("kept after: %+v", kept)
	}

	// a document removed here: a later statement about it is kept, not dropped
	var doc store.Attachment
	for _, d := range sub.docs(t, body) {
		if d.IssuerID == "kept-doc" {
			doc = d
		}
	}
	if err := sub.feeds.Attachments.Remove(ctx, admin, "acme", "tresor", doc.ID); err != nil {
		t.Fatal(err)
	}
	wire = append(wire, stmt(7, "kept-doc", func(s *attachment.Statement) { s.Audience = "administrators" }))
	if res := sub.run(t, "reviews"); res.Error != "" || res.Counts["kept"] != 1 {
		t.Fatalf("a statement about a tombstone: %+v", res)
	}
	if k, ok, _ := keptOne(t, sub, tn.ID, feedURL, "kept-doc"); !ok || k.Seq != 7 {
		t.Fatalf("not kept: %+v", k)
	}

	// clearing export drops the feed's refusals and keys file; the kept statements stay while
	// another export feed of the URL remains, and go with the last one
	off, on := false, true
	if _, err := sub.feeds.Change(ctx, admin, "acme", "reviews", attachment.FeedChange{Export: &off}, 0); err != nil {
		t.Fatal(err)
	}
	if r, _ := sub.st.FeedRefusals(ctx, reviews.ID, body, 0, 100); len(r) != 0 {
		t.Fatalf("refusals stay: %+v", r)
	}
	if _, ok, _ := keptOne(t, sub, tn.ID, feedURL, "never-stored"); !ok {
		t.Fatal("kept statements went while another export feed remains")
	}
	if _, err := sub.feeds.Change(ctx, admin, "acme", "all", attachment.FeedChange{Export: &off}, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := keptOne(t, sub, tn.ID, feedURL, "never-stored"); ok {
		t.Fatal("kept statements stay with no export feed")
	}
	// setting export again resets the cursors: the next run reads the history again
	if _, err := sub.feeds.Change(ctx, admin, "acme", "reviews", attachment.FeedChange{Export: &on}, 0); err != nil {
		t.Fatal(err)
	}
	if cur, _ := sub.st.FeedCursors(ctx, reviews.ID); len(cur) != 0 {
		t.Fatalf("cursors: %v", cur)
	}
	if res := sub.run(t, "reviews"); res.Error != "" || res.Counts["kept"] == 0 {
		t.Fatalf("history again: %+v", res)
	}
	if r, _ := sub.st.FeedRefusals(ctx, reviews.ID, body, 0, 100); len(r) != 3 {
		t.Fatalf("refusals again: %+v", r)
	}

	// a run overtaken by a change of export writes no cursor and keeps nothing: the change reset
	// what it would write and asked for another run
	add("late-export", attachment.FeedSpec{})
	onFeed = func() {
		onFeed = nil
		if _, err := sub.feeds.Change(ctx, admin, "acme", "late-export", attachment.FeedChange{Export: &on}, 0); err != nil {
			t.Error(err)
		}
	}
	if res := sub.run(t, "late-export"); res.Error != "" || res.Counts["overtaken"] != 1 {
		t.Fatalf("overtaken: %+v", res)
	}
	le := mustFeed(t, sub, "late-export")
	if cur, _ := sub.st.FeedCursors(ctx, le.ID); len(cur) != 0 || le.RequestedAt.IsZero() || !le.Export {
		t.Fatalf("an overtaken run wrote cursors %v, or no run is requested: %+v", cur, le)
	}
	if res := sub.run(t, "late-export"); res.Error != "" || res.Counts["refused"] != 1 { // the kept statements are there already
		t.Fatalf("the run after: %+v", res)
	}
	if r, _ := sub.st.FeedRefusals(ctx, le.ID, body, 0, 100); len(r) != 1 || r[0].Reason != "content" { // it accepts every kind
		t.Fatalf("refusals after: %+v", r)
	}

	// past the cap on what export feeds keep, an export feed's body waits and the run fails with limits
	if err := store.ExecRaw(ctx, sub.st, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
INSERT INTO feed_kept (tenant_id, issuer, issuer_id, body_hash, seq, statement, signature, kept_at)
SELECT ?, 'https://filler.example/api/v1/tenants/x', 'f' || i, ?, i, '{}', 'x', '2026-10-17T00:00:00Z' FROM n`,
		store.MaxFeedKept, tn.ID, body); err != nil {
		t.Fatal(err)
	}
	wire = append(wire, stmt(8, "another-sbom", sbom))
	cur, _ := sub.st.FeedCursors(ctx, reviews.ID)
	res := sub.run(t, "reviews")
	if res.Error == "" || res.Counts["held"] != 1 {
		t.Fatalf("over the quota: %+v", res)
	}
	if after, _ := sub.st.FeedCursors(ctx, reviews.ID); after[body] != cur[body] {
		t.Fatalf("the cursor moved past a statement not kept: %v -> %v", cur, after)
	}
	if f := mustFeed(t, sub, "reviews"); f.Status != store.FeedFailing {
		t.Fatalf("status: %s", f.Status)
	}
}

func keptOne(t *testing.T, sub *subscriber, tenantID, issuer, id string) (store.FeedKept, bool, error) {
	t.Helper()
	var k store.FeedKept
	var ok bool
	err := sub.st.InTx(ctx, "", func(tx *store.Tx) (err error) { k, ok, err = tx.GetFeedKept(ctx, tenantID, issuer, id); return })
	return k, ok, err
}

// A bundle-mode feed (spec 0014 phase 1b): its URL is only its issuer's identity (no egress port
// check, no credential), it never runs or is requested, takes no export, and its mode never
// changes.
func TestFeedBundleMode(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	sub := newSubscriber(t, srv, "T")
	fp := extfile.Fingerprint(&mustKey(t).PublicKey)
	url := "https://127.0.0.1:1/api/v1/tenants/src" // a port egress never reaches
	sp := func(mod func(*attachment.FeedSpec)) attachment.FeedSpec {
		s := attachment.FeedSpec{Name: "air", URL: url, Keys: []string{fp}, Mode: store.FeedBundle}
		mod(&s)
		return s
	}
	for name, s := range map[string]attachment.FeedSpec{
		"a credential": sp(func(s *attachment.FeedSpec) { s.Credential = "src" }),
		"export":       sp(func(s *attachment.FeedSpec) { s.Export = true }),
		"http":         sp(func(s *attachment.FeedSpec) { s.URL = "http://127.0.0.1/api/v1/tenants/src" }),
		"a mode":       sp(func(s *attachment.FeedSpec) { s.Mode = "offline" }),
	} {
		if _, err := sub.feeds.Add(ctx, admin, "acme", s); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f, err := sub.feeds.Add(ctx, admin, "acme", sp(func(*attachment.FeedSpec) {}))
	if err != nil || f.Mode != store.FeedBundle || !f.RequestedAt.IsZero() || f.Status != store.FeedNew {
		t.Fatalf("add: %+v %v", f, err)
	}
	if _, err := sub.feeds.Sync(ctx, admin, "acme", "air"); !errors.Is(err, attachment.ErrBundle) {
		t.Fatalf("sync: %v", err)
	}
	cred, yes, paused, active := "src", true, store.FeedPaused, store.FeedActive
	for name, ch := range map[string]attachment.FeedChange{"a credential": {Credential: &cred}, "export": {Export: &yes}} {
		if _, err := sub.feeds.Change(ctx, admin, "acme", "air", ch, 0); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("change %s: %v", name, err)
		}
	}
	for _, st := range []*string{&paused, &active} {
		if _, err := sub.feeds.Change(ctx, admin, "acme", "air", attachment.FeedChange{State: st}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if f := mustFeed(t, sub, "air"); !f.RequestedAt.IsZero() {
		t.Fatal("resuming a bundle-mode feed requested a run")
	}
	for _, scheduled := range []bool{false, true} {
		if ids, err := sub.st.DueFeeds(ctx, f.CreatedAt.AddDate(1, 0, 0), scheduled); err != nil || len(ids) != 0 {
			t.Fatalf("due: %v %v", ids, err)
		}
	}
	if _, err := sub.feeds.RunFeed(ctx, f.ID, nil); !errors.Is(err, attachment.ErrNotDue) {
		t.Fatalf("run: %v", err)
	}
}

// A kept withdrawal applies to a document stored later only when the storing feed pins the key that
// signed it, and only for the same body (spec 0014).
func TestFeedKeptPins(t *testing.T) {
	key, other := mustKey(t), mustKey(t)
	fp, otherFP := extfile.Fingerprint(&key.PublicKey), extfile.Fingerprint(&other.PublicKey)
	cdx := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`)
	var wire []attachment.WireStatement
	var keysFile []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/attestation-keys"):
			_, _ = w.Write(keysFile)
		case strings.HasSuffix(r.URL.Path, "/feed"):
			_ = json.NewEncoder(w).Encode(attachment.FeedAnswer{Statements: wire, Bodies: []attachment.HighWater{}})
		default:
			_, _ = w.Write(cdx)
		}
	}))
	t.Cleanup(srv.Close)
	var ks []any
	for _, k := range []*rsa.PrivateKey{key, other} {
		ks = append(ks, map[string]any{"fingerprint": extfile.Fingerprint(&k.PublicKey),
			"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustDER(t, &k.PublicKey)}))})
	}
	keysFile, _ = json.Marshal(map[string]any{"keys": ks})
	feedURL := srv.URL + "/api/v1/tenants/src"
	sub := newSubscriber(t, srv, "T")
	body := tresorBody(t, func(p string) map[string]any { return getJSON(t, sub.env, p) })
	sign := func(k *rsa.PrivateKey, seq int64, id string, withdrawn bool) attachment.WireStatement {
		s := attachment.Statement{Issuer: feedURL, Key: extfile.Fingerprint(&k.PublicKey), IssuerID: id, Seq: seq, BodyHash: body,
			Kind: store.KindSBOM, MediaType: attachment.CycloneDX, ContentHash: blob.ContentHash(cdx), Size: int64(len(cdx)), Title: "SBOM",
			IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "administrators"}
		if withdrawn {
			s.Withdrawn, s.Kind, s.MediaType, s.ContentHash, s.Size, s.Title = true, "", "", "", 0, ""
		}
		b, _ := s.Canonical()
		sig, err := attachment.Sign(ctx, keySigner{k}, b)
		if err != nil {
			t.Fatal(err)
		}
		return attachment.WireStatement{Statement: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(sig)}
	}
	add := func(name string, kinds, keys []string) {
		if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: name, URL: feedURL, Credential: "src", Keys: keys,
			Kinds: kinds, Export: true, Offer: store.OfferChannels, Channels: []string{"prod"}}); err != nil {
			t.Fatal(err)
		}
	}
	// "reviews" refuses the SBOMs by kind and keeps their withdrawals: one signed by each key
	wire = []attachment.WireStatement{sign(key, 1, "a", false), sign(key, 2, "b", false), sign(other, 3, "a", true), sign(key, 4, "b", true)}
	add("reviews", []string{store.KindReview}, []string{fp, otherFP})
	if res := sub.run(t, "reviews"); res.Error != "" || res.Counts["kept"] != 2 {
		t.Fatalf("reviews: %+v", res)
	}
	// "sboms" pins only the first key: it stores both documents; b's withdrawal (its key) applies, a's
	// (a key it does not pin) does not; both kept rows go
	wire = wire[:2]
	add("sboms", nil, []string{fp})
	if res := sub.run(t, "sboms"); res.Error != "" || res.Counts["received"] != 2 {
		t.Fatalf("sboms: %+v", res)
	}
	got := map[string]bool{}
	for _, d := range sub.docs(t, body) {
		got[d.IssuerID] = !d.WithdrawnAt.IsZero()
	}
	if len(got) != 2 || got["a"] || !got["b"] {
		t.Fatalf("withdrawn: %v", got)
	}
	tn, _ := sub.st.GetTenant(ctx, "acme")
	if k, _ := sub.st.FeedKeptOf(ctx, tn.ID, feedURL, body, 0, 100); len(k) != 0 {
		t.Fatalf("kept rows stay: %+v", k)
	}
}

// A bundle-mode feed's import holds a body at a statement that does not verify, or that comes out of
// seq order: nothing after it is applied, and the cursor stops just below it (spec 0014 phase 1b).
func TestFeedImportHolds(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	sub := newSubscriber(t, srv, "T")
	key, stranger := mustKey(t), mustKey(t)
	fp := extfile.Fingerprint(&key.PublicKey)
	url := "https://src.example.org/api/v1/tenants/src"
	f, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "air", URL: url, Keys: []string{fp}, Mode: store.FeedBundle,
		Offer: store.OfferChannels, Channels: []string{"prod"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.SetFeedBundleMark(ctx, f.ID, "b-1", time.Now(), strings.Repeat("a", 64))
	}); err != nil {
		t.Fatal(err)
	}
	body := tresorBody(t, func(p string) map[string]any { return getJSON(t, sub.env, p) })
	tn, _ := sub.st.GetTenant(ctx, "acme")
	content := []byte(review)
	sign := func(k *rsa.PrivateKey, seq int64) attachment.BundleStatement {
		s := attachment.Statement{Issuer: url, Key: fp, IssuerID: "d" + time.Duration(seq).String(), Seq: seq, BodyHash: body,
			Kind: store.KindReview, MediaType: attachment.Review, ContentHash: blob.ContentHash(content), Size: int64(len(content)),
			Title: "Review", IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "readers"}
		b, _ := s.Canonical()
		sig, err := attachment.Sign(ctx, keySigner{k}, b)
		if err != nil {
			t.Fatal(err)
		}
		return attachment.BundleStatement{Statement: b, Signature: sig}
	}
	der := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustDER(t, &key.PublicKey)})
	keysFile, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"public_key": string(der)}}})
	// in order: a cursor never moves back
	for _, c := range []struct {
		name  string
		stmts []attachment.BundleStatement
		want  int64
	}{
		{"a bad signature", []attachment.BundleStatement{sign(key, 1), sign(stranger, 2), sign(key, 3)}, 1},
		{"out of order", []attachment.BundleStatement{sign(key, 5), sign(key, 4), sign(key, 6)}, 5},
	} {
		name := c.name
		spool := attachment.NewDocSpool(t.TempDir(), 0)
		if err := spool.Put(blob.ContentHash(content), content); err != nil {
			t.Fatal(err)
		}
		fi, err := sub.feeds.ImportFeed(ctx, tn, mustFeed(t, sub, "air"), "b-1", map[string][2]int64{body: {0, 10}}, nil, spool, false,
			func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		fi.Keys(keysFile)
		if err := fi.Batch(ctx, c.stmts); err != nil {
			t.Fatal(err)
		}
		res, err := fi.Finish(ctx, "os:1:t", true)
		if err != nil || res.Counts["held"] != 1 {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		if cur, _ := sub.st.FeedCursors(ctx, f.ID); cur[body] != c.want {
			t.Errorf("%s: cursor %v, want %d", name, cur, c.want)
		}
		spool.Close()
	}
}
