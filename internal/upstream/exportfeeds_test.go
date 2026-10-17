package upstream_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

type attSigner struct{ k *rsa.PrivateKey }

func (s attSigner) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.k, crypto.SHA256, h[:])
}
func (s attSigner) Public() *rsa.PublicKey { return &s.k.PublicKey }
func (s attSigner) ID() string             { return "test" }

const feedURL = "https://src.example.org/api/v1/tenants/reviews"

// source signs statements as a feed source would.
type source struct {
	t   *testing.T
	key *rsa.PrivateKey
	fp  string
}

func (s source) statement(seq int64, id, body string, content []byte, mod func(*attachment.Statement)) ([]byte, string, attachment.Statement) {
	st := attachment.Statement{Issuer: feedURL, Key: s.fp, IssuerID: id, Seq: seq, BodyHash: body, Kind: store.KindReview,
		MediaType: attachment.Review, ContentHash: blob.ContentHash(content), Size: int64(len(content)), Title: "Review",
		IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "administrators"}
	if mod != nil {
		mod(&st)
	}
	b, err := st.Canonical()
	if err != nil {
		s.t.Fatal(err)
	}
	sig, err := attachment.Sign(context.Background(), attSigner{s.key}, b)
	if err != nil {
		s.t.Fatal(err)
	}
	return b, base64.StdEncoding.EncodeToString(sig), st
}

func withdrawal(st *attachment.Statement) {
	st.Withdrawn, st.Kind, st.MediaType, st.ContentHash, st.Size, st.Title = true, "", "", "", 0, ""
}

