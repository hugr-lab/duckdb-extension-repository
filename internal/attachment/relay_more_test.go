package attachment_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0045, after its reviews: two export feeds of one issuer at unequal cursors, the quota before
// a download and a held body's later statements, a relay's note, the answer's bound as written, the
// relay's reads checked, and where its answers end.

// objects are the attachment objects in the tenant's storage.
func (e *env) objects(t *testing.T) int {
	t.Helper()
	n := 0
	if err := e.blobs.List(ctx, blob.AttachmentKey(""), func(string, int64, time.Time) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

func seqs(t *testing.T, ans attachment.FeedAnswer) string {
	t.Helper()
	var out []string
	for _, w := range ans.Statements {
		out = append(out, fmt.Sprintf("%d%s", decoded(t, w).Seq, map[bool]string{true: "!" + w.Note}[w.Note != ""]))
	}
	return strings.Join(out, " ")
}

// A statement the feed that read furthest refused for its kind is not answered "not held by the
// relay" while another feed of the issuer that hands on takes the kind and has not read that far:
// the downstream would refuse it for good, and never get the document the other feed stores later.
func TestRelayUnequalFeeds(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	src := a.src
	src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)          // 1
	src.offer(a.body, "p1", reportJSON(1, a.body), asReport, nil) // 2: narrow leaves the kind out
	src.offer(a.body, "r2", []byte(reviewDoc), nil, nil)          // 3
	a.add("narrow", func(sp *attachment.FeedSpec) { sp.Export, sp.Kinds = true, []string{store.KindReview} })
	a.add("wide", func(sp *attachment.FeedSpec) { sp.Export = true })
	if res := a.run("narrow"); res.Counts["received"] != 2 || reasons(res)["kind"] != 1 {
		t.Fatalf("narrow's run: %+v", res)
	}
	ask := func() attachment.FeedAnswer {
		t.Helper()
		ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}}, srcURL)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	// wide has not run: the body's answer ends just below the report
	if got := seqs(t, ask()); got != "1" {
		t.Fatalf("while wide is behind: %q", got)
	}
	b := newSubscriber(t)
	b.feeds.Fetch = &throughRelay{t: t, relay: a.svc}
	fb := b.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Keys = []string{src.fp} })
	if res := b.run("own"); res.Counts["received"] != 1 || res.Counts["refused"] != 0 || b.cursor(fb, b.body) != 1 {
		t.Fatalf("B's run while wide is behind: %+v", res)
	}
	// the same while wide is paused: it still hands on, and may yet store the report
	paused, active := store.FeedPaused, store.FeedActive
	if _, err := a.feeds.Change(ctx, admin, "acme", "wide", attachment.FeedChange{State: &paused}, 0); err != nil {
		t.Fatal(err)
	}
	if got := seqs(t, ask()); got != "1" {
		t.Fatalf("while wide is paused: %q", got)
	}
	if res := b.run("own"); res.Counts["refused"] != 0 || b.cursor(fb, b.body) != 1 {
		t.Fatalf("B's run while wide is paused: %+v", res)
	}
	// wide catches up and stores it: B receives the report and what follows
	if _, err := a.feeds.Change(ctx, admin, "acme", "wide", attachment.FeedChange{State: &active}, 0); err != nil {
		t.Fatal(err)
	}
	if res := a.run("wide"); res.Counts["received"] != 1 {
		t.Fatalf("wide's run: %+v", res)
	}
	if got := seqs(t, ask()); got != "1 2 3" {
		t.Fatalf("once wide read it: %q", got)
	}
	if res := b.run("own"); res.Counts["received"] != 2 || res.Counts["refused"] != 0 || b.docs()["p1"].Kind != store.KindReport {
		t.Fatalf("B's run once wide read it: %+v", res)
	}
	// a kind no feed that hands on takes is still "not held": nobody here will store it
	src.offer(a.body, "s1", []byte(sbom), func(st *attachment.Statement) { st.Kind, st.MediaType = store.KindSBOM, attachment.CycloneDX }, nil) // 4
	kinds := []string{store.KindReview, store.KindReport}
	if _, err := a.feeds.Change(ctx, admin, "acme", "wide", attachment.FeedChange{Kinds: &kinds}, 0); err != nil {
		t.Fatal(err)
	}
	a.run("narrow")
	if got := seqs(t, ask()); got != "1 2 3 4!"+attachment.NoteNotHeld {
		t.Fatalf("a kind no feed takes: %q", got)
	}
}

