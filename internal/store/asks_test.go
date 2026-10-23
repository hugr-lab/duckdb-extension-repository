package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0032: an ask is inserted when absent, by one of two racing requests, and its time renewed
// only when older than an hour; a fetch mode and an eager entry are kept.
func TestUpstreamAsks(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		at := store.TimeArg(s, now)
		const tid, cid, uid = "t0000000-0000-0000-0000-000000000000", "c0000000-0000-0000-0000-000000000000", "u0000000-0000-0000-0000-000000000000"
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, at},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, at},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatal(err)
			}
		}
		u := store.Upstream{TenantID: tid, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: cid,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchLazy,
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "acl", Eager: true}, {Name: "tresor"}}}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) }); err != nil {
			t.Fatal(err)
		}
		ask := func() (bool, error) {
			var wrote bool
			err := s.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				wrote, err = tx.Ask(ctx, u.ID, "v2.0.0", "linux_amd64", "tresor")
				return err
			})
			return wrote, err
		}
		asks := func() map[[3]string]time.Time {
			var out map[[3]string]time.Time
			if err := s.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				out, err = tx.Asks(ctx, u.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		wrote := 0
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := ask()
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				if ok {
					wrote++
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		k := [3]string{"v2.0.0", "linux_amd64", "tresor"}
		if got := asks(); wrote != 1 || len(got) != 1 || !got[k].Equal(now) {
			t.Fatalf("racing asks: %d wrote, %v", wrote, got)
		}
		now = now.Add(59 * time.Minute)
		if ok, err := ask(); ok || err != nil {
			t.Fatalf("a recent ask was renewed: %v %v", ok, err)
		}
		now = now.Add(2 * time.Minute)
		if ok, err := ask(); !ok || err != nil || !asks()[k].Equal(now) {
			t.Fatalf("an old ask was not renewed: %v %v %v", ok, err, asks())
		}
		var got store.Upstream
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			got, err = tx.GetUpstreamByID(ctx, u.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.FetchMode != store.FetchLazy || !got.Entries[0].Eager || got.Entries[1].Eager {
			t.Fatalf("read back: %+v", got)
		}
		// an upstream's removal takes its asks
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeleteUpstream(ctx, got, 0) }); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "upstream_asks", "1 = 1"); err != nil || n != 0 {
			t.Fatalf("asks left: %d %v", n, err)
		}
	})
}
