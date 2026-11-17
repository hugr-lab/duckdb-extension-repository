package attachment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0046, the edges of waiting: another spelling of a media type is refused for good, as it
// always was; a wait followed by a hold or a transient failure in the same body is counted once; a
// malformed statement whose document id cannot be read still moves its body's cursor; a retake
// overtaken by a change of export writes nothing; a kept row whose statement was withdrawn at the
// source goes with the gateway's retake.

func TestFeedMediaSpelling(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "spelt", []byte(reviewDoc), func(st *attachment.Statement) { st.MediaType = attachment.Review + "; charset=utf-8" }, nil)
	s.src.offer(s.body, "later", []byte(reviewDoc), func(st *attachment.Statement) { st.MediaType = "application/vnd.kista.review.v9+json" }, nil)
	s.src.offer(s.body, "none", []byte(reviewDoc), func(st *attachment.Statement) { st.MediaType = "not a media type;;" }, nil)
	f := s.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	res := s.run("src")
	if reasons(res)[attachment.RefusedMediaSpelling] != 2 || res.Counts["refused"] != 2 || res.Counts["waiting"] != 1 || res.Error != "" {
		t.Fatalf("the run: %+v", res)
	}
	w, _ := s.st.FeedWaits(ctx, f.ID)
	if x := w[s.body]; x.From != 2 || x.Count != 1 || x.Why != attachment.WaitMedia {
		t.Fatalf("the waiting state: %+v", w)
	}
	// each is kept for export under its own word: a site never reads the refusal's note as a wait
	rs, _ := s.st.FeedRefusals(ctx, f.ID, s.body, 0, 10)
	if len(rs) != 3 || rs[0].Reason != "media type spelling" || rs[1].Reason != "media type" || rs[2].Reason != "media type spelling" ||
		attachment.Waits(rs[0].Reason) || !attachment.Waits(rs[1].Reason) {
		t.Fatalf("kept: %+v", rs)
	}
}

// A change of a feed that is committed answers with its record even when what the feed passed over
// cannot be read then: the counts are left out, the change is not reported as failed.
func TestFeedChangeAnswersWithoutCounts(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "r", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil)
	s.add("src", nil)
	s.run("src")
	readers := false
	got, err := s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Readers: &readers}, 0)
	if err != nil || got.Waiting[attachment.WaitKind] != 1 || got.Readers {
		t.Fatalf("a change: %+v %v", got.Waiting, err)
	}
	s.feeds.FailWaitingRead(errors.New("the database is away"))
	readers = true
	got, err = s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Readers: &readers}, 0)
	if err != nil || !got.Readers || got.Waiting != nil {
		t.Fatalf("a change whose counts could not be read: %+v %v %v", got.Readers, got.Waiting, err)
	}
	if got, err := s.feeds.Sync(ctx, admin, "acme", "src"); err != nil || got.RequestedAt.IsZero() || got.Waiting != nil {
		t.Fatalf("a sync whose counts could not be read: %+v %v", got.Waiting, err)
	}
}

func TestFeedWaitThenHold(t *testing.T) {
	s := newSubscriber(t)
	src := s.src
	report := func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }
	src.offer(s.body, "r1", []byte(`{}`), report, nil)           // 1: waits
	two := src.offer(s.body, "bad", []byte(reviewDoc), nil, nil) // 2: does not verify: holds the body
	src.spoil(two)
	src.offer(s.body, "ok", []byte(reviewDoc), nil, nil)   // 3
	src.offer(s.other, "r2", []byte(`{}`), report, nil)    // 4: waits
	src.offer(s.other, "doc", []byte(reviewDoc), nil, nil) // 5: its document fails to download for now
	src.down = map[string]bool{"doc": true}
	f := s.add("src", nil)
	state := func() (map[string]int64, map[string]store.FeedWait) {
		t.Helper()
		cur, err := s.st.FeedCursors(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		w, err := s.st.FeedWaits(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cur, w
	}
	for i := range 2 { // the second run meets the hold and the failure again: the waits are counted once
		res := s.run("src")
		want := 2
		if i == 1 {
			want = 0
		}
		if res.Counts["waiting"] != want || reasons(res)["signature"] != 1 || res.Counts["failed"] != 1 || res.Counts["received"] != 0 {
			t.Fatalf("run %d: %+v", i, res)
		}
		cur, w := state()
		if cur[s.body] != 1 || cur[s.other] != 4 || w[s.body].Count != 1 || w[s.body].From != 1 || w[s.other].Count != 1 || w[s.other].From != 4 {
			t.Fatalf("run %d: cursors %v, waits %+v", i, cur, w)
		}
	}
	src.down = nil
	if res := s.run("src"); res.Counts["received"] != 1 || res.Counts["waiting"] != 0 {
		t.Fatalf("once the document downloads: %+v", res)
	}
	if cur, w := state(); cur[s.other] != 5 || w[s.other].Count != 1 {
		t.Fatalf("after: %v %+v", cur, w)
	}
}

func TestFeedMalformedWithoutDocumentID(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "x", []byte(reviewDoc), nil, func(m map[string]any) { m["issuer_id"] = 7 })
	s.src.offer(s.body, "ok", []byte(reviewDoc), nil, nil)
	// one whose body cannot be read is refused at every run: no cursor can move for it
	s.src.offer(s.other, "y", []byte(reviewDoc), nil, func(m map[string]any) { m["seq"] = "3" })
	s.src.stmts[2].body = s.other
	f := s.add("src", nil)
	res := s.run("src")
	if reasons(res)["statement"] != 2 || res.Counts["received"] != 1 || reasons(res)["signature"] != 0 || res.Error != "" {
		t.Fatalf("the run: %+v", res)
	}
	if cur, _ := s.st.FeedCursors(ctx, f.ID); cur[s.body] != 2 || cur[s.other] != 0 {
		t.Fatalf("the cursors: %v", cur)
	}
	if res := s.run("src"); reasons(res)["statement"] != 1 || res.Counts["refused"] != 1 {
		t.Fatalf("the next run: %+v", res)
	}
}

