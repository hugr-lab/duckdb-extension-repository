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

// A tenant hands on at most one feed per issuer (spec 0045): export is refused for a second feed
// of the issuer, at its creation and at a change, naming the feed that hands the issuer on. The
// relay then answers from that one feed: a statement it refused for its kinds is "not held", though
// another feed of the issuer stored the document, and one it waits on carries the waiting reason.
func TestOneExportFeedPerIssuer(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	src := a.src
	src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)                                                                                  // 1
	src.offer(a.body, "p1", reportJSON(1, a.body), asReport, nil)                                                                         // 2: narrow leaves the kind out
	src.offer(a.body, "a1", []byte(`{}`), func(st *attachment.Statement) { st.Kind, st.MediaType = "audit", "application/x-audit" }, nil) // 3: a kind not known here
	a.add("narrow", func(sp *attachment.FeedSpec) { sp.Export, sp.Kinds = true, []string{store.KindReview} })
	spec := attachment.FeedSpec{Name: "wide", URL: srcURL, Credential: "src", Keys: []string{src.fp}, Offer: store.OfferChannels, Channels: []string{"prod"}, Export: true}
	_, err := a.feeds.Add(ctx, admin, "acme", spec)
	if !errors.Is(err, store.ErrInvalid) || !errors.Is(err, store.ErrHandedOn) || !strings.Contains(err.Error(), `the feed "narrow" hands `+srcURL+` on already`) {
		t.Fatalf("a second export feed of the issuer: %v", err)
	}
	a.add("wide", nil) // without export it is any other feed of the issuer
	on, off := true, false
	if _, err := a.feeds.Change(ctx, admin, "acme", "wide", attachment.FeedChange{Export: &on}, 0); !errors.Is(err, store.ErrHandedOn) || a.feed("wide").Export {
		t.Fatalf("export set on a second feed of the issuer: %v", err)
	}
	// a feed of another issuer hands on freely
	a.add("other", func(sp *attachment.FeedSpec) { sp.URL, sp.Export = "https://src.example/api/v1/tenants/other", true })

	if res := a.run("narrow"); res.Counts["received"] != 1 || reasons(res)["kind"] != 2 {
		t.Fatalf("narrow's run: %+v", res)
	}
	if res := a.run("wide"); res.Counts["received"] != 1 || res.Counts["waiting"] != 1 {
		t.Fatalf("wide's run: %+v", res)
	}
	ask := func() string {
		t.Helper()
		ans, err := a.svc.Feed(ctx, downstream, "acme", []attachment.FeedBody{{BodyHash: a.body}}, srcURL)
		if err != nil {
			t.Fatal(err)
		}
		return seqs(t, ans)
	}
	if got := ask(); got != "1 2!"+attachment.NoteNotHeld+" 3!"+attachment.NoteNotHeld {
		t.Fatalf("answered from the one feed that hands on: %q", got)
	}
	if err := a.svc.FeedDocument(ctx, downstream, "acme", "p1", srcURL, func(store.Attachment, []byte) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("content of a kind the export feed does not take: %v", err)
	}
	// the tenant moves handing on to the feed that takes every kind: one clears, the other sets
	if _, err := a.feeds.Change(ctx, admin, "acme", "narrow", attachment.FeedChange{Export: &off}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.feeds.Change(ctx, admin, "acme", "wide", attachment.FeedChange{Export: &on}, 0); err != nil {
		t.Fatalf("export on the other feed once the first gave it up: %v", err)
	}
	a.run("wide") // it reads the history again, and keeps what it waits on
	if got := ask(); got != "1 2 3!"+attachment.WaitKind {
		t.Fatalf("answered from the feed that takes every kind: %q", got)
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
	if ask(); !a.feed("src").RelayStoppedAt.IsZero() || attachment.RelayStopped(a.feed("src")) != "" {
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
	if got.RelayStoppedAt.IsZero() || !strings.Contains(attachment.RelayStopped(got), "ended at a statement that does not verify") {
		t.Fatalf("the feed's record: %v %q", got.RelayStoppedAt, attachment.RelayStopped(got))
	}
	// new pins: said anew by the next answer
	pins := []string{a.src.fp, "sha256:" + strings.Repeat("1", 64)}
	if f, err := a.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Keys: &pins}, 0); err != nil || !f.RelayStoppedAt.IsZero() || !a.feed("src").RelayStoppedAt.IsZero() {
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

// Where handing on stops early is recorded on the feed at most once a minute, whatever bodies a
// caller asks about; a change of the feed's pins clears it, and an answer that read the feed before
// the change does not write it back.
func TestRelayStoppedOncePerMinute(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)
	a.src.offer(a.other, "r2", []byte(reviewDoc), nil, nil)
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	a.run("src")
	for _, d := range a.docs() { // neither body's statement verifies any more
		if err := store.ExecRaw(ctx, a.st, "UPDATE attachments SET statement = ? WHERE id = ?", strings.Replace(d.Statement, `"title":"Review"`, `"title":"Other"`, 1), d.ID); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Now().UTC().Truncate(time.Second)
	a.st.Now = func() time.Time { return clock }
	ask := func(bodies ...string) {
		t.Helper()
		var req []attachment.FeedBody
		for _, b := range bodies {
			req = append(req, attachment.FeedBody{BodyHash: b})
		}
		if ans, err := a.svc.Feed(ctx, downstream, "acme", req, srcURL); err != nil || len(ans.Statements) != 0 {
			t.Fatalf("the answer: %+v %v", ans, err)
		}
	}
	ask(a.body)
	first := a.feed("src").RelayStoppedAt
	if first.IsZero() || attachment.RelayStopped(a.feed("src")) == "" {
		t.Fatalf("not recorded: %v", first)
	}
	// callers with other sets of bodies, within the minute: nothing is written again
	for _, bodies := range [][]string{{a.body, a.other}, {a.other}, {a.body}} {
		clock = clock.Add(10 * time.Second)
		ask(bodies...)
		if at := a.feed("src").RelayStoppedAt; !at.Equal(first) {
			t.Fatalf("written again within a minute for %d bodies: %v, was %v", len(bodies), at, first)
		}
	}
	clock = clock.Add(2 * time.Minute)
	ask(a.other)
	if at := a.feed("src").RelayStoppedAt; !at.After(first) {
		t.Fatalf("not said again after a minute: %v", at)
	}
	// the pins change while an answer is being read: the answer does not write the record back
	pins := []string{a.src.fp, "sha256:" + strings.Repeat("1", 64)}
	clock = clock.Add(2 * time.Minute)
	a.svc.AfterRelayRead(func() {
		if _, err := a.feeds.Change(ctx, admin, "acme", "src", attachment.FeedChange{Keys: &pins}, 0); err != nil {
			t.Fatal(err)
		}
	})
	ask(a.body)
	a.svc.AfterRelayRead(nil)
	if got := a.feed("src"); !got.RelayStoppedAt.IsZero() || attachment.RelayStopped(got) != "" {
		t.Fatalf("written back after the pins changed: %v", got.RelayStoppedAt)
	}
}

// An issuer's keys file that leaves no room for a statement in an answer is said on the relaying
// feed's record, as an answer that ends early is, and the downstream's error covers it.
func TestRelayKeysOverBoundIsShown(t *testing.T) {
	a := newSubscriber(t)
	relayOf(a)
	a.svc.FeedAnswerMax = attachment.MinFeedAnswer
	a.src.keysPad = strings.Repeat("<", 60<<10)
	a.src.offer(a.body, "r1", []byte(reviewDoc), nil, nil)
	a.add("src", func(sp *attachment.FeedSpec) { sp.Export = true })
	a.run("src")
	b := newSubscriber(t)
	b.feeds.Fetch = &throughRelay{t: t, relay: a.svc}
	b.add("own", func(sp *attachment.FeedSpec) { viaFeed(sp); sp.Keys = []string{a.src.fp} })
	res := b.run("own")
	if !strings.Contains(res.Error, "no room for the issuer's keys") || b.feed("own").Status != store.FeedFailing {
		t.Fatalf("the downstream's run: %+v", res)
	}
	if got := a.feed("src"); got.RelayStoppedAt.IsZero() || attachment.RelayStopped(got) == "" {
		t.Fatalf("the relaying feed's record: %v %q", got.RelayStoppedAt, attachment.RelayStopped(got))
	}
}

// At the quota, a statement that would need no record is judged as ever: one without content, and
// one over its issuer's heads limit, are refused for good and the cursor moves; only a document
// that would be stored holds its body.
func TestQuotaJudgesWhatNeedsNoRecord(t *testing.T) {
	s := newViaSubscriber(t)
	wasMax, wasHeads := store.MaxTenantAttachments, store.MaxAttachmentHeads
	defer func() { store.MaxTenantAttachments, store.MaxAttachmentHeads = wasMax, wasHeads }()
	store.MaxTenantAttachments, store.MaxAttachmentHeads = 1, 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)         // 1: stored, the quota is full
	gone := s.src.offer(s.body, "r2", []byte(reviewDoc), nil, nil) // 2: not held by the relay
	s.src.offer(s.body, "r3", []byte(reviewDoc), nil, nil)         // 3: a second head of its kind on the body
	s.src.offer(s.other, "r4", []byte(reviewDoc), nil, nil)        // 4: would be stored: held
	s.src.notes[gone] = attachment.NoteNotHeld
	f := s.add("own", viaFeed)
	res := s.run("own")
	if res.Counts["received"] != 1 || reasons(res)["no content"] != 1 || reasons(res)["limits"] != 1 || res.Counts["held"] != 1 ||
		s.cursor(f, s.body) != 3 || s.cursor(f, s.other) != 0 || s.src.downloads != 1 {
		t.Fatalf("the run at the quota: %+v, cursors %d %d, %d downloads", res, s.cursor(f, s.body), s.cursor(f, s.other), s.src.downloads)
	}
}

// The same from a bundle: a statement the bundle carries no content for is refused at the quota,
// not held; and a dry run says so, counts a newer statement about a document held as an update,
// and counts its room once for the tenant, across the bundle's feeds.
func TestBundleQuotaJudgesWhatNeedsNoRecord(t *testing.T) {
	s := newSubscriber(t)
	was := store.MaxTenantAttachments
	defer func() { store.MaxTenantAttachments = was }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil) // 1
	s.src.offer(s.body, "r2", []byte(`{"not":"carried"}`), nil, nil)
	delete(s.src.docs, "r2") // 2: the bundle carries no content for it
	fi, fd := s.bundleImport("air", 0, 2, false, map[int64]string{2: "limits"})
	res, err := fi.Finish(ctx, "os:1:t", true)
	if err != nil || fi.Failed() != nil || res.Counts["received"] != 1 || res.Counts["held"] != 0 || reasons(res)["bundle: no content"] != 1 {
		t.Fatalf("the import at the quota: %+v %v %v", res, err, fi.Failed())
	}
	if cur, _ := s.st.FeedCursors(ctx, fd.ID); cur[s.body] != 2 {
		t.Fatalf("the cursor: %v", cur)
	}
	// a dry run, the quota full: a newer statement about the document held is no hold, a statement
	// without content is refused, a new document would be held
	s.src.offer(s.body, "r1", []byte(reviewDoc), func(st *attachment.Statement) { st.Audience = "administrators" }, nil) // 3: r1 again
	s.src.offer(s.body, "r5", []byte(reviewDoc+" "), nil, nil)                                                           // 4: new
	fi, _ = s.bundleImport("air", 2, 4, true, nil)
	res, _ = fi.Finish(ctx, "os:1:t", true)
	if res.Counts["would_update"] != 1 || res.Counts["would_hold"] != 1 || res.Counts["would_store"] != 0 {
		t.Fatalf("the dry run at the quota: %+v", res)
	}
	// room for one: two feeds of the bundle, each bringing a new document, share it
	store.MaxTenantAttachments = 2
	room := &attachment.QuotaRoom{}
	one, _ := s.bundleImport("air", 2, 4, true, nil)
	one.ShareRoom(room)
	r1, _ := one.Finish(ctx, "os:1:t", true)
	two, _ := s.bundleImport("air2", 0, 4, true, nil)
	two.ShareRoom(room)
	r2, _ := two.Finish(ctx, "os:1:t", true)
	if r1.Counts["would_store"] != 1 || r2.Counts["would_store"] != 0 || r2.Counts["would_hold"] != 1 {
		t.Fatalf("two feeds' dry runs with room for one: %+v %+v", r1, r2)
	}
}

// A run with a transient failure and a quota hold is tried again soon, for the failure: only a
// run that ended on the limit alone waits for the feed's interval.
func TestQuotaAndFailureRetrySoon(t *testing.T) {
	s := newSubscriber(t)
	s.feeds.Interval = time.Hour
	was := store.MaxTenantAttachments
	defer func() { store.MaxTenantAttachments = was }()
	store.MaxTenantAttachments = 1
	s.src.offer(s.body, "r1", []byte(reviewDoc), nil, nil)  // fails to download for now
	s.src.offer(s.other, "r2", []byte(reviewDoc), nil, nil) // stored
	s.src.offer(s.other, "r3", []byte(reviewDoc), nil, nil) // over the quota
	s.src.down = map[string]bool{"r1": true}
	s.add("src", nil)
	res := s.run("src")
	got := s.feed("src")
	if res.Counts["failed"] != 1 || res.Counts["held"] != 1 || got.Status != store.FeedFailing || time.Until(got.NextRunAt) > 10*time.Minute {
		t.Fatalf("the run: %+v, next in %s", res, time.Until(got.NextRunAt))
	}
}
