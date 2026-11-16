package upstream_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0046 at a mirror of a kista newer than this one: a revocation in a state it does not know
// is a yank (the source's word before its reason), never lowers a stored stronger state and
// forgets nothing; once this kista knows the state, the same row arrives with it and applies by
// the usual precedence (so its ETag is never kept meanwhile).
func TestRevocationUnknownState(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		k := en.kista()
		for i, v := range []string{"1.0", "1.1"} {
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp(v, "v2.0.0"), i+1, "tresor_duckdb_cpp_init"))
			if i == 0 {
				sp := en.spec(store.UpstreamEntry{Name: "tresor"})
				sp.Revocations = true
				if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
					t.Fatal(err)
				}
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("run %d: %+v", i, r)
			}
		}
		rels := map[string]store.Candidate{}
		for _, r := range en.releases(t, "tresor") {
			rels[r.PackageVersion] = r
		}
		v10, v11 := rels["1.0"], rels["1.1"]
		older := func() string {
			t.Helper()
			u, err := en.up.Get(ctx, admin, "acme", "acme-repo")
			if err != nil {
				t.Fatal(err)
			}
			return upstream.OlderThanSource(u.LastRun)
		}

		// a state of a later release: a yank, applied, the source's word kept before its reason
		k.set(row(v10, "recalled", "CVE-1"))
		r := en.sync(t, false)
		if r.Counts["revoked"] != 1 || r.Counts["revocations_unknown"] != 1 || r.Error != "" {
			t.Fatalf("an unknown state: %+v", r)
		}
		if s := en.stored(t)["1.0"]; s.State != store.RevokeYanked || s.Reason != "recalled: CVE-1" || s.AppliedState != store.RevokeYanked {
			t.Fatalf("stored: %+v", s)
		}
		if st := states(t, en, "prod", "tresor"); st["1.0"] != store.ReleaseYanked || st["1.1"] != store.ReleaseActive {
			t.Fatalf("after the yank: %+v", st)
		}
		if line := older(); !strings.Contains(line, "1 revocations in a state it does not know") {
			t.Fatalf("the administrators' line: %q", line)
		}
		// the read is never answered 304 while a state is unknown: an upgraded kista must read it again
		reads, conds := k.reads, k.conds
		if r := en.sync(t, false); r.Counts["revocations_unknown"] != 1 || r.Counts["revoked"] != 0 || k.reads != reads+1 || k.conds != conds {
			t.Fatalf("the next run: %+v (reads %d, 304s %d)", r, k.reads-reads, k.conds-conds)
		}

		// a stored stronger state is never lowered by a state this kista does not know, and a word
		// that is not one is shown under a fixed label; a complete read of such rows forgets nothing
		k.set(row(v10, "recalled", "CVE-1"), row(v11, store.RevokeBlocked, "bad"))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 {
			t.Fatalf("a block: %+v", r)
		}
		k.set(row(v10, "Recalled For Good!", ""), row(v11, "quarantined", "worse"))
		r = en.sync(t, false)
		s := en.stored(t)
		if r.Counts["revocations_unknown"] != 2 || r.Counts["revoked"] != 0 || len(s) != 2 {
			t.Fatalf("two unknown states: %+v %+v", r, s)
		}
		if x := s["1.1"]; x.State != store.RevokeBlocked || x.Reason != "bad" || !x.UnlistedAt.IsZero() || x.AppliedState != store.RevokeBlocked {
			t.Fatalf("a stored block met an unknown state: %+v", x)
		}
		if x := s["1.0"]; x.State != store.RevokeYanked || x.Reason != "unknown" || !x.UnlistedAt.IsZero() {
			t.Fatalf("a state that is no word: %+v", x)
		}
		if st := states(t, en, "prod", "tresor"); st["1.0"] != store.ReleaseYanked || st["1.1"] != store.ReleaseYanked {
			t.Fatalf("what was applied stays: %+v", st)
		}

		// "upgraded": the same rows arrive in states this kista knows, and apply by precedence
		k.set(row(v10, store.RevokeWithdrawn, "CVE-1"), row(v11, store.RevokeBlocked, "bad"))
		r = en.sync(t, false)
		if r.Counts["revoked"] != 1 || r.Counts["revocations_unknown"] != 0 || older() != "" {
			t.Fatalf("the real state: %+v, %q", r, older())
		}
		if x := en.stored(t)["1.0"]; x.State != store.RevokeWithdrawn || x.AppliedState != store.RevokeWithdrawn || x.Reason != "CVE-1" {
			t.Fatalf("the real state stored: %+v", x)
		}
	})
}

