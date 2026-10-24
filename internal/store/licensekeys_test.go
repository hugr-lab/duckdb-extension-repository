package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0021: the licence keys recorded per build and source, which builds are live, and removal;
// spec 0033 phase B: a source's live keys are those it recorded on its own live builds, or on its
// newest available version in the catalogue.
func TestLicenseKeys(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, prod := fixture(t, s)
		mirror := store.Channel{TenantID: tn.ID, Name: "mirror", Kind: store.ChannelPassthrough}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &mirror) })
		// the sources: a marked publisher and an upstream
		p1 := store.Publisher{TenantID: tn.ID, Name: "p1", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &p1); err != nil {
				return err
			}
			_, err := tx.SetPublisherLicenseKeys(ctx, tn.ID, "p1", true)
			return err
		})
		u1 := store.NewID()
		if err := store.ExecRaw(ctx, s, "INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, "+
			"request_dry_run, created_at, created_by) VALUES (?, ?, 'u1', 'repository', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')",
			u1, tn.ID, prod.ID, false, store.TimeArg(s, s.Now())); err != nil {
			t.Fatal(err)
		}
		ps, us := store.PublisherSource(p1.ID), store.UpstreamSource(u1)
		k1 := store.LicenseKey{Kid: "sha256:" + strings.Repeat("1", 64), PEM: "pem-1"}
		k2 := store.LicenseKey{Kid: "sha256:" + strings.Repeat("2", 64), PEM: "pem-2"}
		release := func(ch store.Channel, src, ver, hash string) (store.Build, store.Release) {
			t.Helper()
			b := cppBuild(tn, "tresor", ver, hash)
			b.Source = src
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, Source: src, PackageVersion: b.PackageVersion,
				Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public}
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.InsertRelease(ctx, &r, "os:1:t"); err != nil {
					return err
				}
				_, err := tx.SetChannelSource(ctx, ch.ID, b.Name, src, "os:1:t") // the channel serves it (spec 0031)
				return err
			})
			return b, r
		}
		b1, r1 := release(prod, ps, "1.0", "a")
		b2, _ := release(mirror, us, "1.1", "b") // a passthrough channel: never live for licences
		version := func() int64 {
			t.Helper()
			c, err := s.GetChannel(ctx, "acme", "prod")
			if err != nil {
				t.Fatal(err)
			}
			return c.ReleaseVersion
		}
		record := func(b store.Build, kind, src string, keys ...store.LicenseKey) bool {
			t.Helper()
			var added bool
			inTx(t, s, func(tx *store.Tx) (err error) {
				added, err = tx.RecordLicenseKeys(ctx, tn.ID, b.ID, kind, src, keys)
				return err
			})
			return added
		}
		live := func(src string) string {
			t.Helper()
			var ks []store.LicenseKey
			inTx(t, s, func(tx *store.Tx) (err error) {
				ks, err = tx.LiveLicenseKeys(ctx, tn.ID, pkgtype.DuckDBExtension, "tresor", src)
				return err
			})
			var out []string
			for _, k := range ks {
				out = append(out, k.Kid[7:8]+":"+k.PEM)
			}
			return strings.Join(out, ",")
		}

		// recorded once per (build, kid, source); a new record bumps the channels holding the build
		v := version()
		if !record(b1, store.KeySourcePublisher, p1.ID, k1, k2) || version() != v+1 {
			t.Fatal("the first record")
		}
		if record(b1, store.KeySourcePublisher, p1.ID, k1) || version() != v+1 {
			t.Fatal("a record again")
		}
		if !record(b1, store.KeySourceUpstream, u1, k1) || !record(b2, store.KeySourceUpstream, u1, k2) {
			t.Fatal("another source's record")
		}
		if record(b1, store.KeySourceUpstream, u1) {
			t.Fatal("no keys recorded something")
		}

		// live: an active release in a signed channel, of the source's own build; u1's k1 is on
		// p1's build, and its own build is in a passthrough channel only, but is its newest
		// available version in the catalogue
		if got := live(ps); got != "1:pem-1,2:pem-2" {
			t.Fatalf("p1's live keys: %q", got)
		}
		if got := live(us); got != "2:pem-2" {
			t.Fatalf("u1's live keys: %q", got)
		}
		if got := live(store.SourceTenant) + live(store.LicenseUnknown); got != "" {
			t.Fatalf("the tenant's or unknown's keys: %q", got)
		}
		// a channel that serves the name from another source does not make the build live; the
		// source's newest available version still counts
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetChannelSource(ctx, prod.ID, "tresor", us, "os:1:t")
			return err
		})
		b3 := cppBuild(tn, "tresor", "2.0", "c") // p1's newer version, without keys
		b3.Source = ps
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b3) })
		if got := live(ps); got != "" {
			t.Fatalf("live keys of a source the channel does not serve, its newest version declaring none: %q", got)
		}
		inTx(t, s, func(tx *store.Tx) error {
			_, err := tx.SetChannelSource(ctx, prod.ID, "tresor", ps, "os:1:t")
			return err
		})
		ks, err := s.TenantLicenseKeys(ctx, tn.ID, pkgtype.DuckDBExtension, "")
		if err != nil || len(ks) != 2 || ks[0].Kid != k1.Kid || ks[0].Builds != 1 || ks[0].Owner != ps || !ks[0].OwnerRecorded ||
			fmt.Sprint(ks[0].Sources) != "[{publisher p1} {upstream u1}]" || fmt.Sprint(ks[1].Sources) != "[{publisher p1} {upstream u1}]" ||
			ks[1].Builds != 2 || ks[1].Owner != ps {
			t.Fatalf("tenant keys: %+v %v", ks, err)
		}
		if ks, err := s.TenantLicenseKeys(ctx, tn.ID, pkgtype.DuckDBExtension, "other"); err != nil || len(ks) != 0 {
			t.Fatalf("another name's keys: %+v %v", ks, err)
		}
		kids, err := s.BuildKids(ctx, prod.ID)
		if err != nil || len(kids) != 1 || fmt.Sprint(kids[b1.ID]) != "["+k1.Kid+" "+k2.Kid+"]" {
			t.Fatalf("the channel's kids: %v %v", kids, err)
		}

		// a yanked release, then a blocked body, is not live
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r1, store.ReleaseYanked, "os:1:t") })
		if got := live(ps); got != "" {
			t.Fatalf("a yanked release's keys: %q", got)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r1, store.ReleaseDeprecated, "os:1:t") })
		if got := live(ps); got == "" {
			t.Fatal("a deprecated release is live")
		}
		inTx(t, s, func(tx *store.Tx) error {
			return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: b1.BodyHash, Reason: "CVE", CreatedBy: "os:1:t"})
		})
		if got := live(ps); got != "" {
			t.Fatalf("a blocked body's keys: %q", got)
		}

		// a source that is gone records nothing, and its keys count for nothing
		if record(b1, store.KeySourceUpstream, "gone", k2) || record(b1, store.KeySourcePublisher, "gone", k2) {
			t.Fatal("a missing source recorded keys")
		}
		if err := store.ExecRaw(ctx, s, "INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at) "+
			"VALUES (?, ?, ?, 'pem-3', 'upstream', 'gone', ?)", tn.ID, b1.ID, "sha256:"+strings.Repeat("3", 64), store.TimeArg(s, s.Now())); err != nil {
			t.Fatal(err)
		}
		if kids, _ := s.BuildKids(ctx, prod.ID); len(kids[b1.ID]) != 2 {
			t.Fatalf("an orphan key counts: %v", kids)
		}

		// removing a source drops what it brought and the keys it owns, and bumps the channels
		v = version()
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteLicenseKeysOf(ctx, store.KeySourcePublisher, p1.ID) })
		if kids, _ := s.BuildKids(ctx, prod.ID); fmt.Sprint(kids[b1.ID]) != "["+k1.Kid+"]" || version() != v+1 {
			t.Fatalf("after removing p1: %v", kids)
		}
		inTx(t, s, func(tx *store.Tx) error {
			o, err := tx.KeyOwnerOf(ctx, tn.ID, pkgtype.DuckDBExtension, "tresor", k2.Kid)
			if err == nil && (o.Source != us || o.Recorded) { // the next recorder: u1, on its own build
				t.Errorf("k2's owner after p1's removal: %+v", o)
			}
			return err
		})
	})
}