// At the document quota nothing is fetched that could not be stored; a held body's later
// statements that need no new record are applied; the run is tried again at the feed's interval.
func TestFeedQuotaBeforeDownload(t *testing.T) {
	s := newSubscriber(t)
	s.feeds.Interval = time.Hour
	was := store.MaxTenantAttachments
	defer func() { store.MaxTenantAttachments = was }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)  // 1: stored
	s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil)  // 2: over the quota
	s.src.offer(s.other, "r3", []byte(reviewDoc), nil, nil) // 3: over the quota
	s.src.offer(s.body, "r1", nil, withdraw, nil)           // 4: needs no new record
	f := s.add("src", nil)
	for i := range 3 {
		res := s.run("src")
		if res.Counts["held"] != 2 || res.Error == "" || s.src.downloads != 1 {
			t.Fatalf("run %d at the quota: %+v, %d downloads", i+1, res, s.src.downloads)
		}
	}
	// the withdrawal behind the held document was applied, and no cursor moved past the hold
	if d := s.docs()["r1"]; d.WithdrawnAt.IsZero() || d.Seq != 4 || s.cursor(f, s.body) != 1 || s.cursor(f, s.other) != 0 {
		t.Fatalf("the held body: %+v, cursors %d %d", d, s.cursor(f, s.body), s.cursor(f, s.other))
	}
	// nothing was put that has no record
	if n := s.objects(t); n != 1 {
		t.Fatalf("%d objects for 1 record", n)
	}
	// limits: tried again at the interval, not in five minutes
	if got := s.feed("src"); got.Status != store.FeedFailing || time.Until(got.NextRunAt) < 50*time.Minute {
		t.Fatalf("the next run: %s in %s", got.Status, time.Until(got.NextRunAt))
	}
	store.MaxTenantAttachments = was
	res := s.run("src")
	if res.Error != "" || res.Counts["received"] != 2 || s.cursor(f, s.body) != 4 || s.objects(t) != 3 || s.docs()["r1"].WithdrawnAt.IsZero() {
		t.Fatalf("the run with room: %+v", res)
	}
}

// The same at a bundle import, and a dry run at the quota reports nothing it would store.
func TestBundleQuotaLaterStatements(t *testing.T) {
	s := newSubscriber(t)
	was := store.MaxTenantAttachments
	defer func() { store.MaxTenantAttachments = was }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil) // 1
	s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil) // 2: over the quota
	s.src.offer(s.body, "r1", nil, withdraw, nil)          // 3
	fi, fd := s.bundleImport("air", 0, 3, true, nil)
	res, err := fi.Finish(ctx, "os:1:t", true)
	if err != nil || res.Counts["would_store"] != 1 || res.Counts["would_hold"] != 1 || res.Counts["would_withdraw"] != 1 {
		t.Fatalf("the dry run at the quota: %+v %v", res, err)
	}
	fi, _ = s.bundleImport("air", 0, 3, false, nil)
	res, err = fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() == nil || res.Counts["received"] != 1 || res.Counts["held"] != 1 {
		t.Fatalf("the import at the quota: %+v %v", res, err)
	}
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); cur[s.body] != 1 || s.docs()["r1"].WithdrawnAt.IsZero() || s.objects(t) != 1 {
		t.Fatalf("the held body: cursor %v, %+v, %d objects", cur, s.docs()["r1"], s.objects(t))
	}
	// a dry run with no room at all
	fi, _ = s.bundleImport("air", 1, 3, true, nil)
	if res, _ := fi.Finish(ctx, "os:1:t", true); res.Counts["would_store"] != 0 || res.Counts["would_hold"] != 1 {
		t.Fatalf("a dry run with no room: %+v", res)
	}
}