// Spec 0046: "api": 1 is frozen on the dash routes and its value is checked as a guard. A higher
// one fails the run with its reason: nothing is read as major 1, nothing stored is forgotten, and
// no name is taken or removed.
func TestDashRoutesNewerAPI(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		k := en.kista()
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		en.repo.mu.Lock()
		en.repo.names = []string{"tresor"}
		en.repo.mu.Unlock()
		sp := en.spec(store.UpstreamEntry{Name: "*"})
		sp.Revocations = true
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 || r.Counts["discovered"] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		c := en.releases(t, "tresor")[0]
		k.set(row(c, store.RevokeYanked, "old"))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 || len(en.stored(t)) != 1 {
			t.Fatalf("a yank: %+v", r)
		}

		// the revocations route answers a later major: its rows (none: everything would be forgotten
		// if read as 1) are not read
		k.set()
		k.api = 2
		r := en.sync(t, false)
		if !strings.Contains(r.Error, "an api newer than this kista reads") || len(r.Problems) != 0 {
			t.Fatalf("api 2 on -/revocations: %+v", r)
		}
		if s := en.stored(t); len(s) != 1 || !s["1.0"].UnlistedAt.IsZero() || s["1.0"].State != store.RevokeYanked {
			t.Fatalf("api 2 forgot what was stored: %+v", s)
		}
		u, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		if !u.RemoteKista || !u.Revocations || !strings.Contains(upstream.OlderThanSource(u.LastRun), "not read until an upgrade") {
			t.Fatalf("the upstream after api 2: %+v, %q", u, upstream.OlderThanSource(u.LastRun))
		}
		if st := states(t, en, "prod", "tresor"); st["1.0"] != store.ReleaseYanked {
			t.Fatalf("what was applied: %+v", st)
		}

		// the names route alone answers a later major: the run fails, the discovered names stay
		k.api = 0
		k.set(row(c, store.RevokeYanked, "old"))
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesAPI = []string{"other"}, 2
		en.repo.mu.Unlock()
		r = en.sync(t, false)
		if !strings.Contains(r.Error, "an api newer than this kista reads") {
			t.Fatalf("api 2 on -/names: %+v", r)
		}
		if got := en.discovered(t); !slices.Equal(got, []string{"tresor"}) {
			t.Fatalf("api 2 changed the names: %v", got)
		}
		// back at major 1 the run reads again
		en.repo.mu.Lock()
		en.repo.names, en.repo.namesAPI = []string{"tresor"}, 0
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Error != "" || r.Counts["discovered"] != 1 {
			t.Fatalf("api 1 again: %+v", r)
		}
	})
}

