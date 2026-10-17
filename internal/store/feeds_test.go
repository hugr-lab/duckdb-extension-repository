package store_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// The feed's records (spec 0011 phase 1b) on every dialect: attestation keys and their registry,
// statement numbering, what a source offers and to whom, feeds, cursors, received documents.
func TestFeedRecords(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)

		// attestation keys: one registry with channel keys, one active per tenant
		channelFP := "sha256:" + strings.Repeat("c", 64)
		addKey(t, s, ch, channelFP, store.KeyActive)
		att := func(fp, state string) (store.Key, error) {
			k := store.Key{TenantID: tn.ID, Fingerprint: fp, SignerRef: "file:" + fp, PublicKey: []byte{1}, State: state}
			err := s.InTx(ctx, store.AttestationLock(tn.ID), func(tx *store.Tx) error { return tx.InsertAttestationKey(ctx, &k, "os:1:t") })
			return k, err
		}
		if _, err := att(channelFP, store.KeyTrusted); !errors.Is(err, store.ErrExists) {
			t.Fatalf("a channel key as an attestation key: %v", err)
		}
		k1, err := att("sha256:"+strings.Repeat("1", 64), store.KeyActive)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := att("sha256:"+strings.Repeat("2", 64), store.KeyActive); err == nil {
			t.Fatal("two active attestation keys")
		}
		k2, err := att("sha256:"+strings.Repeat("3", 64), store.KeyTrusted)
		if err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.SetAttestationKeyState(ctx, &k1, store.KeyTrusted, "os:1:t"); err != nil {
				return err
			}
			return tx.SetAttestationKeyState(ctx, &k2, store.KeyActive, "os:1:t")
		})
		if ks, err := s.AttestationKeys(ctx, tn.ID); err != nil || len(ks) != 2 || ks[1].State != store.KeyActive || ks[0].ChannelID != "" {
			t.Fatalf("keys: %+v %v", ks, err)
		}

		// statement numbering: per tenant, from 1, never reused
		var seqs []int64
		for range 3 {
			inTx(t, s, func(tx *store.Tx) error {
				n, err := tx.NextStatementSeq(ctx, tn.ID)
				seqs = append(seqs, n)
				return err
			})
		}
		if seqs[0] != 1 || seqs[2] != 3 {
			t.Fatalf("seqs: %v", seqs)
		}

		// a body released in prod, offered: statements about it, the latest only while feedable
		b := cppBuild(tn, "tresor", "1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, ExtVersion: b.ExtVersion, Platform: b.Platform,
			Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 1, Origin: store.OriginUpstream}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r, "system:upstream") })
		doc := store.Attachment{TenantID: tn.ID, BodyHash: b.BodyHash, Kind: store.KindReview, MediaType: "application/vnd.kista.review.v1+json",
			Title: "R", Size: 1, ContentHash: strings.Repeat("0", 64), Summary: `{}`, Origin: store.AttachedByTenant, AttachedBy: "os:1:t",
			Visibility: store.VisibleReaders}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &doc) })
		inTx(t, s, func(tx *store.Tx) error { return tx.SetAttachmentFlags(ctx, &doc, doc.Visibility, true, 0) })
		issue := func(seq int64, withdrawn bool) {
			inTx(t, s, func(tx *store.Tx) error {
				return tx.InsertStatement(ctx, store.IssuedStatement{TenantID: tn.ID, Seq: seq, IssuerID: doc.ID, BodyHash: b.BodyHash,
					Withdrawn: withdrawn, Statement: []byte(`{"seq":` + strconv.FormatInt(seq, 10) + `}`), Signature: []byte{1, 2, 3}, IssuedAt: time.Now()})
			})
		}
		issue(4, false)
		issue(5, false)
		if sts, err := s.BodyStatements(ctx, tn.ID, b.BodyHash, 4, 10); err != nil || len(sts) != 1 || sts[0].Seq != 5 ||
			string(sts[0].Signature) != "\x01\x02\x03" {
			t.Fatalf("statements after 4: %+v %v", sts, err)
		}
		for seq, want := range map[int64]bool{4: false, 5: true} {
			if ok, err := s.StatementOffered(ctx, tn.ID, doc.ID, seq); err != nil || ok != want {
				t.Errorf("offered %d: %v %v", seq, ok, err)
			}
		}
		if latest, err := s.LatestStatement(ctx, tn.ID, doc.ID); err != nil || latest.Seq != 5 {
			t.Fatalf("latest: %+v %v", latest, err)
		}
		if in, err := s.ReleasedIn(ctx, tn.ID, []string{ch.ID}, []string{b.BodyHash, strings.Repeat("f", 64)}); err != nil || len(in) != 1 || !in[b.BodyHash] {
			t.Fatalf("released in: %v %v", in, err)
		}
		if in, _ := s.ReleasedIn(ctx, tn.ID, []string{store.NewID()}, []string{b.BodyHash}); len(in) != 0 {
			t.Fatal("released in another channel")
		}

		// feeds: limits on keys, offered bodies (from upstreams, not the tenant's own), cursors
		f := store.Feed{TenantID: tn.ID, Name: "src", URL: "https://src.example/api/v1/tenants/x", Credential: "c", Offer: store.OfferUpstreams,
			Kinds: []string{store.KindReview}, CreatedBy: "os:1:t"}
		if err := s.InTx(ctx, store.FeedLock(tn.ID), func(tx *store.Tx) error { return tx.InsertFeed(ctx, &f) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a feed without keys: %v", err)
		}
		f.Keys = []string{"sha256:" + strings.Repeat("9", 64)}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &f) })
		if again := f; s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertFeed(ctx, &again) }) == nil {
			t.Fatal("a feed name twice")
		}
		if bodies, err := s.OfferedBodies(ctx, f); err != nil || len(bodies) != 1 || bodies[0] != b.BodyHash {
			t.Fatalf("offered: %v %v", bodies, err)
		}
		own := cppBuild(tn, "acl", "1.0", "b")
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &own) })
		ro := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: own.ID, Name: own.Name, ExtVersion: own.ExtVersion, Platform: own.Platform,
			Slot: own.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: 2, Origin: store.OriginPublication}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &ro, "publisher:acme/ci") })
		if bodies, _ := s.OfferedBodies(ctx, f); len(bodies) != 1 {
			t.Fatalf("the tenant's own publication offered: %v", bodies)
		}
		byChannel := f
		byChannel.ID, byChannel.Offer, byChannel.Name, byChannel.Channels = "", store.OfferChannels, "chan", []string{ch.ID}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &byChannel) })
		if bodies, _ := s.OfferedBodies(ctx, byChannel); len(bodies) != 2 {
			t.Fatalf("a channel's bodies: %v", bodies)
		}
		for _, seq := range []int64{3, 2, 7} {
			inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, f.ID, b.BodyHash, seq) })
		}
		if cur, err := s.FeedCursors(ctx, f.ID); err != nil || cur[b.BodyHash] != 7 {
			t.Fatalf("cursors: %v %v", cur, err)
		}

		// runs: requested, then not again until requested or due
		inTx(t, s, func(tx *store.Tx) error { return tx.RequestFeedRun(ctx, &f) })
		if due, err := s.DueFeeds(ctx, time.Now(), false); err != nil || len(due) != 1 || due[0] != f.ID {
			t.Fatalf("due: %v %v", due, err)
		}
		started := time.Now()
		inTx(t, s, func(tx *store.Tx) error {
			if _, ok, err := tx.StartFeedRun(ctx, f.ID, false); err != nil || !ok {
				t.Fatalf("start: %v %v", ok, err)
			}
			if _, ok, _ := tx.StartFeedRun(ctx, f.ID, false); ok {
				t.Fatal("started twice")
			}
			return tx.FinishFeedRun(ctx, f.ID, `{"counts":{}}`, store.FeedOK, "", started, started.Add(time.Hour))
		})
		got := mustGetFeed(t, s, tn.ID, "src")
		if got.Status != store.FeedOK || got.LastSuccessAt.IsZero() || got.NextRunAt.IsZero() || !got.RequestedAt.IsZero() {
			t.Fatalf("after a run: %+v", got)
		}
		if due, _ := s.DueFeeds(ctx, time.Now(), true); len(due) != 1 || due[0] != byChannel.ID {
			t.Fatalf("scheduled: %v", due)
		}

		// received documents: an unresolved supersedes is a new head, a reissue and a withdrawal move forward only
		recv := func(issuerID, supersedes string, seq int64) store.Attachment {
			return store.Attachment{TenantID: tn.ID, BodyHash: b.BodyHash, Kind: store.KindReview, MediaType: doc.MediaType, Title: "R", Size: 1,
				ContentHash: strings.Repeat("0", 64), Summary: `{}`, Origin: store.AttachedByFeed, AttachedBy: f.ID,
				Visibility: store.VisibleAdministrators, Issuer: f.URL, IssuerID: issuerID, Supersedes: supersedes, Seq: seq}
		}
		a := recv("r2", "r1", 3)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &a) })
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.FeedAttachment(ctx, tn.ID, f.URL, "r2")
			if err != nil || got.Seq != 3 || got.Supersedes != "r1" {
				t.Fatalf("received: %+v %v", got, err)
			}
			return nil
		})
		older := a
		older.Seq = 2
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.ReissueFeedAttachment(ctx, &older) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("an older reissue: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.WithdrawFeedAttachment(ctx, &a, 9, "{}", "s") }); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			cur, _ := tx.GetAttachment(ctx, tn.ID, a.ID)
			return tx.SetAttachmentFlags(ctx, &cur, store.VisibleReaders, false, 0)
		})
		newer := a
		newer.Seq, newer.Visibility = 10, store.VisibleReaders
		inTx(t, s, func(tx *store.Tx) error { return tx.ReissueFeedAttachment(ctx, &newer) })
		if g, _ := s.GetAttachment(ctx, tn.ID, a.ID); !g.WithdrawnAt.IsZero() || g.Seq != 10 || g.Visibility != store.VisibleReaders {
			t.Fatalf("reissued: %+v", g)
		}
		narrowed := a
		narrowed.Seq, narrowed.Visibility = 11, store.VisibleAdministrators
		inTx(t, s, func(tx *store.Tx) error { return tx.ReissueFeedAttachment(ctx, &narrowed) })
		if g, _ := s.GetAttachment(ctx, tn.ID, a.ID); g.Visibility != store.VisibleAdministrators {
			t.Fatalf("not narrowed: %+v", g)
		}

		// the heads limit counts per issuer: one source never uses up another's room
		for i := range store.MaxAttachmentHeads {
			x := recv("x"+strconv.Itoa(i), "", 1)
			x.Issuer = "https://x.example/api/v1/tenants/x"
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &x) })
		}
		over := recv("x-over", "", 1)
		over.Issuer = "https://x.example/api/v1/tenants/x"
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &over) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a head beyond the issuer's limit: %v", err)
		}
		other := recv("y1", "", 1)
		other.Issuer = "https://y.example/api/v1/tenants/y"
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &other) })

		// removing the feed's documents, the feed; the collector's cursors of bodies no longer held
		inTx(t, s, func(tx *store.Tx) error {
			n, err := tx.RemoveFeedDocuments(ctx, f)
			if n != 2+store.MaxAttachmentHeads {
				t.Errorf("removed %d", n)
			}
			return err
		})
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, byChannel.ID, strings.Repeat("d", 64), 4) })
		// what an export feed keeps (spec 0014) goes with the cursors of bodies no longer held
		gone := strings.Repeat("d", 64)
		for _, h := range []string{b.BodyHash, gone} {
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: byChannel.ID, BodyHash: h, Seq: 5, Reason: "kind",
					Statement: "{}", Signature: "s"}); err != nil {
					return err
				}
				_, err := tx.KeepFeedStatement(ctx, store.FeedKept{TenantID: tn.ID, Issuer: f.URL, IssuerID: "k-" + h[:4], BodyHash: h, Seq: 6,
					Statement: "{}", Signature: "s"})
				return err
			})
		}
		if n, err := s.DeleteGoneFeedCursors(ctx, "default"); err != nil || n != 1 {
			t.Fatalf("gone cursors: %d %v", n, err)
		}
		for _, h := range []string{b.BodyHash, gone} {
			want := map[bool]int{true: 1, false: 0}[h == b.BodyHash]
			r, _ := s.FeedRefusals(ctx, byChannel.ID, h, 0, 100)
			k, _ := s.FeedKeptOf(ctx, tn.ID, f.URL, h, 0, 100)
			if len(r) != want || len(k) != want {
				t.Fatalf("%s: refusals %d, kept %d; want %d", h[:4], len(r), len(k), want)
			}
		}
		if cur, _ := s.FeedCursors(ctx, f.ID); cur[b.BodyHash] != 7 {
			t.Fatalf("a held body's cursor went: %v", cur)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteFeed(ctx, f, 0) })
		if cur, _ := s.FeedCursors(ctx, f.ID); len(cur) != 0 {
			t.Fatalf("a removed feed's cursors: %v", cur)
		}
	})
}

func mustGetFeed(t *testing.T, s *store.Store, tenantID, name string) store.Feed {
	t.Helper()
	var f store.Feed
	inTx(t, s, func(tx *store.Tx) error {
		var err error
		f, err = tx.GetFeed(ctx, tenantID, name)
		return err
	})
	return f
}