// attachments.feed_answer_max bounds the bytes the API writes: a keys file whose characters the
// API escapes counts as written, and one that leaves no room for a statement is not handed on.
func TestRelayAnswerBoundAsWritten(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.src.keysPad = strings.Repeat("<", 60<<10) // 60 KiB as fetched, 360 KiB as the API writes it
	pad := func(m map[string]any) { m["pad"] = strings.Repeat("x", 9000) }
	const n = 80
	for i := range n { // one chain: the heads limit does not count it
		a.src.offer(a.body, fmt.Sprintf("r%02d", i), []byte(reviewDoc), func(st *attachment.Statement) {
			if i > 0 {
				st.Supersedes = fmt.Sprintf("r%02d", i-1)
			}
		}, pad)
	}
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	if res := a.run("src"); res.Error != "" || res.Counts["received"] != n {
		t.Fatalf("the relay's own run: %+v", res)
	}
	ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}}, srcURL)
	raw, _ := json.Marshal(ans) // as the API's reply does
	if err != nil || !ans.More || len(ans.Statements) == 0 || len(raw) > attachment.MaxFeedAnswer || len(raw) < attachment.MaxFeedAnswer*3/4 {
		t.Fatalf("the answer: %d statements, %d bytes, more %v, %v", len(ans.Statements), len(raw), ans.More, err)
	}
	if ks, err := attachment.ParseKeysFile(ans.Keys); err != nil || ks[a.src.fp] == nil {
		t.Fatalf("the keys as written: %v", err)
	}
	// under a bound the keys file alone nearly fills, the issuer is not handed on: no answer could
	// carry a statement
	a.svc.FeedAnswerMax = attachment.MinFeedAnswer
	ans, err = a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}}, srcURL)
	if err != nil || len(ans.Keys) != 0 || len(ans.Statements) != 0 || ans.More {
		t.Fatalf("a keys file over the bound: %+v %v", ans, err)
	}
}

// The relay's reads are not one snapshot: a body whose feed's export setting or cursor changed
// while the answer was read is answered nothing.
func TestRelaySnapshot(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)
	a.src.offer(a.other, "r2", []byte(reviewDoc), nil, nil)
	f := a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	a.run("src")
	ask := func() attachment.FeedAnswer {
		t.Helper()
		ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}, {BodyHash: a.other}}, srcURL)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	if got := seqs(t, ask()); got != "1 2" {
		t.Fatalf("undisturbed: %q", got)
	}
	// the collector removes a body's cursor meanwhile: that body is answered nothing, the other is
	a.svc.AfterRelayRead(func() {
		if err := store.ExecRaw(ctx, a.st, "DELETE FROM feed_cursors WHERE feed_id = ? AND body_hash = ?", f.ID, a.body); err != nil {
			t.Fatal(err)
		}
	})
	if ans := ask(); seqs(t, ans) != "2" || len(ans.Bodies) != 1 || ans.Bodies[0].BodyHash != a.other || len(ans.Keys) == 0 {
		t.Fatalf("a cursor gone meanwhile: %q %+v", seqs(t, ans), ans.Bodies)
	}
	// export switched off and on meanwhile (what the feed kept dropped, its cursors reset): nothing
	off, on := false, true
	a.svc.AfterRelayRead(func() {
		for _, v := range []*bool{&off, &on} {
			if _, err := a.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Export: v}, 0); err != nil {
				t.Fatal(err)
			}
		}
	})
	a.run("src")
	if ans := ask(); len(ans.Statements) != 0 || len(ans.Bodies) != 0 {
		t.Fatalf("export changed meanwhile: %q", seqs(t, ans))
	}
	// export switched off meanwhile: not handed on at all, as for any issuer that is not
	a.svc.AfterRelayRead(func() {
		if _, err := a.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Export: &off}, 0); err != nil {
			t.Fatal(err)
		}
	})
	a.run("src")
	if ans := ask(); len(ans.Statements) != 0 || len(ans.Keys) != 0 {
		t.Fatalf("export cleared meanwhile: %+v", ans)
	}
}