// Spec 0014 phase 1b, the export of feeds: the gateway's export feed chosen by kinds and keys file,
// each body's range from the plan's cursor to the gateway's; stored statements with their
// documents, kept ones, refusals and tombstones without content, each with its note; the range
// ended below a statement that does not verify or a live document that cannot be read; a feed the
// gateway cannot answer fails alone.
func TestExportFeeds(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			ak, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			src := source{t: t, key: ak, fp: extfile.Fingerprint(&ak.PublicKey)}
			stranger := source{t: t, key: mustRSA(t), fp: src.fp} // claims the key, signs with another
			pemStr, _ := extfile.MarshalPublicKeyPEM(&ak.PublicKey)
			keysFile, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": src.fp, "public_key": pemStr}}})
			tn, _ := g.st.GetTenant(ctx, "acme")
			b1, b2, b3 := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)

			// the gateway's export feed of the URL, run: its keys file, its cursors
			gw := store.Feed{TenantID: tn.ID, Name: "reviews", URL: feedURL, Credential: "src", Keys: []string{src.fp}, Offer: store.OfferUpstreams,
				Export: true, CreatedBy: "os:1:t"}
			narrow := store.Feed{TenantID: tn.ID, Name: "narrow", URL: "https://src.example.org/api/v1/tenants/scans", Credential: "src",
				Keys: []string{src.fp}, Kinds: []string{store.KindSBOM}, Offer: store.OfferUpstreams, Export: true, CreatedBy: "os:1:t"}
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					for _, f := range []*store.Feed{&gw, &narrow} {
						if err := tx.InsertFeed(ctx, f); err != nil {
							return err
						}
						if err := tx.SetFeedKeysFile(ctx, f.ID, keysFile); err != nil {
							return err
						}
					}
					for b, c := range map[string]int64{b1: 10, b2: 10, b3: 5} {
						if err := tx.SetFeedCursor(ctx, gw.ID, b, c); err != nil {
							return err
						}
					}
					return nil
				})
			})
			gw, _ = getFeed(t, g.env, tn.ID, "reviews")

			// what it holds for b1: a stored document (1), a refusal (2), a kept withdrawal (3), a
			// tombstone (4), a document whose object is gone (6); for b2 a statement that does not
			// verify (2) after a stored one (1)
			doc := []byte(`{"schema":"kista.review.v1","verdict":"approved"}`)
			store1 := func(seq int64, id, body string, s source) store.Attachment {
				b, sig, st := s.statement(seq, id, body, doc, nil)
				a := store.Attachment{ID: store.NewID(), TenantID: tn.ID, BodyHash: body, Kind: st.Kind, MediaType: st.MediaType, Title: st.Title,
					Size: st.Size, ContentHash: st.ContentHash, Summary: "{}", Origin: store.AttachedByFeed, AttachedBy: gw.ID,
					Visibility: store.VisibleAdministrators, Issuer: feedURL, IssuerID: id, KeyFP: src.fp, SourceOrigin: "tenant",
					IssuedAt: time.Now(), Statement: string(b), Signature: sig, Seq: seq}
				if err := g.blob.PutAttachment(ctx, tn.StorageDomain, a.ID, doc); err != nil {
					t.Fatal(err)
				}
				must(t, func() error {
					return g.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &a) })
				})
				return a
			}
			store1(1, "d1", b1, src)
			rb, rsig, _ := src.statement(2, "d2", b1, doc, func(st *attachment.Statement) { st.Kind, st.MediaType = store.KindSBOM, attachment.CycloneDX })
			kb, ksig, _ := src.statement(3, "d0", b1, nil, withdrawal)
			gone := store1(4, "d4", b1, src)
			lost := store1(6, "d6", b1, src)
			store1(1, "e1", b2, src)
			ub, usig, _ := stranger.statement(2, "e2", b2, doc, nil)
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					if err := tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: gw.ID, BodyHash: b1, Seq: 2, Reason: "kind", Statement: string(rb),
						Signature: rsig}); err != nil {
						return err
					}
					if _, err := tx.KeepFeedStatement(ctx, store.FeedKept{TenantID: tn.ID, Issuer: feedURL, IssuerID: "d0", BodyHash: b1, Seq: 3,
						Statement: string(kb), Signature: ksig}); err != nil {
						return err
					}
					// a statement held by nobody but a refusal that does not verify: it ends b2's range
					if err := tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: gw.ID, BodyHash: b2, Seq: 2, Reason: "kind", Statement: string(ub),
						Signature: usig}); err != nil {
						return err
					}
					_, err := tx.RemoveAttachment(ctx, gone)
					return err
				})
			})
			if err := g.blob.DeleteAttachment(ctx, tn.StorageDomain, lost.ID); err != nil {
				t.Fatal(err)
			}

			p := g.plan(t)
			p.Feeds = []bundle.PlanFeed{
				{URL: feedURL, Keys: []string{src.fp}, Kinds: []string{store.KindReview}, Sources: []int{},
					Bodies: []bundle.PlanBody{{BodyHash: b1, Seq: 0}, {BodyHash: b2, Seq: 0}, {BodyHash: b3, Seq: 5}}},
				{URL: "https://src.example.org/api/v1/tenants/scans", Keys: []string{src.fp}, Kinds: []string{}, Sources: []int{},
					Bodies: []bundle.PlanBody{{BodyHash: b1, Seq: 0}}},
				{URL: "https://src.example.org/api/v1/tenants/zzz", Keys: []string{src.fp}, Kinds: []string{}, Sources: []int{},
					Bodies: []bundle.PlanBody{}},
			}
			res, data := g.export(t, p, upstream.ExportOptions{})
			if !res.Failed() || len(res.Feeds) != 3 || res.Feeds[0].Error != "" || res.Feeds[1].Error != "accepts fewer kinds" ||
				res.Feeds[2].Error != "not subscribed here" {
				t.Fatalf("feeds: %+v", res.Feeds)
			}
			if c := res.Feeds[0].Counts; c["bodies"] != 2 || c["statements"] != 5 || c["withdrawals"] != 1 || c["without_content"] != 2 ||
				c["documents"] != 1 {
				t.Fatalf("counts: %+v", c)
			}

			// the bundle: one feed, its ranges, notes and the one document
			b, err := bundle.Open(bytes.NewReader(data), time.Now(), func(string) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			if len(b.Manifest.Feeds) != 1 {
				t.Fatalf("manifest feeds: %+v", b.Manifest.Feeds)
			}
			ranges := map[string][2]int64{}
			for _, fb := range b.Manifest.Feeds[0].Bodies {
				ranges[fb.BodyHash] = [2]int64{fb.AfterSeq, fb.Seq}
			}
			if ranges[b1] != [2]int64{0, 5} || ranges[b2] != [2]int64{0, 1} || len(ranges) != 2 {
				t.Fatalf("ranges: %v", ranges) // b1 ends below the lost document, b2 below the unverified statement, b3 is at its cursor
			}
			notes := map[int64]string{}
			var docs [][]byte
			var keys []byte
			for {
				it, err := b.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if it.R != nil {
					_, _ = io.Copy(io.Discard, it.R)
				}
				if it.Feed == nil {
					continue
				}
				if it.Damaged != "" {
					t.Fatalf("damaged: %s", it.Damaged)
				}
				fe, _ := bundle.ParseFeedEntry(it.Feed.Entries[it.Entry].Path)
				switch {
				case fe.Keys:
					keys = it.Data
				case fe.Batch > 0:
					entries, err := bundle.ParseBatch(it.Data)
					if err != nil {
						t.Fatal(err)
					}
					for _, be := range entries {
						raw, _ := base64.StdEncoding.DecodeString(be.Statement)
						var st struct {
							Seq      int64  `json:"seq"`
							BodyHash string `json:"body_hash"`
						}
						_ = json.Unmarshal(raw, &st)
						if st.BodyHash == b1 {
							notes[st.Seq] = be.Note
						}
					}
				default:
					docs = append(docs, it.Data)
				}
			}
			if !b.FeedWhole(0) || !bytes.Equal(keys, keysFile) || len(docs) != 1 || !bytes.Equal(docs[0], doc) {
				t.Fatalf("entries: whole %v, keys %q, %d documents", b.FeedWhole(0), keys, len(docs))
			}
			want := map[int64]string{1: "", 2: "kind", 3: "", 4: "removed on the exporting kista"}
			if len(notes) != len(want) {
				t.Fatalf("b1's statements: %v", notes)
			}
			for q, n := range want {
				if notes[q] != n {
					t.Errorf("seq %d: note %q, want %q", q, notes[q], n)
				}
			}
			if evs := g.events(t, "bundle.export"); len(evs) == 0 || !strings.Contains(evs[0].Data, `"feed_statements":5`) ||
				!strings.Contains(evs[0].Data, `"failed_feeds":2`) {
				t.Errorf("the event: %+v", evs)
			}

			// a kind the site's feed does not take: its content does not leave
			p.Feeds[0].Kinds = []string{store.KindSBOM}
			res, _ = g.export(t, p, upstream.ExportOptions{})
			// the lost document no longer ends b1's range: its content would not leave anyway
			if c := res.Feeds[0].Counts; res.Feeds[0].Error != "" || c["documents"] != 0 || c["without_content"] != 5 || c["statements"] != 6 {
				t.Fatalf("another kind: %+v", res.Feeds[0])
			}
			p.Feeds[0].Kinds = []string{store.KindReview}

			// over 1 MiB of statements: several batches, a document after the batch that first names it
			for q := int64(6); q < 1206; q++ {
				wb, wsig, _ := src.statement(q, "w"+strings.Repeat("x", 40)+string(rune('a'+q%26))+time.Duration(q).String(), b3, nil, withdrawal)
				must(t, func() error {
					return g.st.InTx(ctx, "", func(tx *store.Tx) error {
						_, err := tx.KeepFeedStatement(ctx, store.FeedKept{TenantID: tn.ID, Issuer: feedURL, IssuerID: "w" + time.Duration(q).String(),
							BodyHash: b3, Seq: q, Statement: string(wb), Signature: wsig})
						return err
					})
				})
			}
			first := doc
			doc = []byte(`{"schema":"kista.review.v1","verdict":"rejected"}`) // another content: its own entry
			store1(1250, "late", b3, src)
			doc = first
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, gw.ID, b3, 1300) })
			})
			p.Feeds = p.Feeds[:1]
			res, data = g.export(t, p, upstream.ExportOptions{})
			if res.Feeds[0].Error != "" || res.Feeds[0].Counts["statements"] != 5+1201 {
				t.Fatalf("many statements: %+v", res.Feeds[0])
			}
			b, err = bundle.Open(bytes.NewReader(data), time.Now(), func(string) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, e := range b.Manifest.Feeds[0].Entries {
				fe, _ := bundle.ParseFeedEntry(e.Path)
				switch {
				case fe.Keys:
					kinds = append(kinds, "keys")
				case fe.Batch > 0:
					kinds = append(kinds, "batch")
				default:
					kinds = append(kinds, "doc")
				}
			}
			if got := strings.Join(kinds, ","); got != "keys,batch,doc,batch,doc" {
				t.Fatalf("entries: %s", got) // d1's document after the first batch, late's after the batch that names it
			}

			// the plan pins another key: nothing verifies, the ranges are empty and the bodies left out
			p.Feeds = p.Feeds[:1]
			p.Feeds[0].Keys = []string{"sha256:" + strings.Repeat("f", 64)}
			res, _ = g.export(t, p, upstream.ExportOptions{})
			if res.Feeds[0].Error != "" || res.Feeds[0].Counts["statements"] != 0 || res.Feeds[0].Counts["bodies"] != 0 {
				t.Fatalf("another pin: %+v", res.Feeds[0])
			}
			// the export feed not kept: the feed fails as a whole
			off := false
			if _, err := g.feeds().Change(ctx, admin, "acme", "reviews", attachment.FeedChange{Export: &off}, 0); err != nil {
				t.Fatal(err)
			}
			p.Feeds[0].Keys = []string{src.fp}
			if res, _ = g.export(t, p, upstream.ExportOptions{}); res.Feeds[0].Error != "not kept for export" {
				t.Fatalf("not kept: %+v", res.Feeds[0])
			}
		})
	}
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func getFeed(t *testing.T, en *env, tenantID, name string) (store.Feed, error) {
	t.Helper()
	var f store.Feed
	err := en.st.InTx(ctx, "", func(tx *store.Tx) (err error) { f, err = tx.GetFeed(ctx, tenantID, name); return })
	return f, err
}

// feeds is the gateway's feed service, for changes only (it never runs here).
func (g *gateway) feeds() *attachment.Feeds {
	return &attachment.Feeds{Store: g.st, Authz: g.up.Authz, Attachments: &attachment.Service{Store: g.st, Blob: g.blob, Authz: g.up.Authz}}
}