// Spec 0046 through an export feed: a statement the gateway could not read is kept under its
// waiting reason and carried without content, the reason its note. A site that can read it leaves it
// waiting too and asks for it again at every plan (level 0), until the gateway carries its content;
// one it cannot read waits by its own rules; with any other note a contentless statement is refused
// for good. A gateway's retake deletes its kept rows of the rewound range, so a statement withdrawn
// at the source meanwhile is not carried as waiting for ever.
func TestWaitingThroughExport(t *testing.T) {
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
			gwT, _ := g.st.GetTenant(ctx, "acme")
			gw := store.Feed{TenantID: gwT.ID, Name: "reviews", URL: feedURL, Credential: "src", Keys: []string{src.fp},
				Offer: store.OfferUpstreams, Export: true, CreatedBy: "os:1:t"}
			doc := func(text string) []byte {
				return []byte(`{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"Enterest",` +
					`"capabilities":[],"network":[],"files":[],"processes":[],"licences":["MIT"],"findings":[],"text":"` + text + `"}`)
			}
			// what the gateway's export feed holds about the body: a stored document (1); a statement
			// its older binary could not read, though this binary can (2); one refused there for its
			// own limits (3); one of a kind no binary here reads (4)
			b1, sig1, st1 := src.statement(1, "d1", body, doc("one"), nil)
			b2, sig2, st2 := src.statement(2, "d2", body, doc("two"), nil)
			b3, sig3, _ := src.statement(3, "d3", body, doc("three"), nil)
			b4, sig4, _ := src.statement(4, "d4", body, []byte(`{}`), func(st *attachment.Statement) {
				st.Kind, st.MediaType = "report", "application/vnd.kista.report.v1+json"
			})
			held := func(id string, seq int64, b []byte, sig string, st attachment.Statement, content []byte) {
				t.Helper()
				a := store.Attachment{ID: store.NewID(), TenantID: gwT.ID, BodyHash: body, Kind: st.Kind, MediaType: st.MediaType, Title: st.Title,
					Size: st.Size, ContentHash: st.ContentHash, Summary: "{}", Origin: store.AttachedByFeed, AttachedBy: gw.ID,
					Visibility: store.VisibleAdministrators, Issuer: feedURL, IssuerID: id, KeyFP: src.fp, SourceOrigin: "tenant",
					IssuedAt: time.Now(), Statement: string(b), Signature: sig, Seq: seq}
				must(t, func() error {
					return g.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAttachment(ctx, &a) })
				})
				if err := g.blob.PutAttachment(ctx, gwT.StorageDomain, a.ID, content); err != nil {
					t.Fatal(err)
				}
			}
			keep := func(seq int64, reason string, b []byte, sig string) {
				t.Helper()
				must(t, func() error {
					return g.st.InTx(ctx, "", func(tx *store.Tx) error {
						return tx.KeepFeedRefusal(ctx, gwT.ID, store.FeedRefusal{FeedID: gw.ID, BodyHash: body, Seq: seq, Reason: reason,
							Statement: string(b), Signature: sig})
					})
				})
			}
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error {
					if err := tx.InsertFeed(ctx, &gw); err != nil {
						return err
					}
					if err := tx.SetFeedKeysFile(ctx, gw.ID, keysFile); err != nil {
						return err
					}
					if err := tx.SetFeedCursor(ctx, gw.ID, body, 4); err != nil {
						return err
					}
					// the gateway's own waiting state, as its run left it: from 2, at its (older) level
					return tx.AddFeedWait(ctx, gw.ID, store.FeedWait{BodyHash: body, From: 2, Count: 2, Why: attachment.WaitKind, Level: 0})
				})
			})
			held("d1", 1, b1, sig1, st1, doc("one"))
			keep(2, attachment.WaitKind, b2, sig2)
			keep(3, "limits", b3, sig3)
			keep(4, attachment.WaitKind, b4, sig4)

			// the site's bundle-mode feed
			if _, err := feeds.Add(ctx, admin, "site", attachment.FeedSpec{Name: "enterest", URL: feedURL, Keys: []string{src.fp},
				Mode: store.FeedBundle}); err != nil {
				t.Fatal(err)
			}
			siteT, _ := g.st.GetTenant(ctx, "site")
			site, err := getFeed(t, g.env, siteT.ID, "enterest")
			if err != nil {
				t.Fatal(err)
			}
			feedRun := func(r upstream.ImportResult) attachment.RunResult {
				t.Helper()
				for _, tr := range r.Targets {
					if tr.Feed == "enterest" && tr.FeedRun != nil {
						if tr.Status != upstream.TargetImported {
							t.Fatalf("the feed target: %+v", tr)
						}
						return *tr.FeedRun
					}
				}
				t.Fatalf("no feed target: %+v", r.Targets)
				return attachment.RunResult{}
			}
			siteDocs := func() map[string]store.Attachment {
				t.Helper()
				as, err := g.st.BodyAttachments(ctx, siteT.ID, body)
				if err != nil {
					t.Fatal(err)
				}
				out := map[string]store.Attachment{}
				for _, a := range as {
					out[a.IssuerID] = a
				}
				return out
			}
			planSeq := func(p bundle.Plan) int64 {
				t.Helper()
				for _, b := range p.Feeds[0].Bodies {
					if b.BodyHash == body {
						return b.Seq
					}
				}
				return 0
			}

			// the first bundle: the files and the feed
			plan := g.plan(t)
			res, data := g.export(t, plan, upstream.ExportOptions{})
			if res.Failed() || res.Feeds[0].Counts["statements"] != 4 || res.Feeds[0].Counts["without_content"] != 3 ||
				res.Feeds[0].Counts["documents"] != 1 {
				t.Fatalf("the export: %+v", res.Feeds)
			}
			run := feedRun(g.importSite(t, data))
			why := map[string]string{}
			for _, r := range run.Refused {
				why[r.IssuerID] = r.Reason + "/" + r.Note
			}
			if run.Error != "" || run.Counts["received"] != 1 || run.Counts["waiting"] != 2 || run.Counts["refused"] != 1 ||
				why["d3"] != "bundle: no content/limits" {
				t.Fatalf("the import: %+v %v", run, why)
			}
			if d := siteDocs(); len(d) != 1 || d["d1"].Seq != 1 {
				t.Fatalf("stored at the site: %+v", d)
			}
			// the site's waiting state: from 2; a row has one level, the lower: 0, so every plan asks again
			if w, _ := g.st.FeedWaits(ctx, site.ID); w[body].From != 2 || w[body].Count != 2 || w[body].Level != 0 {
				t.Fatalf("the site's waiting state: %+v", w)
			}
			if cur, _ := g.st.FeedCursors(ctx, site.ID); cur[body] != 4 {
				t.Fatalf("the site's cursor: %v", cur)
			}
			var shown store.Feed
			if shown, err = feeds.Get(ctx, admin, "site", "enterest"); err != nil || shown.Waiting[attachment.WaitKind] != 2 || shown.Status != store.FeedOK {
				t.Fatalf("the site's feed: %+v %v", shown.Waiting, err)
			}

			// a plan after that: the cursor is back just below what waits, with no reset
			plan = g.plan(t)
			if seq := planSeq(plan); seq != 1 {
				t.Fatalf("the plan asks from %d", seq)
			}
			if w, _ := g.st.FeedWaits(ctx, site.ID); len(w) != 0 {
				t.Fatalf("the retake left waiting state: %+v", w)
			}
			// the gateway is as it was: the same bundle again, the same waits, nothing stored twice
			_, data = g.export(t, plan, upstream.ExportOptions{})
			run = feedRun(g.importSite(t, data))
			if run.Counts["received"] != 0 || run.Counts["waiting"] != 2 || len(siteDocs()) != 1 {
				t.Fatalf("the same bundle again: %+v", run)
			}

			// the gateway is upgraded: its retake rewinds its cursor and deletes its kept rows from
			// what waited on; its run then stores d2, refuses d3 again, and the source no longer
			// offers d4 (withdrawn there since: a source sends only what it still offers)
			var rewound int
			must(t, func() error {
				return g.st.InTx(ctx, store.FeedLock(gwT.ID), func(tx *store.Tx) error {
					var err error
					rewound, err = tx.RetakeFeedCursors(ctx, gw.ID, attachment.ReaderLevel)
					return err
				})
			})
			if rs, _ := g.st.FeedRefusals(ctx, gw.ID, body, 0, 100); rewound != 1 || len(rs) != 0 {
				t.Fatalf("the gateway's retake: %d bodies, kept %+v", rewound, rs)
			}
			if cur, _ := g.st.FeedCursors(ctx, gw.ID); cur[body] != 1 {
				t.Fatalf("the gateway's cursor after its retake: %v", cur)
			}
			held("d2", 2, b2, sig2, st2, doc("two"))
			keep(3, "limits", b3, sig3)
			must(t, func() error {
				return g.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, gw.ID, body, 4) })
			})

			plan = g.plan(t)
			if seq := planSeq(plan); seq != 1 {
				t.Fatalf("the next plan asks from %d", seq)
			}
			res, data = g.export(t, plan, upstream.ExportOptions{})
			if res.Failed() || res.Feeds[0].Counts["statements"] != 2 || res.Feeds[0].Counts["documents"] != 1 {
				t.Fatalf("the export after the gateway's upgrade: %+v", res.Feeds)
			}
			run = feedRun(g.importSite(t, data))
			if run.Error != "" || run.Counts["received"] != 1 || run.Counts["waiting"] != 0 || run.Counts["refused"] != 1 {
				t.Fatalf("the import after the gateway's upgrade: %+v", run)
			}
			if d := siteDocs(); len(d) != 2 || d["d2"].Seq != 2 || !bytes.Contains([]byte(d["d2"].Statement), []byte(`"issuer_id":"d2"`)) {
				t.Fatalf("stored at the site: %+v", d)
			}
			if w, _ := g.st.FeedWaits(ctx, site.ID); len(w) != 0 {
				t.Fatalf("the site waits still: %+v", w)
			}
			// no further than the last statement verified in the bundle (the gateway read to 4)
			if cur, _ := g.st.FeedCursors(ctx, site.ID); cur[body] != 3 {
				t.Fatalf("the site's cursor: %v", cur)
			}
		})
	}
}