func TestFeedRetakeOvertaken(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "r", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil)
	f := s.add("src", nil)
	if res := s.run("src"); res.Counts["waiting"] != 1 {
		t.Fatalf("the first run: %+v", res)
	}
	// an upgraded binary's run; export is switched on as the run fetches the keys file, before its
	// retake: the change reset the cursors and asked for another run, and this one writes nothing
	s.feeds.SetReaderLevel(attachment.ReaderLevel + 1)
	on := true
	s.src.onKeys = func() {
		s.src.onKeys = nil
		if _, err := s.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Export: &on}, 0); err != nil {
			t.Error(err)
		}
	}
	res := s.run("src")
	if res.Counts["overtaken"] != 1 || res.Error != "" || res.Counts["waiting"] != 0 {
		t.Fatalf("the overtaken run: %+v", res)
	}
	if cur, _ := s.st.FeedCursors(ctx, f.ID); len(cur) != 0 {
		t.Fatalf("the overtaken run wrote cursors: %v", cur)
	}
	if got := s.feed("src"); got.RequestedAt.IsZero() || got.Status != store.FeedOK {
		t.Fatalf("the feed after: requested %v, %s", got.RequestedAt, got.Status)
	}
}

func TestGatewayRetakeOfWithdrawn(t *testing.T) {
	s := newSubscriber(t)
	report := func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }
	one := s.src.offer(s.body, "r1", []byte(`{}`), report, nil)
	s.src.offer(s.body, "ok", []byte(reviewDoc), nil, nil)
	s.src.offer(s.body, "r3", []byte(`{}`), report, nil)
	f := s.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	if res := s.run("src"); res.Counts["waiting"] != 2 || res.Counts["received"] != 1 {
		t.Fatalf("the first run: %+v", res)
	}
	kept := func() []int64 {
		t.Helper()
		rs, err := s.st.FeedRefusals(ctx, f.ID, s.body, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rs {
			out = append(out, r.Seq)
		}
		return out
	}
	if k := kept(); len(k) != 2 || k[0] != 1 || k[1] != 3 {
		t.Fatalf("kept: %v", k)
	}
	// the source withdraws the first report (it offers it no more), then the gateway is upgraded:
	// its run takes the range again, and the kept row of the withdrawn statement is gone, so no
	// later bundle carries it as waiting for ever
	s.src.hide(one, true)
	s.feeds.SetReaderLevel(attachment.ReaderLevel + 1)
	if res := s.run("src"); res.Counts["waiting"] != 1 || res.Counts["received"] != 0 || res.Error != "" {
		t.Fatalf("the upgraded run: %+v", res)
	}
	if k := kept(); len(k) != 1 || k[0] != 3 {
		t.Fatalf("kept after the retake: %v", k)
	}
	if w, _ := s.st.FeedWaits(ctx, f.ID); w[s.body].From != 3 || w[s.body].Count != 1 {
		t.Fatalf("the waiting state: %+v", w)
	}
}

