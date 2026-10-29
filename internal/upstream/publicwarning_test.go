package upstream_test

import (
	"context"
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
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0035 phase 2: an entry that holds a licence served publicly warns, advisory, once per act,
// source, name and channel, whatever the platforms: a release made public, a placement, a
// promotion, a source switch, an auto rule's and a follow's placements, a licence imported or made
// public into an entry with public releases, a relay, a successor taking releases. A private release of a licensed entry warns
// nothing, nor a public release of an unlicensed one, nor a licence into an entry without public
// releases. (A bundle's licence: TestBundleLicenses.)
func TestLicensePublicWarning(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) { testLicensePublicWarning(t, e) })
	}
}

func testLicensePublicWarning(t *testing.T, e storetest.Engine) {
	en, tokenFile := privateEnv(t, e)
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
	for _, ch := range []string{"third", "fourth", "auto", "follows"} {
		must(t, func() error {
			_, err := en.ten.CreateChannel(ctx, admin, "acme", ch, store.ChannelSigned, "")
			return err
		})
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", ch, []string{"v2.0.0"}, nil)
			return err
		})
		if err := signer.GenerateKeyFile(filepath.Join(en.dir, ch+".pem")); err != nil {
			t.Fatal(err)
		}
		must(t, func() error { _, err := en.ks.Add(ctx, admin, "acme", ch, "file:"+ch+".pem", true); return err })
	}
	svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
		Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
	tid := en.tenantID(t)
	now, day := clock.Unix(), int64(24*3600)

	// a distributor's publisher with tresor 1.0 in the catalogue, on two platforms, declaring the
	// licence's key
	dist := store.Publisher{TenantID: tid, Name: "enterest-ci", CreatedBy: "os:1:t", Distributor: "enterest"}
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &dist); err != nil {
				return err
			}
			if _, err := tx.SetDistributorHold(ctx, tid, 0); err != nil { // spec 0037: its builds live at once
				return err
			}
			_, err := tx.SetPublisherLicenseKeys(ctx, tid, dist.Name, true)
			return err
		})
	})
	for i, platform := range []string{"linux_amd64", "osx_arm64"} {
		b := store.Build{TenantID: tid, Source: store.PublisherSource(dist.ID), Name: "tresor", PackageVersion: "1.0", Platform: platform,
			ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: fmt.Sprintf("%064x", 7000+i), Visibility: store.Public,
			Origin: store.OriginPublication, CreatedBy: "os:1:t", Checked: true}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
					return err
				}
				_, err := tx.RecordLicenseKeys(ctx, tid, b.ID, store.KeySourcePublisher, dist.ID, []store.LicenseKey{{Kid: kid, PEM: pemKey}})
				return err
			})
		})
	}
	const ds = "publisher:enterest-ci"

	// act runs one act as a route does (its warnings collected) and checks what it warned: the
	// warnings answered (unless background: an act that answers nobody), and one event each, of
	// the act, naming the source and the channel
	seen, background := 0, false
	act := func(what, wantAct, source string, channels []string, f func(context.Context) error) {
		t.Helper()
		wctx := store.WithLicenseWarnings(ctx)
		if err := f(wctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		ws := store.LicenseWarnings(wctx)
		var got []string
		for _, w := range ws {
			if w.Name != "tresor" || w.Source != source || w.PackageType != pkgtype.DuckDBExtension || w.TenantID != tid ||
				!strings.Contains(w.Message, "advisory") {
				t.Errorf("%s: a warning %+v", what, w)
			}
			got = append(got, w.Channel)
		}
		slices.Sort(got)
		if !background && !slices.Equal(got, channels) || background && len(got) != 0 {
			t.Fatalf("%s: warned in %v, want %v", what, got, channels)
		}
		evs, err := en.st.ListEvents(ctx, tid, store.EventFilter{Kind: string(store.LicenseWarningKind), Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		fresh := evs[:len(evs)-seen]
		seen = len(evs)
		got = nil
		for _, ev := range fresh {
			if !strings.Contains(ev.Data, `"act":"`+wantAct+`"`) || !strings.Contains(ev.Data, `"source":"`+source+`"`) ||
				!strings.Contains(ev.Data, `"name":"tresor"`) || strings.Contains(ev.Data, "package_type") || ev.Actor == "" ||
				!strings.HasPrefix(ev.Subject, "channel:") {
				t.Errorf("%s: an event %+v", what, ev)
			}
			got = append(got, strings.TrimSuffix(strings.TrimPrefix(ev.Subject, "channel:"), "/ext:tresor"))
		}
		slices.Sort(got)
		if !slices.Equal(got, channels) {
			t.Fatalf("%s: events in %v, want %v: %+v", what, got, channels, fresh)
		}
	}

	// a licence into an entry with no public release: nothing
	bought := lic.sign("tresor", "ent", now-60, now+30*day)
	var bv licenses.View
	act("an import into an entry not served", "", ds, nil, func(c context.Context) (err error) {
		bv, err = svc.Import(c, admin, "acme", "", ds, bought, licenses.Create)
		return err
	})
	// a private placement of the licensed entry, on two platforms: nothing
	var placed []store.Release
	act("a private placement", "", ds, nil, func(c context.Context) (err error) {
		placed, _, err = en.rel.Place(c, admin, "acme", "other", "tresor", release.PlaceOptions{Source: ds, Version: "1.0", Private: true})
		return err
	})
	if len(placed) != 2 {
		t.Fatalf("placed: %+v", placed)
	}
	// one of them made public
	act("a release made public", "release.public", ds, []string{"other"}, func(c context.Context) error {
		_, err := en.rel.Apply(c, admin, "acme", "other", "tresor", placed[0].ID, release.SetPublic, 0)
		return err
	})
	// made public again (a replay): nothing changes, nothing warns; the private one made private
	// again is still a change, told (its build narrowed), and warns nothing
	act("a release made public again", "", ds, nil, func(c context.Context) error {
		r, err := en.rel.Apply(c, admin, "acme", "other", "tresor", placed[0].ID, release.SetPublic, 0)
		if err == nil && r.Version != 2 {
			err = fmt.Errorf("a replay changed the release: %+v", r)
		}
		return err
	})
	act("a release made private again", "", ds, nil, func(c context.Context) error {
		r, err := en.rel.Apply(c, admin, "acme", "other", "tresor", placed[1].ID, release.SetPrivate, 0)
		if err == nil && r.Version != 2 {
			err = fmt.Errorf("made private again, untold: %+v", r)
		}
		return err
	})
	// a public placement of two platforms: one warning
	act("a placement", "release.place", ds, []string{"third"}, func(c context.Context) error {
		_, _, err := en.rel.Place(c, admin, "acme", "third", "tresor", release.PlaceOptions{Source: ds, Version: "1.0"})
		return err
	})
	// a promotion of two platforms: one warning
	act("a promotion", "release.promote", ds, []string{"fourth"}, func(c context.Context) error {
		_, _, err := en.rel.Promote(c, admin, "acme", "fourth", "tresor", release.PromoteOptions{From: "third", Version: "1.0"})
		return err
	})
	// a source switch: away from the licensed entry nothing, back to it a warning
	act("a switch away", "", ds, nil, func(c context.Context) error {
		_, err := en.rel.SetSource(c, admin, "acme", "third", "tresor", store.SourceTenant, "")
		return err
	})
	act("a switch back", "source.set", ds, []string{"third"}, func(c context.Context) error {
		_, err := en.rel.SetSource(c, admin, "acme", "third", "tresor", ds, "")
		return err
	})
	// the licence made public, and replaced: each channel serving the entry publicly, once
	act("the licence made public", "license.change", ds, []string{"fourth", "other", "third"}, func(c context.Context) (err error) {
		bv, err = svc.SetVisibility(c, admin, "acme", "", "tresor", ds, store.Public, bv.Version)
		return err
	})
	act("a licence replaced", "license.import", ds, []string{"fourth", "other", "third"}, func(c context.Context) error {
		_, err := svc.Import(c, admin, "acme", "", ds, lic.sign("tresor", "ent", now, now+60*day), bv.Version)
		return err
	})
	// made private, it does not warn
	act("the licence made private", "", ds, nil, func(c context.Context) error {
		v, err := svc.Get(c, admin, "acme", "", "tresor", ds)
		if err == nil {
			_, err = svc.SetVisibility(c, admin, "acme", "", "tresor", ds, store.Private, v.Version)
		}
		return err
	})

	// automatic placements of two platforms, each build in a transaction of its own: an auto rule's
	// and a follow's, each warned once by its look (the rules themselves select an entry with no
	// release there yet: nothing)
	placer := &release.Placer{Service: en.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}
	act("an auto rule", "", ds, nil, func(c context.Context) error {
		_, err := en.rel.SetSource(c, admin, "acme", "auto", "tresor", ds, store.RuleAuto)
		return err
	})
	background = true // the placement pass's, in its own context
	act("the rule's placement", "release.place", ds, []string{"auto"}, func(context.Context) error {
		placer.Once(ctx, false)
		return nil
	})
	background = false
	act("a follow", "", ds, nil, func(c context.Context) error {
		_, err := en.rel.SetFollow(c, admin, "acme", "follows", ds)
		return err
	})
	background = true
	act("the follow's placement", "release.place", ds, []string{"follows"}, func(context.Context) error {
		placer.Once(ctx, false)
		return nil
	})
	for _, ch := range []string{"auto", "follows"} {
		if rs, err := en.rel.List(ctx, admin, "acme", ch, "tresor"); err != nil || len(rs) != 2 {
			t.Fatalf("placed into %s: %+v %v", ch, rs, err)
		}
	}

	// a mirror's run bringing a public release of tresor into prod, of an entry without a licence:
	// nothing
	en.rel.MayPublish = func(_, c string) bool { return c == "open" }
	en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
	sp := en.spec(store.UpstreamEntry{Name: "tresor"})
	sp.Credential, sp.Visibility = "open", store.Public
	u, err := en.up.Add(ctx, admin, "acme", sp)
	if err != nil {
		t.Fatal(err)
	}
	act("a public release of an unlicensed entry", "", "upstream:acme-repo", nil, func(context.Context) error {
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			return fmt.Errorf("a run: %+v", r)
		}
		return nil
	})
	background = false
	if rs := en.releases(t, "tresor"); len(rs) != 1 || rs[0].Visibility != store.Public {
		t.Fatalf("the mirror's release: %+v", rs)
	}
	// its relay stores a licence there: a warning (issued with the one the tenant holds: spec 0037, an
	// older one would be refused)
	en.answer(t, lic.sign("tresor", "pro", now, now+30*day))
	act("the relay", "license.sync", "upstream:acme-repo", []string{"prod"}, func(c context.Context) error {
		res, err := svc.RunUpstream(c, u.ID)
		if err == nil && res.Counts["stored"] != 1 {
			err = fmt.Errorf("the relay: %+v", res)
		}
		return err
	})

	// the upstream removed (its relayed licence with it) and added again: its successor's entry,
	// holding a licence before its first run, takes the public release in prod and serves its new
	// version publicly, warned once by the run
	if err := en.up.Remove(ctx, admin, "acme", "acme-repo", 0); err != nil {
		t.Fatal(err)
	}
	u2, err := en.up.Add(ctx, admin, "acme", sp)
	if err != nil {
		t.Fatal(err)
	}
	// a licence put into the successor's entry (as one a migration converted would be): it serves
	// nothing yet, so nothing
	act("a licence into the successor's entry, before it serves", "", "upstream:acme-repo", nil, func(c context.Context) error {
		return en.st.InTx(c, "", func(tx *store.Tx) error {
			l := store.License{TenantID: tid, PackageType: pkgtype.DuckDBExtension, Name: "tresor", Entry: store.UpstreamSource(u2.ID),
				LicenseID: "lic-successor", Issuer: "i", HolderID: "acme", Plan: "pro", Token: bought, Key: kid, IssuedAt: now,
				ExpiresAt: now + 30*day, GraceUntil: now + 31*day, Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:t"}
			return tx.PutLicense(ctx, &l, 0)
		})
	})
	en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.1", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
	background = true // the runner's, in its own context
	act("the successor's run", "upstream.release", "upstream:acme-repo", []string{"prod"}, func(context.Context) error {
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			return fmt.Errorf("the successor's run: %+v", r)
		}
		return nil
	})
	if rs := en.releases(t, "tresor"); len(rs) != 2 || rs[0].Source != store.UpstreamSource(u2.ID) || rs[1].Source != store.UpstreamSource(u2.ID) {
		t.Fatalf("the successor's releases: %+v", rs)
	}
}
