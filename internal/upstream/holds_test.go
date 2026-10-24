package upstream_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033 phase A2: an eager mirror's hold keeps a new version in the catalogue (cell held) until
// it ends, when the placement pass places it in the mirror's home channel, the first arrival of a
// name included; a channel's hold does the same; a lazy mirror takes no hold.
func TestUpstreamHold(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		// a store clock half a day back: moved forward by hours, a blob commit's claim (until a
		// deadline on the real clock) stays live
		clock := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)
		en.st.Now = func() time.Time { return clock }
		pass := func() {
			(&release.Placer{Service: en.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}).Once(ctx, false)
		}
		versions := func(name string) string {
			var vs []string
			for _, r := range en.releases(t, name) {
				vs = append(vs, r.PackageVersion)
			}
			return strings.Join(vs, " ")
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		u, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0)
		if err != nil || u.Hold != time.Hour {
			t.Fatalf("the hold: %+v %v", u, err)
		}
		// a new version is held: in the catalogue, not in prod
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("2.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
			t.Fatalf("a run with a hold: %+v", r)
		}
		pass()
		if got := versions("acl"); got != "1.0" {
			t.Fatalf("placed while held: %q", got)
		}
		// a new name's first arrival is held too, and writes its rule
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "geo"}); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "geo", signed(t, en.key, cpp("0.1", "v2.0.0"), 3, "geo_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
			t.Fatalf("a first arrival with a hold: %+v", r)
		}
		if v, err := en.rel.Source(ctx, admin, "acme", "prod", "geo"); err != nil || v.Source != "upstream:acme-repo" {
			t.Fatalf("the held first arrival's rule: %+v %v", v, err)
		}
		clock = clock.Add(61 * time.Minute)
		pass()
		if got := versions("acl") + "/" + versions("geo"); got != "2.0 1.0/0.1" {
			t.Fatalf("after the hold: %q", got)
		}
		rs := en.releases(t, "acl")
		if rs[0].Origin != store.OriginUpstream || !strings.Contains(rs[0].Provenance, `"placed":"home"`) {
			t.Fatalf("a held mirror build placed: %+v", rs[0])
		}
		// a channel's hold: the mirror's run waits for it too
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.SetChannelHold(ctx, admin, "acme", "prod", 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("3.0", "v2.0.0"), 4, "acl_duckdb_cpp_init"))
		clock = clock.Add(time.Minute)
		if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
			t.Fatalf("a run into a channel with a hold: %+v", r)
		}
		pass()
		if got := versions("acl"); got != "2.0 1.0" {
			t.Fatalf("placed within the channel's hold: %q", got)
		}
		clock = clock.Add(2*time.Hour + time.Minute)
		pass()
		if got := versions("acl"); got != "3.0 2.0 1.0" {
			t.Fatalf("after the channel's hold: %q", got)
		}
		// a hold is an eager mirror's: a lazy one takes none, and a mirror with one is not made lazy
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchLazy, 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a mirror with a hold made lazy: %v", err)
		}
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchLazy, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a lazy mirror's hold: %v", err)
		}
	})
}