// A relayed answer that ends at a statement the feed cannot verify says so on the feed's record.
func TestRelayStoppedIsShown(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)
	a.src.offer(a.body, "r2", []byte(reviewDoc), nil, nil)
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	a.run("src")
	ask := func() attachment.FeedAnswer {
		ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}}, srcURL)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	if ask(); a.feed("src").RelayStopped != 0 || attachment.RelayStopped(a.feed("src")) != "" {
		t.Fatalf("an answer that verified: %+v", a.feed("src"))
	}
	r2 := a.docs()["r2"]
	if err := store.ExecRaw(ctx, a.st, "UPDATE attachments SET statement = ? WHERE id = ?", strings.Replace(r2.Statement, `"title":"Review"`, `"title":"Other"`, 1), r2.ID); err != nil {
		t.Fatal(err)
	}
	if got := seqs(t, ask()); got != "1" {
		t.Fatalf("the answer: %q", got)
	}
	got := a.feed("src")
	if got.RelayStopped != 1 || got.RelayStoppedAt.IsZero() || !strings.Contains(attachment.RelayStopped(got), "ended 1 bodies at a statement that does not verify") {
		t.Fatalf("the feed's record: %d %v %q", got.RelayStopped, got.RelayStoppedAt, attachment.RelayStopped(got))
	}
	// new pins: said anew by the next answer
	pins := []string{a.src.fp, "sha256:" + strings.Repeat("1", 64)}
	if f, err := a.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Keys: &pins}, 0); err != nil || f.RelayStopped != 0 || a.feed("src").RelayStopped != 0 {
		t.Fatalf("after a change of pins: %+v %v", f, err)
	}
}

// A report an older binary left waiting as a kind it did not know is stored once the binary knows
// it: the reader level rose with the kind, so the feed takes it again by itself (spec 0046).
func TestReportTakenAfterUpgrade(t *testing.T) {
	s := newSubscriber(t)
	s.src.offer(s.body, "p1", reportJSON(1, s.body), asReport, nil)
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)
	f := s.add("src", nil)
	// the release before spec 0045: reader level 1, and no kind report
	known := attachment.ForgetKind(store.KindReport)
	s.feeds.SetReaderLevel(1)
	res := s.run("src")
	known()
	if res.Counts["waiting"] != 1 || res.Counts["received"] != 1 || s.feed("src").Waiting[attachment.WaitKind] != 1 || s.cursor(f, s.body) != 2 {
		t.Fatalf("the older binary's run: %+v", res)
	}
	// this binary
	s.feeds.SetReaderLevel(0)
	if attachment.ReaderLevel < 2 {
		t.Fatalf("the reader level did not rise with the kind: %d", attachment.ReaderLevel)
	}
	res = s.run("src")
	if res.Error != "" || res.Counts["received"] != 1 || res.Counts["waiting"] != 0 || s.docs()["p1"].Kind != store.KindReport || len(s.feed("src").Waiting) != 0 {
		t.Fatalf("this binary's run: %+v %v", res, s.feed("src").Waiting)
	}
}

// The relay's scope: a body released only in a channel the caller holds no feed on is answered as
// one without documents, on the feed route and on the content route.
func TestRelayScopeByChannel(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a) // feed on prod only
	tn, _ := a.st.GetTenant(ctx, "acme")
	elsewhere := strings.Repeat("c", 64)
	if err := a.st.InTx(ctx, "", func(tx *store.Tx) error {
		ch := store.Channel{TenantID: tn.ID, Name: "staging", Kind: store.ChannelSigned}
		if err := tx.CreateChannel(ctx, &ch); err != nil {
			return err
		}
		b := store.Build{TenantID: tn.ID, Name: "tresor", PackageVersion: "2.0", Platform: "linux_amd64", ABI: store.ABICPP, DuckDBVersion: "v2.0.0",
			BodyHash: elsewhere, Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
		if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
			return err
		}
		r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: "tresor", PackageVersion: b.PackageVersion, Platform: b.Platform,
			Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 1, Origin: store.OriginAdmin}
		return tx.InsertRelease(ctx, &r, admin.String())
	}); err != nil {
		t.Fatal(err)
	}
	a.src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)
	a.src.offer(elsewhere, "r2", []byte(reviewDoc), nil, nil)
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export, sp.Channels = true, []string{"prod", "staging"} })
	if res := a.run("src"); res.Counts["received"] != 2 {
		t.Fatalf("the relay's own run: %+v", res)
	}
	ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}, {BodyHash: elsewhere}}, srcURL)
	if err != nil || seqs(t, ans) != "1" || len(ans.Bodies) != 1 || ans.Bodies[0].BodyHash != a.body {
		t.Fatalf("the answer: %q %+v %v", seqs(t, ans), ans.Bodies, err)
	}
	read := func(id string) error {
		return a.svc.FeedDocument(ctx, downstream, "acme", id, srcURL, func(store.Attachment, []byte) error { return nil })
	}
	if err := read("r1"); err != nil {
		t.Fatalf("a document in scope: %v", err)
	}
	if err := read("r2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a document of a body released only where the caller holds no feed: %v", err)
	}
}
