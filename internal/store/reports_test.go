package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0045 on every dialect: migration 0055 (feeds.via, feed_refusals.issuer_id, min_reader), the
// default offer over the catalogue builds that came from outside, the attachments list narrowed to a
// source and a version, a kept refusal replaced by a later one of the same document, and the
// document quota as an error of its own.
func TestReceivedReports(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 55 AND min_reader = 55"); err != nil || n != 1 {
			t.Fatalf("migration 55 raises min_reader: %d %v", n, err) // an older replica would run a via feed against its issuer
		}

		// --- the offer ---
		own := store.Publisher{TenantID: tn.ID, Name: "ci"}
		ownGone := store.Publisher{TenantID: tn.ID, Name: "old-ci"}
		dist := store.Publisher{TenantID: tn.ID, Name: "enterest-ci", Distributor: "enterest"}
		distGone := store.Publisher{TenantID: tn.ID, Name: "enterest-old", Distributor: "enterest"}
		inTx(t, s, func(tx *store.Tx) error {
			for _, p := range []*store.Publisher{&own, &ownGone, &dist, &distGone} {
				if err := tx.InsertPublisher(ctx, p); err != nil {
					return err
				}
			}
			return nil
		})
		mirror := store.Upstream{TenantID: tn.ID, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &mirror) })
		up := store.UpstreamSource(mirror.ID)
		seq := int64(0)
		// build adds a catalogue build of a source: landed (unreleased), or released in the channel
		build := func(name, hash, source string, released bool) store.Build {
			t.Helper()
			b := cppBuild(tn, name, "1.0", hash)
			b.Source = source
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
					return err
				}
				if !released {
					return tx.SetLanded(ctx, b.ID)
				}
				seq++
				r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Source: b.Source, Name: b.Name, PackageVersion: b.PackageVersion,
					Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: seq, Origin: store.OriginPlacement}
				return tx.InsertRelease(ctx, &r, "os:1:t")
			})
			return b
		}
		h := func(c string) string { return strings.Repeat(c, 64) }
		build("up-released", "1", up, true)
		build("up-unreleased", "2", up, false)
		build("dist-live", "3", store.PublisherSource(dist.ID), false)
		build("dist-removed", "4", store.PublisherSource(distGone.ID), true)
		build("own-tenant", "5", store.SourceTenant, true)
		build("own-publisher", "6", store.PublisherSource(own.ID), true)
		build("own-removed", "7", store.PublisherSource(ownGone.ID), true)
		withdrawn := build("up-withdrawn", "8", store.PublisherSource(dist.ID), false)
		revoked := build("up-revoked", "9", store.PublisherSource(dist.ID), true)
		// a body from outside that a build of the tenant's own has too: never offered
		build("both", "a", up, true)
		build("both", "a", store.SourceTenant, false)
		// a build the catalogue does not keep (neither landed nor released): not offered
		stray := cppBuild(tn, "stray", "1.0", "b")
		stray.Source = up
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &stray) })
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, ownGone.Name, "os:1:t") })
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, distGone.Name, "os:1:t") })
		inTx(t, s, func(tx *store.Tx) error {
			_, _, err := tx.Withdraw(ctx, tn.ID, withdrawn.PackageType, withdrawn.Source, withdrawn.Name, withdrawn.PackageVersion, "bad", "os:1:t")
			return err
		})
		if err := store.ExecRaw(ctx, s, "UPDATE builds SET revoked_at = created_at WHERE id = ?", revoked.ID); err != nil {
			t.Fatal(err)
		}

		f := store.Feed{TenantID: tn.ID, Name: "src", URL: "https://issuer.example/api/v1/tenants/x", Credential: "c", Offer: store.OfferUpstreams,
			Keys: []string{"sha256:" + strings.Repeat("9", 64)}, CreatedBy: "os:1:t", Export: true, Via: "https://relay.example/api/v1/tenants/acme"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &f) })
		bodies, err := s.OfferedBodies(ctx, f)
		if want := []string{h("1"), h("2"), h("3"), h("4"), h("9")}; err != nil || !slices.Equal(bodies, want) {
			t.Fatalf("the offer:\n got %v\nwant %v (%v)", bodies, want, err)
		}
		// another tenant's build that names this tenant's distributor's publisher is that tenant's
		// own: a publisher is a distributor's only in its own tenant
		beta := store.Tenant{Name: "beta"}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateTenant(ctx, &beta) })
		foreign := cppBuild(beta, "geo", "1.0", "e")
		foreign.Source = store.PublisherSource(dist.ID)
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &foreign); err != nil {
				return err
			}
			return tx.SetLanded(ctx, foreign.ID)
		})
		bf := store.Feed{TenantID: beta.ID, Name: "src", URL: f.URL, Credential: "c", Offer: store.OfferUpstreams, Keys: f.Keys, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &bf) })
		if bodies, err := s.OfferedBodies(ctx, bf); err != nil || len(bodies) != 0 {
			t.Fatalf("another tenant offers a body by this tenant's distributor's publisher: %v %v", bodies, err)
		}
		// the cursors of the bodies asked about, and no others
		inTx(t, s, func(tx *store.Tx) error {
			for i, b := range []string{h("1"), h("2"), h("3")} {
				if err := tx.SetFeedCursor(ctx, f.ID, b, int64(i+5)); err != nil {
					return err
				}
			}
			return nil
		})
		if cur, err := s.FeedCursorsOf(ctx, f.ID, []string{h("1"), h("3"), h("f")}); err != nil || len(cur) != 2 || cur[h("1")] != 5 || cur[h("3")] != 7 {
			t.Fatalf("the cursors asked about: %v %v", cur, err)
		}
		if cur, err := s.FeedCursorsOf(ctx, f.ID, nil); err != nil || len(cur) != 0 {
			t.Fatalf("no body asked about: %v %v", cur, err)
		}
		// what a relayed answer could not verify, on the feed's record
		cur := mustGetFeed(t, s, tn.ID, "src")
		if wrote, err := s.MarkFeedRelayStopped(ctx, cur); err != nil || !wrote {
			t.Fatalf("relay stopped: %v %v", wrote, err)
		}
		if wrote, err := s.MarkFeedRelayStopped(ctx, cur); err != nil || wrote { // within the minute
			t.Fatalf("relay stopped, written twice in a minute: %v %v", wrote, err)
		}
		if got := mustGetFeed(t, s, tn.ID, "src"); got.RelayStoppedAt.IsZero() {
			t.Fatalf("relay stopped: %+v", got)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.ClearFeedRelayStopped(ctx, f.ID) })
		stale := cur
		stale.Version-- // a feed as read before it changed
		if wrote, err := s.MarkFeedRelayStopped(ctx, stale); err != nil || wrote {
			t.Fatalf("relay stopped, written for a feed read before its change: %v %v", wrote, err)
		}
		if got := mustGetFeed(t, s, tn.ID, "src"); !got.RelayStoppedAt.IsZero() {
			t.Fatalf("relay stopped, cleared: %+v", got)
		}
		if room, err := s.FeedDocumentRoom(ctx, tn.ID); err != nil || room != store.MaxTenantAttachments {
			t.Fatalf("the room for feed documents: %d %v", room, err)
		}

		// --- feeds.via: stored, read back, never changed by SetFeed, refused on a bundle-mode feed ---
		got := mustGetFeed(t, s, tn.ID, "src")
		if got.Via != f.Via {
			t.Fatalf("via: %q", got.Via)
		}
		got.Readers, got.Via = true, "https://other.example/api/v1/tenants/x"
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeed(ctx, &got, 0) })
		if again := mustGetFeed(t, s, tn.ID, "src"); again.Via != f.Via || !again.Readers {
			t.Fatalf("a change moved via: %+v", again)
		}
		direct := f
		direct.ID, direct.Name, direct.Via = "", "direct", ""
		// a tenant hands on at most one feed per issuer: a second feed of the URL with export is
		// refused, at its creation and at a change, naming the feed that hands the issuer on
		err = s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertFeed(ctx, &direct) })
		if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), `"src"`) {
			t.Fatalf("a second export feed of the issuer: %v", err)
		}
		direct.ID, direct.Export = "", false
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &direct) })
		on := mustGetFeed(t, s, tn.ID, "direct")
		on.Export = true
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetFeed(ctx, &on, 0) }); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), `"src"`) {
			t.Fatalf("export set on a second feed of the issuer: %v", err)
		}
		if d := mustGetFeed(t, s, tn.ID, "direct"); d.Export {
			t.Fatal("the refused change was written")
		}
		// the one that hands on may change otherwise; another issuer's feed, and another tenant's feed
		// of the same issuer, hand on freely
		same := mustGetFeed(t, s, tn.ID, "src")
		same.Readers = !same.Readers
		inTx(t, s, func(tx *store.Tx) error { return tx.SetFeed(ctx, &same, 0) })
		elsewhere := f
		elsewhere.ID, elsewhere.Name, elsewhere.URL, elsewhere.Via = "", "elsewhere", "https://other.example/api/v1/tenants/x", ""
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &elsewhere) })
		gamma := store.Tenant{Name: "gamma"}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateTenant(ctx, &gamma) })
		theirs := f
		theirs.ID, theirs.TenantID, theirs.Via = "", gamma.ID, ""
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertFeed(ctx, &theirs) })
		if d := mustGetFeed(t, s, tn.ID, "direct"); d.Via != "" {
			t.Fatalf("a feed without via: %q", d.Via)
		}
		bundle := store.Feed{TenantID: tn.ID, Name: "site", URL: f.URL, Offer: store.OfferUpstreams, Keys: f.Keys, Mode: store.FeedBundle, Via: f.Via}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertFeed(ctx, &bundle) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a bundle-mode feed with via: %v", err)
		}

		// --- a kept refusal replaced by a later one of the same document ---
		keep := func(seq int64, id string) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				return tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: f.ID, BodyHash: h("1"), Seq: seq, IssuerID: id, Reason: "size",
					Statement: "{}", Signature: "s"})
			})
		}
		// a row kept before the column has no issuer_id: it is replaced by none
		if err := store.ExecRaw(ctx, s, "INSERT INTO feed_refusals (feed_id, body_hash, seq, reason, statement, signature, refused_at) SELECT ?, ?, 1, 'size', '{}', 's', created_at FROM feeds WHERE id = ?",
			f.ID, h("1"), f.ID); err != nil {
			t.Fatal(err)
		}
		keep(2, "doc-a")
		keep(3, "doc-b")
		keep(5, "doc-a") // replaces seq 2
		keep(4, "doc-a") // an earlier one arriving late replaces nothing
		keep(6, "")      // a statement whose id cannot be read
		keep(7, "")
		rs, err := s.FeedRefusals(ctx, f.ID, h("1"), 0, 100)
		var seqs []int64
		ids := map[int64]string{}
		for _, r := range rs {
			seqs = append(seqs, r.Seq)
			ids[r.Seq] = r.IssuerID
		}
		if err != nil || !slices.Equal(seqs, []int64{1, 3, 4, 5, 6, 7}) || ids[1] != "" || ids[3] != "doc-b" || ids[5] != "doc-a" || ids[6] != "" {
			t.Fatalf("kept: %v %v %v", seqs, ids, err)
		}
		// another feed's rows of the same document stay
		inTx(t, s, func(tx *store.Tx) error {
			return tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: direct.ID, BodyHash: h("1"), Seq: 2, IssuerID: "doc-a", Reason: "size",
				Statement: "{}", Signature: "s"})
		})
		keep(8, "doc-a")
		if rs, _ := s.FeedRefusals(ctx, direct.ID, h("1"), 0, 100); len(rs) != 1 {
			t.Fatalf("another feed's kept row went: %v", rs)
		}
		// nor does a row of the same id about another body: a document is one body's
		inTx(t, s, func(tx *store.Tx) error {
			return tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: f.ID, BodyHash: h("2"), Seq: 3, IssuerID: "doc-z", Reason: "size", Statement: "{}", Signature: "s"})
		})
		inTx(t, s, func(tx *store.Tx) error {
			return tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: f.ID, BodyHash: h("3"), Seq: 4, IssuerID: "doc-z", Reason: "size", Statement: "{}", Signature: "s"})
		})
		if rs, _ := s.FeedRefusals(ctx, f.ID, h("2"), 0, 100); len(rs) != 1 {
			t.Fatalf("a kept row of another body went: %v", rs)
		}
		// a replacement needs no room: at the cap, a later statement of a kept document is still kept
		was := store.MaxFeedKept
		store.MaxFeedKept = 8 // src: 1, 3, 6, 7, 8 and doc-z twice; direct: 2
		defer func() { store.MaxFeedKept = was }()
		keep(9, "doc-a")
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.KeepFeedRefusal(ctx, tn.ID, store.FeedRefusal{FeedID: f.ID, BodyHash: h("1"), Seq: 10, IssuerID: "doc-new", Reason: "size", Statement: "{}", Signature: "s"})
		}); !errors.Is(err, store.ErrQuota) {
			t.Fatalf("a new document's refusal at the cap: %v", err)
		}
		store.MaxFeedKept = was

		// --- the list narrowed to a source and a version ---
		two := cppBuild(tn, "up-released", "2.0", "c")
		two.Source = up
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &two); err != nil {
				return err
			}
			return tx.SetLanded(ctx, two.ID)
		})
		other := cppBuild(tn, "up-released", "1.0", "d") // the same name and version from another source
		other.Source = store.PublisherSource(dist.ID)
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &other); err != nil {
				return err
			}
			return tx.SetLanded(ctx, other.ID)
		})
		doc := func(body, id string) {
			t.Helper()
			a := store.Attachment{TenantID: tn.ID, BodyHash: body, Kind: "report", MediaType: "application/vnd.kista.report+json", Title: id, Size: 1,
				ContentHash: h("0"), Summary: `{}`, Origin: store.AttachedByFeed, AttachedBy: f.ID, Visibility: store.VisibleAdministrators,
				Issuer: f.URL, IssuerID: id, Seq: 1}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &a) })
		}
		doc(h("1"), "r-1.0")
		doc(h("c"), "r-2.0")
		doc(h("d"), "r-dist")
		titles := func(source, version string) []string {
			t.Helper()
			as, err := s.ExtensionAttachmentsOf(ctx, tn.ID, two.PackageType, "up-released", source, version)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, a := range as {
				out = append(out, a.Title)
			}
			slices.Sort(out)
			return out
		}
		for _, c := range []struct {
			source, version string
			want            []string
		}{
			{"", "", []string{"r-1.0", "r-2.0", "r-dist"}},
			{up, "", []string{"r-1.0", "r-2.0"}},
			{"", "1.0", []string{"r-1.0", "r-dist"}},
			{up, "2.0", []string{"r-2.0"}},
			{store.PublisherSource(dist.ID), "1.0", []string{"r-dist"}},
			{up, "3.0", nil},
			{store.SourceTenant, "", nil},
		} {
			if got := titles(c.source, c.version); !slices.Equal(got, c.want) {
				t.Errorf("source %q version %q: %v, want %v", c.source, c.version, got, c.want)
			}
		}

		// --- the document quota is an error of its own; the heads limit is not ---
		wasMax := store.MaxTenantAttachments
		store.MaxTenantAttachments = 3
		defer func() { store.MaxTenantAttachments = wasMax }()
		over := store.Attachment{TenantID: tn.ID, BodyHash: h("2"), Kind: "report", MediaType: "application/vnd.kista.report+json", Title: "over", Size: 1,
			ContentHash: h("0"), Summary: `{}`, Origin: store.AttachedByFeed, AttachedBy: f.ID, Visibility: store.VisibleAdministrators,
			Issuer: f.URL, IssuerID: "over", Seq: 1}
		err = s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &over) })
		if !errors.Is(err, store.ErrAttachmentQuota) || !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a document over the quota: %v", err)
		}
		store.MaxTenantAttachments = wasMax
		wasHeads := store.MaxAttachmentHeads
		store.MaxAttachmentHeads = 1
		defer func() { store.MaxAttachmentHeads = wasHeads }()
		second := over
		second.BodyHash, second.IssuerID = h("1"), "second-head"
		err = s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &second) })
		if !errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrAttachmentQuota) {
			t.Fatalf("a head over the issuer's limit: %v", err)
		}
	})
}