// bundleFeed adds a bundle-mode feed of the source and starts an import of the body's statements in
// (after, upTo], their documents in the spool.
func (s *subscriber) bundleImport(name string, after, upTo int64, dryRun bool, notes map[int64]string) (*attachment.FeedImport, store.Feed) {
	s.t.Helper()
	fd, err := s.feeds.Get(ctx, admin, "acme", name)
	if errors.Is(err, store.ErrNotFound) {
		fd = s.add(name, func(sp *attachment.FeedSpec) { sp.Mode, sp.Credential = store.FeedBundle, "" })
	} else if err != nil {
		s.t.Fatal(err)
	}
	tn, _ := s.st.GetTenant(ctx, "acme")
	if err := s.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetFeedBundleMark(ctx, fd.ID, "bundle-1", time.Now(), "sha") }); err != nil {
		s.t.Fatal(err)
	}
	spool := attachment.NewDocSpool(s.t.TempDir(), 0)
	s.t.Cleanup(spool.Close)
	stmts := s.src.wire(s.body, after, upTo)
	for i := range stmts {
		seq := after + int64(i) + 1
		stmts[i].Note = notes[seq]
	}
	for _, doc := range s.src.docs {
		if err := spool.Put(blob.ContentHash(doc), doc); err != nil {
			s.t.Fatal(err)
		}
	}
	fi, err := s.feeds.ImportFeed(ctx, tn, fd, "bundle-1", map[string][2]int64{s.body: {after, upTo}}, nil, spool, dryRun,
		func(context.Context) error { return nil })
	if err != nil {
		s.t.Fatal(err)
	}
	fi.Keys(s.src.keysFile())
	if err := fi.Batch(ctx, stmts); err != nil {
		s.t.Fatal(err)
	}
	return fi, fd
}

// A bundle's statements, judged without storing (a dry run) and stored: what would wait is counted
// would_wait; a wait followed by a statement that does not verify is recorded with the cursor that
// stops below the hold.
func TestBundleWaits(t *testing.T) {
	s := newSubscriber(t)
	src := s.src
	report := func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }
	src.offer(s.body, "ok", []byte(reviewDoc), nil, nil)                                                                                       // 1
	src.offer(s.body, "r", []byte(`{}`), report, nil)                                                                                          // 2: waits here
	src.offer(s.body, "newer", []byte(reviewDoc), nil, v2)                                                                                     // 3: waits here
	src.offer(s.body, "gone", []byte(`{"not":"carried"}`), nil, nil)                                                                           // 4: readable, carried without content, the exporter's wait
	src.offer(s.body, "spelt", []byte(reviewDoc), func(st *attachment.Statement) { st.MediaType = attachment.Review + ";charset=utf-8" }, nil) // 5
	// 6: readable, carried without content, the exporter refused it for its media type's spelling:
	// not a wait, whatever the word resembles
	src.offer(s.body, "gone2", []byte(`{"not":"carried either"}`), nil, nil)
	seven := src.offer(s.body, "bad", []byte(reviewDoc), nil, nil) // 7: does not verify
	src.spoil(seven)
	delete(src.docs, "gone")
	delete(src.docs, "gone2")
	notes := map[int64]string{4: attachment.WaitKind, 6: attachment.RefusedMediaSpelling}

	fi, fd := s.bundleImport("air", 0, 7, true, notes)
	res, err := fi.Finish(ctx, "os:1:t", true)
	if err != nil || res.Counts["would_wait"] != 3 || res.Counts["would_store"] != 1 || reasons(res)[attachment.RefusedMediaSpelling] != 1 ||
		reasons(res)["bundle: no content"] != 1 || reasons(res)["signature"] != 1 || res.Counts["waiting"] != 0 {
		t.Fatalf("the dry run: %+v %v", res, err)
	}
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); len(cur) != 0 {
		t.Fatalf("a dry run moved cursors: %v", cur)
	}

	fi, _ = s.bundleImport("air", 0, 7, false, notes)
	res, err = fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() != nil || res.Counts["waiting"] != 3 || res.Counts["received"] != 1 ||
		reasons(res)[attachment.RefusedMediaSpelling] != 1 || reasons(res)["bundle: no content"] != 1 {
		t.Fatalf("the import: %+v %v %v", res, err, fi.Failed())
	}
	// the cursor stops below the statement that does not verify; the waits are recorded with it: a
	// row has one level, the lower (0: the exporter's)
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); cur[s.body] != 6 {
		t.Fatalf("the cursor: %v", cur)
	}
	w, _ := s.st.FeedWaits(ctx, fd.ID)
	if x := w[s.body]; x.From != 2 || x.Count != 3 || x.Level != 0 {
		t.Fatalf("the waiting state: %+v", x)
	}
	got := s.feed("air")
	if len(got.Waiting) != 0 || got.WaitingSource[attachment.WaitKind] != 3 {
		t.Fatalf("the feed's view: %v %v", got.Waiting, got.WaitingSource)
	}
	if line := attachment.ExporterOlderThanSource(got.WaitingSource); line != "The exporting kista is older than its source: 3 statements arrived without content (`unknown kind`)." {
		t.Fatalf("the line: %q", line)
	}
}

