package upstream_test

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0035: licences per catalogue entry, on one engine (the store suite covers the three). A
// distributor's publisher and a pull-through "*" upstream of one name: the distributor's licence
// imports into its entry, named, with no other act; a write names its entry, which must declare
// the key; the keys listing is what an import accepts (held, placed early, available); a copied key
// under another source gets nothing and takes nothing; a mirror relays into its own entry beside the
// distributor's licence, a second upstream of the remote into its own, held first, each storing its
// copy and revoking only its own; an import is replaced by a relayed licence of its key that lasts
// longer, then governed by the relay; removals keep imports under the id form, and a successor
// starts with nothing; a licence manager writes distributors' publishers' entries only.
func TestLicenseSources(t *testing.T) {
	en, tokenFile := privateEnv(t, storetest.Engines(t)[0])
	// a store clock half a day back: moved forward by hours, a blob commit's claim (until a deadline
	// on the real clock) stays live
	clock := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)
	en.st.Now = func() time.Time { return clock }
	en.repo.token = "T-sub"
	if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lic := newLicensor(t)
	kid := extfile.Fingerprint(&lic.key.PublicKey)
	pemKey, err := extfile.MarshalPublicKeyPEM(&lic.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
	sp := en.spec(store.UpstreamEntry{Name: "tresor"})
	sp.Credential = "sub"
	u, err := en.up.Add(ctx, admin, "acme", sp)
	if err != nil {
		t.Fatal(err)
	}
	if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
		t.Fatalf("a run: %+v", r)
	}
	// a pull-through upstream of every name, in channel other
	pt := en.spec(store.UpstreamEntry{Name: "*"})
	pt.Name, pt.Channel, pt.Mode = "pt", "other", store.ModePullThrough
	if _, err := en.up.Add(ctx, admin, "acme", pt); err != nil {
		t.Fatal(err)
	}
	svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
		Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
	tid, ext := en.tenantID(t), pkgtype.DuckDBExtension
	now := clock.Unix()
	day := int64(24 * 3600)
	us, ul := store.UpstreamSource(u.ID), "upstream:acme-repo"
	other, err := en.st.GetChannel(ctx, "acme", "other")
	if err != nil {
		t.Fatal(err)
	}

	// publishers declaring tresor's key on builds of their own: a distributor's (Enterest's, the
	// genuine product), released in other, and a tenant's copycat's, in the catalogue only
	publisher := func(name, distributor string) store.Publisher {
		t.Helper()
		p := store.Publisher{TenantID: tid, Name: name, CreatedBy: "os:1:t"}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.InsertPublisher(ctx, &p); err != nil {
					return err
				}
				if distributor != "" {
					if _, err := tx.SetPublisherDistributor(ctx, tid, name, distributor); err != nil {
						return err
					}
				}
				_, err := tx.SetPublisherLicenseKeys(ctx, tid, name, true)
				return err
			})
		})
		return p
	}
	n := 0
	build := func(p store.Publisher, name string, ch *store.Channel, held time.Duration) store.Build {
		t.Helper()
		n++
		b := store.Build{TenantID: tid, Source: store.PublisherSource(p.ID), Name: name, PackageVersion: fmt.Sprintf("1.%d", n),
			Platform: "linux_amd64", ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: fmt.Sprintf("%064x", 1000+n),
			Origin: store.OriginPublication, CreatedBy: "os:1:t", Checked: true}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		})
		if held > 0 {
			must(t, func() error {
				return store.ExecRaw(ctx, en.st, "UPDATE builds SET available_at = ? WHERE id = ?", store.TimeArg(en.st, clock.Add(held)), b.ID)
			})
		}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if _, err := tx.RecordLicenseKeys(ctx, tid, b.ID, store.KeySourcePublisher, p.ID, []store.LicenseKey{{Kid: kid, PEM: pemKey}}); err != nil {
					return err
				}
				if ch == nil {
					return nil
				}
				r := store.Release{TenantID: tid, ChannelID: ch.ID, BuildID: b.ID, Name: name, Source: b.Source, PackageVersion: b.PackageVersion,
					Platform: "linux_amd64", Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPublication}
				if err := tx.InsertRelease(ctx, &r, "os:1:t"); err != nil {
					return err
				}
				_, err := tx.SetChannelSource(ctx, ch.ID, name, b.Source, "os:1:t")
				return err
			})
		})
		return b
	}
	dist, copycat := publisher("enterest-ci", "enterest"), publisher("copycat", "")
	ds, cs := store.PublisherSource(dist.ID), store.PublisherSource(copycat.ID)
	build(dist, "tresor", &other, 0)
	build(copycat, "tresor", nil, 0)
	// served is what a channel serving tresor from an entry serves: the entry's licence (Serve), while
	// a live build of the entry declares its key (the route's own check, against its snapshot)
	served := func(src string) string {
		t.Helper()
		l, ok, err := svc.Serve(ctx, tid, ext, "tresor", src)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		live := false
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				keys, err := tx.LiveLicenseKeys(ctx, tid, ext, "tresor", src)
				for _, k := range keys {
					live = live || k.Kid == l.Key
				}
				return err
			})
		})
		if !live {
			return ""
		}
		return l.Token
	}
	rule := func(err error) string {
		var le *license.Error
		if errors.As(err, &le) {
			return le.Rule
		}
		return fmt.Sprint(err)
	}

	// a write names its entry, which must be a live source that can carry a licence
	bought := lic.sign("tresor", "ent", now-60, now+30*day)
	for _, bad := range []string{"", "tenant", "upstream:nope", "publisher-id:", "x"} {
		if _, err := svc.Import(ctx, admin, "acme", "", bad, bought, licenses.Create); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an import into %q: %v", bad, err)
		}
	}
	// an entry whose builds do not declare the key: kid_untrusted, naming it
	if _, err := svc.Import(ctx, admin, "acme", "", "upstream:pt", bought, licenses.Create); rule(err) != license.RuleKeyUntrusted ||
		!strings.Contains(err.Error(), "upstream:pt") {
		t.Fatalf("an import into an entry without the key: %v", err)
	}
	// the regression case: the distributor's licence imports into its entry beside a pull-through
	// "*" upstream, with no other act; a licence manager writes it too
	lm := authz.Actor{Kind: authz.ActorLicenseManager, ID: "ops|reconciler"}
	lsvc := svc.WithAuthz(authz.Grants{Store: en.st})
	v, err := lsvc.Import(ctx, lm, "acme", "", "publisher:enterest-ci", bought, licenses.Create)
	if err != nil || v.Entry != "publisher:enterest-ci" || !v.Created {
		t.Fatalf("the distributor's licence: %+v %v", v, err)
	}
	if served(ds) != bought {
		t.Fatal("the distributor's entry serves its licence")
	}
	// but not a tenant's entry, whatever its keys: 403, nothing stored
	if _, err := lsvc.Import(ctx, lm, "acme", "", "publisher:copycat", bought, licenses.Create); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("a licence manager's import into a tenant publisher's entry: %v", err)
	}
	if _, err := lsvc.Import(ctx, lm, "acme", "", ul, bought, licenses.Create); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("a licence manager's import into an upstream's entry: %v", err)
	}
	// a copied key gets nothing, and takes nothing from the genuine entry; the keys listing shows
	// the kid live for every entry declaring it
	if served(cs) != "" || served(ds) != bought {
		t.Fatalf("the copy's %q, the distributor's %q", served(cs), served(ds))
	}
	keys := func(name, source string) string {
		t.Helper()
		nks, err := svc.LicenseKeys(ctx, admin, "acme", "", name, source)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, nk := range nks {
			for _, k := range nk.Keys {
				for _, s := range k.Sources {
					out = append(out, s.Kind+":"+s.Name+"="+fmt.Sprint(s.Builds)+s.Distributor)
				}
			}
		}
		return strings.Join(out, ",")
	}
	if got := keys("tresor", ""); got != "publisher:copycat=1,publisher:enterest-ci=1enterest,upstream:acme-repo=1" {
		t.Fatalf("tresor's keys: %q", got)
	}

	// the listing matches what an import accepts: a held build's key is neither; placed early, both;
	// its hold ended and its release yanked, the newest available version keeps it
	geo := build(dist, "geo", nil, time.Hour)
	geoLic := lic.sign("geo", "pro", now-60, now+30*day)
	if got := keys("geo", "publisher:enterest-ci"); got != "" {
		t.Fatalf("a held build's key listed: %q", got)
	}
	if _, err := svc.Import(ctx, admin, "acme", "", "publisher:enterest-ci", geoLic, licenses.Create); rule(err) != license.RuleKeyUntrusted {
		t.Fatalf("an import against a held build's key: %v", err)
	}
	var early store.Release
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			early = store.Release{TenantID: tid, ChannelID: other.ID, BuildID: geo.ID, Name: "geo", Source: ds, PackageVersion: geo.PackageVersion,
				Platform: "linux_amd64", Slot: geo.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPublication}
			if err := tx.InsertRelease(ctx, &early, "os:1:t"); err != nil {
				return err
			}
			_, err := tx.SetChannelSource(ctx, other.ID, "geo", ds, "os:1:t")
			return err
		})
	})
	if got := keys("geo", "publisher:enterest-ci"); got != "publisher:enterest-ci=1enterest" {
		t.Fatalf("placed early, not listed: %q", got)
	}
	if _, err := svc.Import(ctx, admin, "acme", "", "publisher:enterest-ci", geoLic, licenses.Create); err != nil {
		t.Fatalf("an import against a key placed early: %v", err)
	}
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &early, store.ReleaseYanked, "os:1:t") })
	})
	if got := keys("geo", "publisher:enterest-ci"); got != "" {
		t.Fatalf("yanked while held, still listed: %q", got)
	}
	clock = clock.Add(61 * time.Minute)
	if got := keys("geo", "publisher:enterest-ci"); got != "publisher:enterest-ci=1enterest" {
		t.Fatalf("available, not listed: %q", got)
	}
	if _, err := svc.Import(ctx, admin, "acme", "", "publisher:enterest-ci", geoLic, 0); err != nil {
		t.Fatalf("an import against an available build's key: %v", err)
	}

	// the mirror relays into its own entry, beside the distributor's licence of the same key
	relayed := lic.sign("tresor", "pro", now-60, now+30*day)
	en.answer(t, relayed)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["stored"] != 1 || len(res.Refused) != 0 {
		t.Fatalf("the relay: %+v %v", res, err)
	}
	if served(us) != relayed || served(ds) != bought || served(cs) != "" {
		t.Fatalf("after the relay: the mirror's %q, the distributor's %q, the copy's %q", served(us), served(ds), served(cs))
	}

	// a second upstream of the remote, held, mirroring into its own channel: its relay is refused
	// (not_supplied: a held build supplies nothing, and no key of its entry is live), then stores its
	// own copy once the hold ends
	if err := signer.GenerateKeyFile(filepath.Join(en.dir, "nightly.pem")); err != nil {
		t.Fatal(err)
	}
	must(t, func() error {
		_, err := en.ten.CreateChannel(ctx, admin, "acme", "nightly", store.ChannelSigned, "")
		return err
	})
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "nightly", []string{"v2.0.0"}, nil)
		return err
	})
	must(t, func() error { _, err := en.ks.Add(ctx, admin, "acme", "nightly", "file:nightly.pem", true); return err })
	sp2 := en.spec(store.UpstreamEntry{Name: "tresor"})
	sp2.Name, sp2.Channel, sp2.Credential = "acme-nightly", "nightly", "sub"
	u2, err := en.up.Add(ctx, admin, "acme", sp2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := en.up.SetHold(ctx, admin, "acme", "acme-nightly", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	us2 := store.UpstreamSource(u2.ID)
	syncNightly := func() {
		t.Helper()
		if _, err := en.up.Sync(ctx, admin, "acme", "acme-nightly", false); err != nil {
			t.Fatal(err)
		}
		en.run.Once(ctx)
	}
	syncNightly()
	if res, err := svc.RunUpstream(ctx, u2.ID); err != nil || res.Counts["stored"] != 0 || len(res.Refused) != 1 ||
		res.Refused[0].Rule != "not_supplied" || served(us2) != "" {
		t.Fatalf("the held upstream's relay: %+v %v", res, err)
	}
	clock = clock.Add(61 * time.Minute)
	if res, err := svc.RunUpstream(ctx, u2.ID); err != nil || res.Counts["stored"] != 1 || served(us2) != relayed || served(us) != relayed {
		t.Fatalf("after its hold: %+v %v", res, err)
	}
	// a renewal reaching either first: each entry takes it at its own run
	renewed := lic.sign("tresor", "pro", now, now+60*day)
	en.answer(t, renewed)
	if res, err := svc.RunUpstream(ctx, u2.ID); err != nil || res.Counts["replaced"] != 1 || served(us2) != renewed || served(us) != relayed {
		t.Fatalf("the renewal through the second upstream first: %+v %v", res, err)
	}
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["replaced"] != 1 || served(us) != renewed {
		t.Fatalf("the renewal through the first: %+v %v", res, err)
	}
	// each relay's revocation takes its own entry's licence only
	en.answer(t)
	if res, err := svc.RunUpstream(ctx, u2.ID); err != nil || res.Counts["removed"] != 1 || served(us2) != "" || served(us) != renewed ||
		served(ds) != bought {
		t.Fatalf("the second upstream's revocation: %+v %v", res, err)
	}
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["removed"] != 1 || served(us) != "" || served(ds) != bought {
		t.Fatalf("the first's revocation: %+v %v", res, err)
	}

	// an import in the mirror's entry: a relayed licence of its key that does not last longer leaves
	// it; one that does replaces it, and the relay governs the row from then on
	imported := lic.sign("tresor", "own", now, now+70*day)
	if v, err := svc.Import(ctx, admin, "acme", "", ul, imported, licenses.Create); err != nil || v.Entry != ul || v.Source != store.LicenseImport {
		t.Fatalf("an import into the mirror's entry: %+v %v", v, err)
	}
	en.answer(t, renewed)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || len(res.Refused) != 1 || res.Refused[0].Rule != "superseded" || served(us) != imported {
		t.Fatalf("a relayed licence over an import that lasts longer: %+v %v", res, err)
	}
	longer := lic.sign("tresor", "pro", now, now+90*day)
	en.answer(t, longer)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["replaced"] != 1 || served(us) != longer {
		t.Fatalf("a relayed licence lasting longer than the import: %+v %v", res, err)
	}
	if v, err := svc.Get(ctx, admin, "acme", "", "tresor", ul); err != nil || v.Source != store.LicenseUpstream || v.Upstream != "acme-repo" {
		t.Fatalf("the row is the relay's: %+v %v", v, err)
	}
	if evs, err := en.st.ListEvents(ctx, tid, store.EventFilter{Kind: "license.change", Limit: 1}); err != nil || len(evs) != 1 ||
		!strings.Contains(evs[0].Data, `"replaced":"lic-`) || !strings.Contains(evs[0].Data, `"upstream":"acme-repo"`) ||
		!strings.Contains(evs[0].Data, `"source":"upstream:acme-repo"`) {
		t.Fatalf("the import replaced by the relay, told: %+v %v", evs, err)
	}
	en.answer(t)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["removed"] != 1 || served(us) != "" {
		t.Fatalf("governed by the relay: %+v %v", res, err)
	}

	// removals. The second upstream's: its relayed licence goes with it, and its successor (the
	// upstream added again) is another entry, starting with nothing
	en.answer(t, renewed)
	if res, err := svc.RunUpstream(ctx, u2.ID); err != nil || res.Counts["stored"] != 1 {
		t.Fatalf("the second upstream's relay again: %+v %v", res, err)
	}
	if err := en.up.Remove(ctx, admin, "acme", "acme-nightly", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, admin, "acme", "", "tresor", "upstream-id:"+u2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a removed upstream's relayed licence: %v", err)
	}
	// an import in the removed upstream's entry (put there as one the migration converted would be):
	// its successor gets nothing from it, and neither entry serves it
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			l := store.License{TenantID: tid, PackageType: ext, Name: "tresor", Entry: us2, LicenseID: "lic-old", Issuer: "i", HolderID: "acme",
				Plan: "old", Token: renewed, Key: kid, IssuedAt: now, ExpiresAt: now + 60*day, GraceUntil: now + 61*day,
				Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:t"}
			return tx.PutLicense(ctx, &l, 0)
		})
	})
	u3, err := en.up.Add(ctx, admin, "acme", sp2)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := svc.RunUpstream(ctx, u3.ID); err != nil || res.Counts["stored"] != 0 || len(res.Refused) != 1 ||
		res.Refused[0].Rule != "not_supplied" || served(store.UpstreamSource(u3.ID)) != "" || served(us2) != "" {
		t.Fatalf("the successor's relay: %+v %v", res, err)
	}
	if n, err := store.CountRaw(ctx, en.st, "licenses", "entry_source = ?", store.UpstreamSource(u3.ID)); err != nil || n != 0 {
		t.Fatalf("the successor's entry: %d %v", n, err)
	}
	if err := svc.Remove(ctx, admin, "acme", "", "tresor", "upstream-id:"+u2.ID, 0); err != nil {
		t.Fatal(err)
	}
	// the mirror's: an import in its entry stays, under the id form, live for no key, removable by it
	if _, err := svc.Import(ctx, admin, "acme", "", ul, imported, licenses.Create); err != nil {
		t.Fatal(err)
	}
	if err := en.up.Remove(ctx, admin, "acme", "acme-repo", 0); err != nil {
		t.Fatal(err)
	}
	gone := "upstream-id:" + u.ID
	if vs, err := svc.List(ctx, admin, "acme", "", gone); err != nil || len(vs) != 1 || vs[0].Entry != gone {
		t.Fatalf("a removed upstream's import: %+v %v", vs, err)
	}
	if got := keys("tresor", ""); strings.Contains(got, "upstream:") || served(us) != "" {
		t.Fatalf("a removed upstream's keys: %q", got)
	}
	// served nowhere, so no bundle carries it either
	if ls, err := svc.Exportable(ctx, tid, ext, ""); err != nil || slices.ContainsFunc(ls, func(l store.License) bool { return l.Entry == us }) ||
		!slices.ContainsFunc(ls, func(l store.License) bool { return l.Entry == ds }) {
		t.Fatalf("what a bundle may carry: %+v %v", ls, err)
	}
	if _, err := svc.Import(ctx, admin, "acme", "", gone, imported, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("an import into a removed source's entry: %v", err)
	}
	if err := svc.Remove(ctx, admin, "acme", "", "tresor", gone, 0); err != nil {
		t.Fatalf("removing a removed upstream's import: %v", err)
	}
	// a removed publisher's import: kept, served nowhere (its keys are gone), deletable by its id form
	if v, err := svc.Import(ctx, admin, "acme", "", "publisher:copycat", bought, licenses.Create); err != nil || v.Entry != "publisher:copycat" {
		t.Fatalf("the organisation's purchase in the copier's entry: %+v %v", v, err)
	}
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tid, "copycat", "os:1:t") })
	})
	gone = "publisher-id:" + copycat.ID
	if vs, err := svc.List(ctx, admin, "acme", "", gone); err != nil || len(vs) != 1 || served(cs) != "" {
		t.Fatalf("a removed publisher's import: %+v %v", vs, err)
	}
	if got := keys("tresor", ""); got != "publisher:enterest-ci=1enterest" {
		t.Fatalf("after the removals, tresor's keys: %q", got)
	}
	if err := svc.Remove(ctx, admin, "acme", "", "tresor", gone, 0); err != nil {
		t.Fatalf("removing a removed publisher's import: %v", err)
	}
	if err := svc.Remove(ctx, admin, "acme", "", "tresor", gone, 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a removed publisher's entry without a licence: %v", err)
	}
	// nothing passed to another entry: the distributor's licence is as it was
	if vs, err := svc.List(ctx, admin, "acme", "", ""); err != nil || len(vs) != 2 || served(ds) != bought {
		t.Fatalf("the licences left: %+v %v", vs, err)
	}
	// the distributor's publisher removed: a licence manager removes its licence by the id form,
	// and names nothing else (a refused caller, whatever the source is)
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tid, "enterest-ci", "os:1:t") })
	})
	for _, bad := range []string{"publisher:enterest-ci", "publisher:nope", "upstream:pt", "upstream-id:" + u.ID, "publisher-id:" + copycat.ID} {
		if err := lsvc.Remove(ctx, lm, "acme", "", "tresor", bad, 0); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a licence manager's removal from %s: %v", bad, err)
		}
	}
	if _, err := lsvc.SetVisibility(ctx, lm, "acme", "", "tresor", "publisher-id:"+dist.ID, store.Private, 0); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("a licence manager's change in a removed publisher's entry: %v", err)
	}
	if err := lsvc.Remove(ctx, lm, "acme", "", "tresor", "publisher-id:"+dist.ID, 0); err != nil {
		t.Fatalf("a licence manager's removal from a removed distributor's publisher's entry: %v", err)
	}
}