// newerLicense rewrites a licence into a later format: its payload's v raised to 2 and, with header,
// a member more in its header (a format this kista does not read may change that too). Its
// signature no longer verifies: nothing of such a token is verified here.
func newerLicense(t *testing.T, token string, header bool) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact token: %q", token)
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[1])
	if err != nil || !bytes.Contains(payload, []byte(`"v":1,`)) {
		t.Fatalf("the payload: %s %v", payload, err)
	}
	parts[1] = enc.EncodeToString(bytes.Replace(payload, []byte(`"v":1,`), []byte(`"v":2,"seats":{"max":5},`), 1))
	if header {
		h, err := enc.DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		parts[0] = enc.EncodeToString(append(bytes.TrimSuffix(h, []byte("}")), []byte(`,"crit":["seats"]}`)...))
	}
	return strings.Join(parts, ".")
}

// Spec 0046 at the licence relay: a token in a newer format is refused (v), and the licence stored
// for its name stays, since a token this kista cannot read is not a name left out; a name really
// left out is still removed. An import refuses such a token by the same rule.
func TestLicenseNewerFormat(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lic := newLicensor(t)
			for i, name := range []string{"tresor", "geo"} {
				en.repo.put("v2.0.0", "linux_amd64", name, signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, name+"_duckdb_cpp_init"))
				if i == 0 {
					sp := en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "geo"})
					sp.Credential = "sub"
					if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
						t.Fatal(err)
					}
				}
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 {
				t.Fatalf("a run: %+v", r)
			}
			u, err := en.up.Get(ctx, admin, "acme", "acme-repo")
			if err != nil {
				t.Fatal(err)
			}
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
				Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			now := en.st.Now().Unix()
			day := int64(24 * 3600)
			run := func() licenses.Result {
				t.Helper()
				res, err := svc.RunUpstream(ctx, u.ID)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			held := func() map[string]string {
				t.Helper()
				ls, err := en.st.ListLicenses(ctx, en.tenantID(t), pkgtype.DuckDBExtension)
				if err != nil {
					t.Fatal(err)
				}
				out := map[string]string{}
				for _, l := range ls {
					out[l.Name] = l.Token
				}
				return out
			}
			tresor, geo := lic.sign("tresor", "pro", now-60, now+30*day), lic.sign("geo", "pro", now-60, now+30*day)
			en.answer(t, tresor, geo)
			if res := run(); res.Counts["stored"] != 2 || res.Error != "" {
				t.Fatalf("the first relay: %+v", res)
			}

			// the source moved to a newer format for both names: refused as v, counted, nothing removed
			tresor2 := newerLicense(t, lic.sign("tresor", "pro", now-30, now+60*day), false)
			geo2 := newerLicense(t, lic.sign("geo", "pro", now-30, now+60*day), true)
			en.answer(t, tresor2, geo2)
			res := run()
			if res.Error != "" || res.Counts["refused"] != 2 || res.Counts["newer"] != 2 || res.Counts["removed"] != 0 || res.Counts["stored"] != 0 {
				t.Fatalf("a newer format: %+v", res)
			}
			for _, r := range res.Refused {
				if r.Rule != license.RuleV || (r.Name != "tresor" && r.Name != "geo") {
					t.Fatalf("the refusal: %+v", r)
				}
			}
			if h := held(); h["tresor"] != tresor || h["geo"] != geo {
				t.Fatalf("the stored licences after a newer format: %v", h)
			}
			if evs := en.events(t, "license.remove"); len(evs) != 0 {
				t.Fatalf("a licence was reported removed: %+v", evs)
			}

			// a name left out is removed all the same; the newer token of the other keeps it
			en.answer(t, tresor2)
			res = run()
			if res.Counts["removed"] != 1 || res.Counts["newer"] != 1 {
				t.Fatalf("a name left out: %+v", res)
			}
			if h := held(); len(h) != 1 || h["tresor"] != tresor {
				t.Fatalf("the stored licences after a name left out: %v", h)
			}
			// a payload whose v and ext cannot be read keeps nothing
			en.answer(t, "e30.e30.e30")
			if res := run(); res.Counts["removed"] != 1 || res.Counts["newer"] != 0 || len(held()) != 0 {
				t.Fatalf("an unreadable token: %+v", res)
			}

			// the same rule wherever a token is read
			if _, err := license.Parse(geo2); err == nil || !strings.Contains(err.Error(), "format version 2") {
				t.Fatalf("a newer token parsed: %v", err)
			}
			var le *license.Error
			if _, err := license.Parse(geo2); !errors.As(err, &le) || le.Rule != license.RuleV {
				t.Fatalf("a newer token with another header: %v", err)
			}
			if ext, ok := license.Newer(tresor2); !ok || ext != "tresor" {
				t.Fatalf("the name of a newer token: %q %v", ext, ok)
			}
			if _, ok := license.Newer(tresor); ok {
				t.Fatal("a v1 token read as a newer one")
			}
		})
	}
}