// A plan written while a bundle is imported takes the feed's cursors back to what waits (spec
// 0046); the import, which read them when it started, must not then move them forward past it.
func TestImportAfterRetake(t *testing.T) {
	s := newSubscriber(t)
	for range 24 {
		s.src.seq++
	}
	s.src.offer(s.body, "late", []byte(reviewDoc), nil, nil) // 25
	fd := s.add("air", func(sp *attachment.FeedSpec) { sp.Mode, sp.Credential = store.FeedBundle, "" })
	if err := s.st.InTx(ctx, store.FeedLock(fd.TenantID), func(tx *store.Tx) error {
		if err := tx.SetFeedCursor(ctx, fd.ID, s.body, 20); err != nil {
			return err
		}
		return tx.AddFeedWait(ctx, fd.ID, store.FeedWait{BodyHash: s.body, From: 12, Count: 1, Why: attachment.WaitKind, Level: 0})
	}); err != nil {
		t.Fatal(err)
	}
	fi, _ := s.bundleImport("air", 20, 30, false, nil)
	// the plan, meanwhile
	if err := s.st.InTx(ctx, store.FeedLock(fd.TenantID), func(tx *store.Tx) error {
		_, err := tx.RetakeFeedCursors(ctx, fd.ID, attachment.ReaderLevel)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() == nil || res.Error == "" {
		t.Fatalf("the import went on over a plan's retake: %+v %v", res, fi.Failed())
	}
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); cur[s.body] != 11 {
		t.Fatalf("the cursor after: %v", cur)
	}
	if w, _ := s.st.FeedWaits(ctx, fd.ID); w[s.body].From != 12 || w[s.body].Count != 1 {
		t.Fatalf("the waiting state after: %+v", w)
	}
	if got := s.feed("air"); got.Status != store.FeedFailing {
		t.Fatalf("the feed's status: %s", got.Status)
	}
}

// The versions of a document this kista does not read (spec 0046): they wait at a feed, and are
// refused at the attach route; what is not a document of its media type at all is refused at both.
func TestDocumentVersions(t *testing.T) {
	later := []byte(`{"schema":"kista.review.v1","verdict":"deferred","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest"}`)
	for name, c := range map[string]struct {
		kind, mt, doc string
	}{
		"SPDX 3 (JSON-LD)":           {store.KindSBOM, attachment.SPDX, `{"@context":"https://spdx.org/rdf/3.0.1/spdx-context.jsonld","@graph":[]}`},
		"SPDX 3, a context list":     {store.KindSBOM, attachment.SPDX, `{"@context":["https://spdx.org/rdf/3.0.1/spdx-context.jsonld"],"@graph":[]}`},
		"a CycloneDX 2 VDR":          {store.KindVulnerabilities, attachment.CycloneDX, `{"bomFormat":"CycloneDX","specVersion":"2.0","vulnerabilities":[{}]}`},
		"a review's unknown verdict": {store.KindReview, attachment.Review, string(later)},
	} {
		if _, err := attachment.SummarizeReceived(c.kind, c.mt, "", []byte(c.doc)); !errors.Is(err, attachment.ErrDocumentVersion) {
			t.Errorf("%s, received: %v", name, err)
		}
		if _, err := attachment.Summarize(c.kind, c.mt, "", []byte(c.doc)); !errors.Is(err, attachment.ErrDocument) {
			t.Errorf("%s, attached: %v", name, err)
		}
	}
	// strict at attach: an unknown verdict there is a broken review, not a version
	if _, err := attachment.Summarize(store.KindReview, attachment.Review, "", later); errors.Is(err, attachment.ErrDocumentVersion) {
		t.Errorf("an unknown verdict attached here: %v", err)
	}
	for name, c := range map[string]struct {
		kind, mt, doc string
	}{
		"a JSON-LD document of another context": {store.KindSBOM, attachment.SPDX, `{"@context":"https://schema.org","@graph":[]}`},
		"a review without a verdict":            {store.KindReview, attachment.Review, `{"schema":"kista.review.v1","analysed_at":"2026-10-03T12:00:00Z","analyser":"x"}`},
	} {
		if _, err := attachment.SummarizeReceived(c.kind, c.mt, "", []byte(c.doc)); !errors.Is(err, attachment.ErrDocument) || errors.Is(err, attachment.ErrDocumentVersion) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a VDR that names no specVersion, or a 1.x one, is read as it always was
	for _, doc := range []string{`{"bomFormat":"CycloneDX","vulnerabilities":[{}]}`, `{"bomFormat":"CycloneDX","specVersion":"1.6","vulnerabilities":[{}]}`} {
		if sum, err := attachment.SummarizeReceived(store.KindVulnerabilities, attachment.CycloneDX, "", []byte(doc)); err != nil {
			t.Errorf("%s: %q %v", doc, sum, err)
		}
	}
}
