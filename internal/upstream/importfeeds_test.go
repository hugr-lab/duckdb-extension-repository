package upstream_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0014 phase 1b, round trips: a gateway's export feed holds statements about a mirrored body;
// the site's bundle-mode feed takes them in with the file the same bundle brings (a body of its
// sources, from 0). The site judges each statement itself: a refusal at the gateway comes without
// content and is refused here; a withdrawal follows in the next bundle; a dry run stores nothing;
// a reset makes the old bundle's range a gap; a damaged batch fails the feed only.
func TestImportFeeds(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			feeds := g.feeds()
			feeds.Log = slog.New(slog.DiscardHandler)
			g.up.Feeds = feeds

			// the gateway mirrors tresor; the site plans it through "air"
			g.put(t, "tresor", "1.0", 1)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			body := g.cell(t, "tresor").BodyHash

			// the gateway's export feed of the source: its keys file, what it holds about the body
			ak, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			src := source{t: t, key: ak, fp: extfile.Fingerprint(&ak.PublicKey)}
			pemStr, _ := extfile.MarshalPublicKeyPEM(&ak.PublicKey)
			keysFile, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": src.fp, "public_key": pemStr}}})
			gwT, _ := g.st.GetTenant(ctx, "acme")
			gw := store.Feed{TenantID: gwT.ID, Name: "reviews", URL: feedURL, Credential: "src", Keys: []string{src.fp},
				Offer: store.OfferUpstreams, Export: true, CreatedBy: "os:1:t"}
			doc := []byte(`{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest",` +
				`"capabilities":[],"network":[],"files":[],"processes":[],"licences":["MIT"],"findings":[],"text":"# Fine"}`)
			b1, sig1, st1 := src.statement(1, "d1", body, doc, nil)
			// its content differs from d1's: the bundle carries none for it (content travels by hash)
			rb, rsig, _ := src.statement(2, "d2", body, append(bytes.Clone(doc[:len(doc)-1]), ' ', '}'), nil)
			d1 := store.Attachment{ID: store.NewID(), TenantID: gwT.ID, BodyHash: body, Kind: st1.Kind, MediaType: st1.MediaType,
				Title: st1.Title, Size: st1.Size, ContentHash: st1.ContentHash, Summary: "{}", Origin: store.AttachedByFeed,
				Visibility: store.VisibleAdministrators, Issuer: feedURL, IssuerID: "d1", KeyFP: src.fp, SourceOrigin: "tenant",
				IssuedAt: time.Now(), Statement: string(b1), Signature: sig1, Seq: 1}
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					if err := tx.InsertFeed(ctx, &gw); err != nil {
						return err
					}
					if err := tx.SetFeedKeysFile(ctx, gw.ID, keysFile); err != nil {
						return err
					}
					if err := tx.SetFeedCursor(ctx, gw.ID, body, 10); err != nil {
						return err
					}
					d1.AttachedBy = gw.ID
					if err := tx.InsertAttachment(ctx, &d1); err != nil {
						return err
					}
					// a refusal local to the gateway (its quota): the site judges it again, without content
					return tx.KeepFeedRefusal(ctx, gwT.ID, store.FeedRefusal{FeedID: gw.ID, BodyHash: body, Seq: 2, Reason: "limits",
						Statement: string(rb), Signature: rsig})
				})
			})
			if err := g.blob.PutAttachment(ctx, gwT.StorageDomain, d1.ID, doc); err != nil {
				t.Fatal(err)
			}

			// the site's bundle-mode feed of the URL
			if _, err := feeds.Add(ctx, admin, "site", attachment.FeedSpec{Name: "enterest", URL: feedURL, Keys: []string{src.fp},
				Mode: store.FeedBundle}); err != nil {
				t.Fatal(err)
			}
			siteT, _ := g.st.GetTenant(ctx, "site")
			siteFeed := func() store.Feed {
				f, err := getFeed(t, g.env, siteT.ID, "enterest")
				if err != nil {
					t.Fatal(err)
				}
				return f
			}
			plan := g.plan(t)
			if len(plan.Feeds) != 1 || len(plan.Feeds[0].Sources) != 1 || len(plan.Feeds[0].Bodies) != 0 {
				t.Fatalf("the plan's feed: %+v", plan.Feeds)
			}
			res, data := g.export(t, plan, upstream.ExportOptions{})
			if res.Failed() || res.Feeds[0].Counts["statements"] != 2 {
				t.Fatalf("export: %+v", res.Feeds)
			}
			feedTarget := func(r upstream.ImportResult) upstream.TargetResult {
				t.Helper()
				for _, tr := range r.Targets {
					if tr.Feed == "enterest" {
						return tr
					}
				}
				t.Fatalf("no feed target: %+v", r.Targets)
				return upstream.TargetResult{}
			}
			siteDocs := func() []store.Attachment {
				docs, err := g.st.BodyAttachments(ctx, siteT.ID, body)
				if err != nil {
					t.Fatal(err)
				}
				return docs
			}

			// a dry run judges everything, the body the bundle brings as held, and stores nothing
			dry, err := g.up.Import(ctx, admin, "site", bytes.NewReader(data), upstream.ImportOptions{BundleKeys: []string{g.fp}, Holder: "test",
				DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if tr := feedTarget(dry); tr.Status != upstream.TargetImported || tr.FeedRun.Counts["would_store"] != 1 || len(siteDocs()) != 0 ||
				siteFeed().BundleID != "" {
				t.Fatalf("dry run: %+v %+v", tr, tr.FeedRun)
			}

			// the import: the file, then the review; the refusal judged here, without content
			r := g.importSite(t, data)
			tr := feedTarget(r)
			if tr.Status != upstream.TargetImported || tr.FeedRun.Counts["received"] != 1 || len(tr.FeedRun.Refused) != 1 ||
				tr.FeedRun.Refused[0].Reason != "bundle: no content (limits)" {
				t.Fatalf("import: %+v %+v", tr, tr.FeedRun)
			}
			if d := siteDocs(); len(d) != 1 || d[0].IssuerID != "d1" || d[0].Issuer != feedURL {
				t.Fatalf("the site's documents: %+v", d)
			}
			f := siteFeed()
			cur, _ := g.st.FeedCursors(ctx, f.ID)
			if f.BundleID != res.ID || f.Status != store.FeedOK || cur[body] != 2 {
				t.Fatalf("the feed: %+v, cursor %v", f, cur) // no further than a statement verified: 2, not the gateway's 10
			}
			// the same bundle again: nothing new
			if r := g.importSite(t, data); feedTarget(r).FeedRun.Counts["received"] != 0 {
				t.Fatalf("again: %+v", feedTarget(r).FeedRun)
			}

			// the review withdrawn at the source: the next bundle carries the withdrawal
			wb, wsig, _ := src.statement(3, "d1", body, nil, withdrawal)
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					cur, err := tx.GetAttachment(ctx, gwT.ID, d1.ID)
					if err != nil {
						return err
					}
					return tx.WithdrawFeedAttachment(ctx, &cur, 3, string(wb), wsig)
				})
			})
			plan = g.plan(t)
			if plan.Feeds[0].Bodies[0].Seq != 2 {
				t.Fatalf("the second plan: %+v", plan.Feeds[0])
			}
			_, data2 := g.export(t, plan, upstream.ExportOptions{})
			r = g.importSite(t, data2)
			if tr := feedTarget(r); tr.FeedRun.Counts["withdrawn"] != 1 || siteDocs()[0].WithdrawnAt.IsZero() {
				t.Fatalf("the withdrawal: %+v", tr.FeedRun)
			}

			// a damaged batch fails the feed only: its cursors stay
			at := bytes.Index(data2, []byte(`[{"statement":"`))
			bad := bytes.Clone(data2)
			bad[at+20] ^= 1
			if _, err := g.up.ResetBundle(ctx, admin, "site", "air"); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.ResetFeedBundle(ctx, admin, "site", "enterest"); err != nil {
				t.Fatal(err)
			}
			r, err = g.up.Import(ctx, admin, "site", bytes.NewReader(bad), upstream.ImportOptions{BundleKeys: []string{g.fp}, Holder: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if tr := feedTarget(r); tr.Status != upstream.TargetFailed || !strings.Contains(tr.Detail, "differs") {
				t.Fatalf("damaged: %+v", tr)
			}
			// after the reset the second bundle's range (from 2) does not start at the feed's cursor (0): a gap
			r = g.importSite(t, data2)
			if r.Gaps != 1 || feedTarget(r).Status != upstream.TargetImported {
				t.Fatalf("a gap: %+v", r)
			}
			if c, _ := g.st.FeedCursors(ctx, f.ID); c[body] != 0 {
				t.Fatalf("a gap moved the cursor: %v", c)
			}
		})
	}
}
