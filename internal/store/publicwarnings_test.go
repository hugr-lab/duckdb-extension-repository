package store_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0035 phase 2, the store's part: each hook (a release written public or made public, the
// releases a successor takes, a rule selecting an entry, a licence stored or made public) warns
// only for an entry that holds a licence and has an active or deprecated public release in a
// signed channel whose rule selects it, its build not withdrawn and its body not blocked; once per
// source, name and channel in an act, across its transactions; written after the act commits, and
// a failure to write it is logged while the act stands.
func TestLicensePublicWarnings(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		mirror := func(name string) store.Upstream {
			u := store.Upstream{TenantID: tn.ID, Name: name, Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
				Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
			return u
		}
		a := mirror("a")
		src := store.UpstreamSource(a.ID)
		build := func(platform, hash string) store.Build {
			b := cppBuild(tn, "geo", "1.0", hash)
			b.Platform, b.Source = platform, src
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		release := func(b store.Build, vis string) store.Release {
			return store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: "geo", Source: b.Source, PackageVersion: "1.0",
				Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: vis, Origin: store.OriginUpstream}
		}
		license := func(entry, vis string) *store.License {
			return &store.License{TenantID: tn.ID, PackageType: pkgtype.DuckDBExtension, Name: "geo", Entry: entry, LicenseID: "l-" + entry,
				Issuer: "i", HolderID: "acme", Plan: "pro", Token: "tok", Key: "k", IssuedAt: 1, ExpiresAt: 2, GraceUntil: 3,
				Source: store.LicenseImport, Visibility: vis, Actor: "os:1:admin"}
		}
		// check runs an act's transactions and checks its warnings: answered and written, one each,
		// with the act and the actor wanted
		seen := 0
		check := func(what, act, actor string, want int, fns ...func(*store.Tx) error) {
			t.Helper()
			c := store.WithLicenseWarnings(ctx)
			for _, fn := range fns {
				if err := s.InTx(c, "", fn); err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			}
			ws := store.LicenseWarnings(c)
			evs, err := s.ListEvents(ctx, tn.ID, store.EventFilter{Kind: string(store.LicenseWarningKind), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			fresh := evs[:len(evs)-seen]
			seen = len(evs)
			if len(ws) != want || len(fresh) != want {
				t.Fatalf("%s: %d warnings, %d events, want %d: %+v %+v", what, len(ws), len(fresh), want, ws, fresh)
			}
			for _, w := range ws {
				if w.Name != "geo" || !strings.HasPrefix(w.Source, "upstream:") || w.Channel != "prod" || w.TenantID != tn.ID ||
					w.TenantName != "acme" {
					t.Errorf("%s: %+v", what, w)
				}
			}
			for _, ev := range fresh {
				if ev.Actor != actor || ev.Subject != "channel:prod/ext:geo" || !strings.Contains(ev.Data, `"act":"`+act+`"`) {
					t.Errorf("%s: %+v", what, ev)
				}
			}
		}
		rule := func(source string) func(*store.Tx) error {
			return func(tx *store.Tx) error {
				_, err := tx.SetChannelSource(ctx, ch.ID, "geo", source, "os:1:rule")
				return err
			}
		}

		// a licence into an entry without releases; a private release of it selected by a rule: nothing
		check("a licence, no release", "", "", 0, func(tx *store.Tx) error { return tx.PutLicense(ctx, license(src, store.Private), 0) })
		priv := release(build("linux_amd64", "a"), store.Private)
		check("a private release", "", "", 0, func(tx *store.Tx) error {
			if err := tx.InsertRelease(ctx, &priv, "os:1:t"); err != nil {
				return err
			}
			return rule(src)(tx)
		})
		// two public releases in two transactions of one act: one warning, the hook's act and actor
		pubs := []store.Release{release(build("osx_arm64", "b"), store.Public), release(build("windows_amd64", "c"), store.Public)}
		check("public releases", "release.add", "os:1:t", 1,
			func(tx *store.Tx) error { return tx.InsertRelease(ctx, &pubs[0], "os:1:t") },
			func(tx *store.Tx) error { return tx.InsertRelease(ctx, &pubs[1], "os:1:t") })
		// the private one made public, by the act the service names; public again (a replay), nothing
		check("made public", "release.public", "os:1:t", 1, func(tx *store.Tx) error {
			tx.SetWarningAct("release.public")
			return tx.SetReleaseVisibility(ctx, &priv, store.Public, "os:1:t")
		})
		check("public again", "", "", 0, func(tx *store.Tx) error { return tx.SetReleaseVisibility(ctx, &priv, store.Public, "os:1:t") })
		// made private again, nothing; the rule switched away nothing, back a warning
		check("made private", "", "", 0, func(tx *store.Tx) error { return tx.SetReleaseVisibility(ctx, &priv, store.Private, "os:1:t") })
		check("a switch away", "", "", 0, rule(store.SourceTenant))
		check("a switch back", "source.set", "os:1:rule", 1, rule(src))
		check("the same rule again", "", "", 0, rule(src))
		// the licence made public warns, private not; replaced, it warns, by the act a service names
		var l store.License
		inTx(t, s, func(tx *store.Tx) (err error) {
			l, err = tx.GetLicense(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", src)
			return err
		})
		check("the licence made public", "license.change", "os:1:admin", 1, func(tx *store.Tx) error {
			return tx.SetLicenseVisibility(ctx, &l, store.Public, "os:1:admin", l.Version)
		})
		check("the licence made private", "", "", 0, func(tx *store.Tx) error {
			return tx.SetLicenseVisibility(ctx, &l, store.Private, "os:1:admin", l.Version)
		})
		check("a licence replaced", "bundle.import", "os:1:admin", 1, func(tx *store.Tx) error {
			tx.SetWarningAct("bundle.import")
			return tx.PutLicense(ctx, license(src, store.Private), l.Version)
		})
		replace := func(tx *store.Tx) error {
			cur, err := tx.GetLicense(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", src)
			if err == nil {
				err = tx.PutLicense(ctx, license(src, store.Private), cur.Version)
			}
			return err
		}

		// a failure to check is logged, and the act stands
		var logged bytes.Buffer
		s.Log = slog.New(slog.NewTextHandler(&logged, nil))
		store.SetLicenseWarningFault(func() error { return errors.New("the check failed") })
		check("a failed check", "", "", 0, replace)
		store.SetLicenseWarningFault(nil)
		s.Log = nil
		if !strings.Contains(logged.String(), "a licence warning was not written") || !strings.Contains(logged.String(), "the check failed") {
			t.Fatalf("the failure logged: %q", logged.String())
		}
		if cur, err := s.GetLicense(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", src); err != nil || cur.Version != l.Version+2 {
			t.Fatalf("the act stands: %+v %v", cur, err)
		}
		// and a panic in it too: recovered, logged, the act committed and answered without an error
		logged.Reset()
		s.Log = slog.New(slog.NewTextHandler(&logged, nil))
		store.SetLicenseWarningFault(func() error { panic("the check broke") })
		check("a broken check", "", "", 0, replace)
		store.SetLicenseWarningFault(nil)
		s.Log = nil
		if !strings.Contains(logged.String(), "a licence warning's check failed") || !strings.Contains(logged.String(), "the check broke") {
			t.Fatalf("the panic logged: %q", logged.String())
		}
		if cur, err := s.GetLicense(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", src); err != nil || cur.Version != l.Version+3 {
			t.Fatalf("the act stands: %+v %v", cur, err)
		}

		// served publicly excludes a withdrawn build's release and a blocked body's: with every public
		// release one or the other, the licence stored warns nothing
		if err := store.ExecRaw(ctx, s, "UPDATE builds SET withdrawn_at = created_at WHERE id = ?", pubs[0].BuildID); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: strings.Repeat("c", 64), Reason: "test", CreatedBy: "os:1:t"})
		})
		check("withdrawn and blocked", "", "", 0, replace)

		// a successor: its entry holds a licence and its rule selects it; the releases it takes warn
		// (the public one not withdrawn nor blocked: the private one, made public again)
		check("made public again", "release.public", "os:1:t", 1, func(tx *store.Tx) error {
			tx.SetWarningAct("release.public")
			return tx.SetReleaseVisibility(ctx, &priv, store.Public, "os:1:t")
		})
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteUpstream(ctx, a, 0) })
		succ := store.UpstreamSource(mirror("b").ID)
		check("the successor's licence and rule", "", "", 0,
			func(tx *store.Tx) error { return tx.PutLicense(ctx, license(succ, store.Private), 0) }, rule(succ))
		check("the releases taken", "upstream.release", "system:kista", 1, func(tx *store.Tx) error {
			n, err := tx.TakeReleases(ctx, ch.ID, "geo", src, succ)
			if err == nil && n != 3 {
				err = errors.New("not all taken")
			}
			return err
		})
		if ws := store.LicenseWarnings(context.Background()); ws != nil {
			t.Fatalf("a context that collects nothing: %+v", ws)
		}
	})
}
