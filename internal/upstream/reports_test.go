package upstream_test

import (
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

// Spec 0045, the last leg: an own kista holds a report it took through a relaying tenant (a feed
// with via) and hands it on (export); an air-gapped site plans, the own kista exports a bundle, and
// the site's bundle-mode feed takes the report in, verified against the issuer's key the site
// pinned. The issuer's URL is the feed's identity at every hop; nothing contacts it.
func TestOwnKistaBundleSite(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			feeds := g.feeds()
			feeds.Log = slog.New(slog.DiscardHandler)
			g.up.Feeds = feeds
			g.put(t, "tresor", "1.0", 1)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			body := g.cell(t, "tresor").BodyHash

			ak, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			src := source{t: t, key: ak, fp: extfile.Fingerprint(&ak.PublicKey)}
			pemStr, _ := extfile.MarshalPublicKeyPEM(&ak.PublicKey)
			keysFile, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": src.fp, "public_key": pemStr}}})
			report := func(issue int) []byte {
				b, _ := json.Marshal(map[string]any{"envelope": "kista.report.v1", "report": "rep-1", "issue": issue, "issued_at": "2026-11-16T10:00:00Z",
					"depth": "static", "subject": map[string]any{"bodies": []any{map[string]any{"hash": body, "read": true}}},
					"attention": []any{map[string]any{"kind": "network", "count": 2}}, "html": "<h1>Report</h1>"})
				return b
			}
			asReport := func(st *attachment.Statement) {
				st.Kind, st.MediaType, st.Title = store.KindReport, attachment.Report, "Report"
			}
			doc := report(1)
			b1, sig1, st1 := src.statement(1, "p1", body, doc, asReport)

			// the own kista's feed: the issuer's URL, taken through its organisation's tenant, handed on
			own, _ := g.st.GetTenant(ctx, "acme")
			fd := store.Feed{TenantID: own.ID, Name: "reports", URL: feedURL, Via: "https://relay.example/api/v1/tenants/org", Credential: "src",
				Keys: []string{src.fp}, Offer: store.OfferUpstreams, Export: true, CreatedBy: "os:1:t"}
			p1 := store.Attachment{ID: store.NewID(), TenantID: own.ID, BodyHash: body, Kind: st1.Kind, MediaType: st1.MediaType, Title: st1.Title,
				Size: st1.Size, ContentHash: st1.ContentHash, Summary: "{}", Origin: store.AttachedByFeed, Visibility: store.VisibleAdministrators,
				Issuer: feedURL, IssuerID: "p1", KeyFP: src.fp, SourceOrigin: "tenant", IssuedAt: time.Now(), Statement: string(b1), Signature: sig1, Seq: 1}
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					if err := tx.InsertFeed(ctx, &fd); err != nil {
						return err
					}
					if err := tx.SetFeedKeysFile(ctx, fd.ID, keysFile); err != nil { // as a via run keeps the relay's
						return err
					}
					if err := tx.SetFeedCursor(ctx, fd.ID, body, 1); err != nil {
						return err
					}
					p1.AttachedBy = fd.ID
					return tx.InsertAttachment(ctx, &p1)
				})
			})
			if err := g.blob.PutAttachment(ctx, own.StorageDomain, p1.ID, doc); err != nil {
				t.Fatal(err)
			}
			// the default offer names the mirrored body, so the feed would have asked about it
			if offered, err := g.st.OfferedBodies(ctx, fd); err != nil || len(offered) != 1 || offered[0] != body {
				t.Fatalf("the own kista's offer: %v %v", offered, err)
			}

			// the site: a bundle-mode feed of the issuer, pinned to the issuer's key
			if _, err := feeds.Add(ctx, admin, "site", attachment.FeedSpec{Name: "reports", URL: feedURL, Keys: []string{src.fp}, Mode: store.FeedBundle}); err != nil {
				t.Fatal(err)
			}
			site, _ := g.st.GetTenant(ctx, "site")
			plan := g.plan(t)
			res, data := g.export(t, plan, upstream.ExportOptions{})
			if res.Failed() || len(res.Feeds) != 1 || res.Feeds[0].Counts["statements"] != 1 || res.Feeds[0].Counts["documents"] != 1 {
				t.Fatalf("the own kista's export: %+v", res.Feeds)
			}
			r := g.importSite(t, data)
			var run attachment.RunResult
			for _, tr := range r.Targets {
				if tr.Feed == "reports" {
					if tr.Status != upstream.TargetImported {
						t.Fatalf("the site's feed target: %+v", tr)
					}
					run = *tr.FeedRun
				}
			}
			docs, err := g.st.BodyAttachments(ctx, site.ID, body)
			if err != nil || run.Counts["received"] != 1 || len(docs) != 1 || docs[0].Kind != store.KindReport || docs[0].Issuer != feedURL ||
				docs[0].KeyFP != src.fp || docs[0].Statement != string(b1) || !strings.Contains(docs[0].Summary, `"issue":1`) ||
				!strings.Contains(docs[0].Summary, `"read":true`) {
				t.Fatalf("the report at the site: %+v %+v %v", run, docs, err)
			}
		})
	}
}
