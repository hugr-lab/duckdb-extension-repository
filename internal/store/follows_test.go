package store_test

import (
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0033: a follow is due when its source has a build made since its last look; a look begun
// before the follow was set again is not recorded; removing a source removes its follows.
func TestDueFollows(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fullTenant(t, s, "acme", "1")
		p := store.Publisher{TenantID: tn.ID, Name: "dist"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		src := store.PublisherSource(p.ID)
		build := func(ver, hash string) {
			b := cppBuild(tn, "geo", ver, hash)
			b.Source = src
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		}
		due := func() int {
			fs, err := s.DueFollows(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			return len(fs)
		}
		build("1.0", "a")
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetFollow(ctx, ch.ID, src, "os:1:t"); return err })
		if due() != 1 { // the fixture's follow names no source with builds
			t.Fatal("a new follow is due")
		}
		f := store.DueFollow{TenantID: tn.ID, ChannelID: ch.ID, Source: src}
		clock = clock.Add(time.Minute)
		look := clock
		if err := s.FollowLook(ctx, f, look, look, map[string]string{"x": "reserved"}); err != nil {
			t.Fatal(err)
		}
		if due() != 0 {
			t.Fatal("due after its look")
		}
		if skips, err := s.FollowSkips(ctx, ch.ID, src); err != nil || skips["x"] != "reserved" {
			t.Fatalf("skips: %v %v", skips, err)
		}
		clock = clock.Add(time.Minute)
		build("1.1", "b")
		if due() != 1 {
			t.Fatal("not due after a new build")
		}
		// a look begun before the follow was set (again) is not recorded
		if err := s.FollowLook(ctx, f, look.Add(-time.Hour), look.Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if due() != 1 {
			t.Fatal("an older look recorded")
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "dist", "os:1:t") })
		if ok, err := s.Following(ctx, ch.ID, src); err != nil || ok {
			t.Fatalf("a removed source's follow: %v %v", ok, err)
		}
	})
}
