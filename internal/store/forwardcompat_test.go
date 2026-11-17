package store_test

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0046 on every dialect: a cursor's waiting state (the lowest seq, the count, the last reason,
// the lower level), the rewind (the one move back, with the kept refusals of the rewound range), a
// reset deleting the state with the cursor, and an attachment of a kind this binary does not know
// stored and read as it is (no CHECK on the kind).
func TestForwardCompat(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		f := store.Feed{TenantID: tn.ID, Name: "src", URL: "https://src.example/api/v1/tenants/x", Credential: "c", Offer: store.OfferUpstreams,
			Keys: []string{"sha256:" + strings.Repeat("9", 64)}, Export: true, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &f) })
		a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
		wait := func(body string, seq int64, why string, level int) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.SetFeedCursor(ctx, f.ID, body, seq); err != nil {
					return err
				}
				return tx.AddFeedWait(ctx, f.ID, store.FeedWait{BodyHash: body, From: seq, Count: 1, Why: why, Level: level})
			})
		}
		waits := func() map[string]store.FeedWait {
			t.Helper()
			w, err := s.FeedWaits(ctx, f.ID)
			if err != nil {
				t.Fatal(err)
			}
			return w
		}
		cursor := func(body string) int64 {
			t.Helper()
			cur, err := s.FeedCursors(ctx, f.ID)
			if err != nil {
				t.Fatal(err)
			}
			return cur[body]
		}

		// a cursor without waiting state: the defaults
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, f.ID, b, 3) })
		if w := waits(); len(w) != 0 {
			t.Fatalf("a plain cursor waits: %+v", w)
		}

		// an older replica waited at 15 (level 2), the newer one waits at 12 (level 3), then at 20 for
		// another reason: the lowest seq, the count, the last reason, the lower level
		wait(a, 15, "unknown kind", 2)
		wait(a, 15, "unknown kind", 2) // the cursor is at 15 already: the wait is still counted
		inTx(t, s, func(tx *store.Tx) error {
			return tx.AddFeedWait(ctx, f.ID, store.FeedWait{BodyHash: a, From: 12, Count: 1, Why: "newer", Level: 3})
		})
		wait(a, 20, "media type", 3)
		if w := waits()[a]; w.From != 12 || w.Count != 4 || w.Why != "media type" || w.Level != 2 || cursor(a) != 20 {
			t.Fatalf("the waiting state: %+v, cursor %d", w, cursor(a))
		}
		shown := func() store.FeedWaiting {
			t.Helper()
			var out map[string]store.FeedWaiting
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				out, err = tx.FeedsWaiting(ctx, tn.ID)
				return err
			})
			return out[f.ID]
		}
		if w := shown(); w.Own["media type"] != 4 || len(w.Own) != 1 || len(w.Source) != 0 {
			t.Fatalf("waiting by feed and reason: %+v", w)
		}

		// what the feed kept for export of the body: below and within the rewound range, and another body's
		for _, r := range []store.FeedRefusal{{BodyHash: a, Seq: 11, Reason: "title"}, {BodyHash: a, Seq: 12, Reason: "newer"},
			{BodyHash: a, Seq: 20, Reason: "media type"}, {BodyHash: b, Seq: 13, Reason: "kind"}} {
			r.FeedID, r.Statement, r.Signature = f.ID, "{}", "s"
			inTx(t, s, func(tx *store.Tx) error { return tx.KeepFeedRefusal(ctx, tn.ID, r) })
		}

		// a binary of the same or a lower level retakes nothing
		retake := func(level int) int {
			t.Helper()
			var n int
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				n, err = tx.RetakeFeedCursors(ctx, f.ID, level)
				return err
			})
			return n
		}
		if n := retake(2); n != 0 || cursor(a) != 20 || waits()[a].Count != 4 {
			t.Fatalf("a retake at the row's level: %d bodies, cursor %d", n, cursor(a))
		}
		if n := retake(1); n != 0 || cursor(a) != 20 {
			t.Fatalf("a retake below the row's level: %d bodies, cursor %d", n, cursor(a))
		}
		// a higher level rewinds to just below the lowest seq passed over, and clears the state
		if n := retake(3); n != 1 || cursor(a) != 11 || len(waits()) != 0 || cursor(b) != 3 {
			t.Fatalf("the retake: %d bodies, cursor %d, waits %+v", n, cursor(a), waits())
		}
		for body, want := range map[string][]int64{a: {11}, b: {13}} {
			rs, err := s.FeedRefusals(ctx, f.ID, body, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, r := range rs {
				got = append(got, r.Seq)
			}
			if len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("kept refusals of %s after the retake: %v, want %v", body[:4], got, want)
			}
		}
		// the cursor moves forward again from there
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, f.ID, a, 12) })
		if cursor(a) != 12 {
			t.Fatalf("the cursor after the retake: %d", cursor(a))
		}

		// level 0 (a statement carried without content): asked for again by any binary. The cursor
		// goes back; the state stays for display, apart from this kista's own, until the range is
		// judged again
		wait(b, 9, "unknown kind", 0)
		if w := shown(); w.Source["unknown kind"] != 1 || len(w.Own) != 0 {
			t.Fatalf("a level 0 wait shown: %+v", w)
		}
		if n := retake(1); n != 1 || cursor(b) != 8 || waits()[b].From != 9 || waits()[b].Count != 1 {
			t.Fatalf("a level 0 wait: %d bodies, cursor %d, %+v", n, cursor(b), waits()[b])
		}
		if n := retake(1); n != 0 || cursor(b) != 8 { // rewound already: nothing more to take back
			t.Fatalf("a level 0 wait retaken twice: %d bodies, cursor %d", n, cursor(b))
		}
		// the cursor moving on below the waiting seq keeps the state; reaching it clears it, and what
		// still waits is recorded anew
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, f.ID, b, 8) })
		if w := waits()[b]; w.From != 9 {
			t.Fatalf("the state below the waiting seq: %+v", w)
		}
		wait(b, 9, "newer", 0)
		if w := waits()[b]; w.From != 9 || w.Count != 1 || w.Why != "newer" || w.Level != 0 || cursor(b) != 9 {
			t.Fatalf("the state judged again: %+v, cursor %d", w, cursor(b))
		}
		if n := retake(1); n != 1 || cursor(b) != 8 {
			t.Fatalf("the next retake: %d bodies, cursor %d", n, cursor(b))
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, f.ID, b, 12) }) // the source no longer offers it
		if w := waits(); len(w) != 0 || cursor(b) != 12 {
			t.Fatalf("a rewound state the cursor passed: %+v", w)
		}

		// a reset deletes the waiting state with the cursor
		wait(a, 30, "newer", 1)
		inTx(t, s, func(tx *store.Tx) error { return tx.ResetFeedCursors(ctx, f.ID) })
		if w := waits(); len(w) != 0 || cursor(a) != 0 {
			t.Fatalf("after a reset: %+v, cursor %d", w, cursor(a))
		}
		wait(a, 5, "newer", 1) // a new row starts clean
		if w := waits()[a]; w.From != 5 || w.Count != 1 || w.Level != 1 {
			t.Fatalf("a wait after a reset: %+v", w)
		}

		// an attachment of a sixth kind is stored and read as it is
		bd := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &bd) })
		doc := store.Attachment{TenantID: tn.ID, BodyHash: bd.BodyHash, Kind: "report", MediaType: "application/vnd.example.report+json",
			Title: "R", Size: 1, ContentHash: strings.Repeat("0", 64), Summary: `{}`, Origin: store.AttachedByTenant, AttachedBy: "os:1:t",
			Visibility: store.VisibleReaders}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &doc) })
		got, err := s.GetAttachment(ctx, tn.ID, doc.ID)
		if err != nil || got.Kind != "report" || got.MediaType != doc.MediaType {
			t.Fatalf("a row of a sixth kind: %+v %v", got, err)
		}

		// narrowing: to administrators, marked, once; a readable newer statement clears the mark
		narrow := func(a *store.Attachment) (changed bool) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) (err error) { changed, err = tx.NarrowFeedAttachment(ctx, a); return })
			return changed
		}
		if got.Narrowed || !narrow(&got) {
			t.Fatalf("not narrowed: %+v", got)
		}
		again, err := s.GetAttachment(ctx, tn.ID, doc.ID)
		if err != nil || again.Visibility != store.VisibleAdministrators || !again.Narrowed || again.Version != doc.Version+1 {
			t.Fatalf("narrowed: %+v %v", again, err)
		}
		if narrow(&again) {
			t.Fatal("narrowed twice")
		}
		if last, _ := s.GetAttachment(ctx, tn.ID, doc.ID); last.Version != again.Version {
			t.Fatalf("narrowing a narrowed document changed it: %d, was %d", last.Version, again.Version)
		}
		// a document an administrator had narrowed already is marked all the same
		inTx(t, s, func(tx *store.Tx) error {
			return tx.SetAttachmentFlags(ctx, &again, store.VisibleAdministrators, false, 0)
		})
		upd := again
		upd.Seq, upd.Statement, upd.Signature, upd.Visibility = 9, `{"seq":9}`, "c2ln", store.VisibleReaders
		inTx(t, s, func(tx *store.Tx) error { return tx.ReissueFeedAttachment(ctx, &upd) })
		if last, _ := s.GetAttachment(ctx, tn.ID, doc.ID); last.Narrowed || last.Seq != 9 || last.Visibility != store.VisibleAdministrators {
			t.Fatalf("after a readable newer statement: %+v", last)
		}
	})
}