// Spec 0046 at the exporter: an older gateway reads a newer site's plan. A member it does not know
// is ignored, a feed's kind it does not know is a kind by its grammar, and a source of a package
// type it does not know is left out by itself, with its problem; the rest is exported.
func TestExportNewerPlan(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.put(t, "tresor", "1.0", 1)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			var m map[string]any
			if err := json.Unmarshal(planBytes(t, g.plan(t)), &m); err != nil {
				t.Fatal(err)
			}
			m["accepts_reports"] = map[string]any{"envelope": 2}
			m["feeds"] = []any{map[string]any{"url": feedURL, "keys": []any{"sha256:" + strings.Repeat("9", 64)}, "kinds": []any{"report", "review"},
				"sources": []any{}, "bodies": []any{}, "via": "relay"}}
			// before the repository source, in canonical order; its members in a shape of its own
			m["sources"] = append([]any{map[string]any{"kind": "adbc-registry", "package_type": "adbc_driver",
				"prefix": "https://drivers.example.org/adbc", "keys": map[string]any{"cosign": true}, "cells": []any{map[string]any{"driver": "postgresql"}}}},
				m["sources"].([]any)...)
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			res, err := g.up.Export(ctx, admin, "acme", &buf, upstream.ExportOptions{Plan: raw, Key: g.priv, Holder: "test", Version: "kista test"})
			if err != nil {
				t.Fatalf("the export of a newer site's plan: %v", err)
			}
			if len(res.Sources) != 2 || !strings.Contains(res.Sources[0].Error, "package type adbc_driver is not known here") ||
				res.Sources[0].Kind != "adbc-registry" || res.Sources[1].Error != "" || res.Sources[1].Counts[bundle.Fetched] != 1 {
				t.Fatalf("the sources: %+v", res.Sources)
			}
			// the feed fails alone, for its own reason (no export feed of the URL here), not for its kinds
			if len(res.Feeds) != 1 || res.Feeds[0].Error != "not subscribed here" {
				t.Fatalf("the feed: %+v", res.Feeds)
			}
			// the bundle carries the known source, and the site takes it
			if r := g.importSite(t, buf.Bytes()); len(r.Targets) != 1 || r.Targets[0].Status != upstream.TargetImported ||
				r.Targets[0].Result.Counts[store.CellReleased] != 1 || len(r.LeftOut) != 0 {
				t.Fatalf("the import: %+v", r)
			}
		})
	}
}
