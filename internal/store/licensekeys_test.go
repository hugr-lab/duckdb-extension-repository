package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0021: the licence keys recorded per build and source, which builds are live, and removal.
func TestLicenseKeys(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, prod := fixture(t, s)
		mirror := store.Channel{TenantID: tn.ID, Name: "mirror", Kind: store.ChannelPassthrough}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &mirror) })
		k1 := store.LicenseKey{Kid: "sha256:" + strings.Repeat("1", 64), PEM: "pem-1"}
		k2 := store.LicenseKey{Kid: "sha256:" + strings.Repeat("2", 64), PEM: "pem-2"}
		release := func(ch store.Channel, ver, hash string) (store.Build, store.Release) {
			t.Helper()
			b := cppBuild(tn, "tresor", ver, hash)
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, ExtVersion: b.ExtVersion,
				Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertRelease(ctx, &r, "os:1:t") })
			return b, r
		}
		b1, r1 := release(prod, "1.0", "a")
		b2, _ := release(mirror, "1.1", "b") // a passthrough channel: never live for licences
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
		live := func(kind, src string) string {
			t.Helper()
			var ks []store.LicenseKey
			inTx(t, s, func(tx *store.Tx) (err error) {
				ks, err = tx.LiveLicenseKeys(ctx, tn.ID, "tresor", kind, src)
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
		if !record(b1, store.KeySourcePublisher, "p1", k1, k2) || version() != v+1 {
			t.Fatal("the first record")
		}
		if record(b1, store.KeySourcePublisher, "p1", k1) || version() != v+1 {
			t.Fatal("a record again")
		}
		if !record(b1, store.KeySourceUpstream, "u1", k1) || !record(b2, store.KeySourceUpstream, "u1", k2) {
			t.Fatal("another source's record")
		}
		if record(b1, store.KeySourceUpstream, "u1") {
			t.Fatal("no keys recorded something")
		}

		// live: an active release in a signed channel
		if got := live("", ""); got != "1:pem-1,2:pem-2" {
			t.Fatalf("live keys: %q", got)
		}
		if got := live(store.KeySourceUpstream, "u1"); got != "1:pem-1" {
			t.Fatalf("u1's live keys (b2 is in a passthrough channel only): %q", got)
		}
		ks, err := s.TenantLicenseKeys(ctx, tn.ID, "")
		if err != nil || len(ks) != 2 || ks[0].Kid != k1.Kid || ks[0].Builds != 1 ||
			fmt.Sprint(ks[0].Sources) != "[{publisher p1} {upstream u1}]" || fmt.Sprint(ks[1].Sources) != "[{publisher p1}]" {
			t.Fatalf("tenant keys: %+v %v", ks, err)
		}
		if ks, err := s.TenantLicenseKeys(ctx, tn.ID, "other"); err != nil || len(ks) != 0 {
			t.Fatalf("another name's keys: %+v %v", ks, err)
		}
		kids, err := s.BuildKids(ctx, prod.ID)
		if err != nil || len(kids) != 1 || fmt.Sprint(kids[b1.ID]) != "["+k1.Kid+" "+k2.Kid+"]" {
			t.Fatalf("the channel's kids: %v %v", kids, err)
		}

		// a yanked release, then a blocked body, is not live
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r1, store.ReleaseYanked, "os:1:t") })
		if got := live("", ""); got != "" {
			t.Fatalf("a yanked release's keys: %q", got)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r1, store.ReleaseDeprecated, "os:1:t") })
		if got := live("", ""); got == "" {
			t.Fatal("a deprecated release is live")
		}
		inTx(t, s, func(tx *store.Tx) error {
			return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: b1.BodyHash, Reason: "CVE", CreatedBy: "os:1:t"})
		})
		if got := live("", ""); got != "" {
			t.Fatalf("a blocked body's keys: %q", got)
		}

		// removing a source drops what it brought, and bumps the channels
		v = version()
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteLicenseKeysOf(ctx, store.KeySourcePublisher, "p1") })
		if kids, _ := s.BuildKids(ctx, prod.ID); fmt.Sprint(kids[b1.ID]) != "["+k1.Kid+"]" || version() != v+1 {
			t.Fatalf("after removing p1: %v", kids)
		}
	})
}
