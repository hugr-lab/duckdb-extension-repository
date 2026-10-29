package upstream_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
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

// Spec 0033 phase A2: a mirror's version held at its arrival reaches its home channel when the
// hold ends whatever took it first, and whatever its cell holds by then: another channel's auto rule
// (its own hold shorter than the home channel's), a channel administrator's early placement
// elsewhere, and an upstream that moves on faster than its hold (each version placed after its own
// hold, older first).
func TestUpstreamHoldHome(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)
		en.st.Now = func() time.Time { return clock }
		pass := func(sweep bool) {
			(&release.Placer{Service: en.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}).Once(ctx, sweep)
		}
		versions := func(ch string) string {
			var vs []string
			for _, r := range en.releasesIn(t, ch, "acl") {
				vs = append(vs, r.PackageVersion)
			}
			return strings.Join(vs, " ")
		}
		put := func(v string, n int) {
			en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp(v, "v2.0.0"), n, "acl_duckdb_cpp_init"))
			if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
				t.Fatalf("%s held: %+v", v, r)
			}
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0); err != nil {
			t.Fatal(err)
		}
		// another channel's auto rule takes 2.0 first: prod's own hold is longer
		if _, err := en.rel.SetChannelHold(ctx, admin, "acme", "prod", 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "other", "acl", "upstream:acme-repo", "auto"); err != nil {
			t.Fatal(err)
		}
		put("2.0", 2)
		clock = clock.Add(61 * time.Minute)
		pass(false)
		if got := versions("other") + "/" + versions("prod"); got != "2.0/1.0" {
			t.Fatalf("after the source's hold: %q", got)
		}
		clock = clock.Add(time.Hour)
		pass(false)
		if got := versions("prod"); got != "2.0 1.0" {
			t.Fatalf("prod after its own hold, another channel first: %q", got)
		}
		// a channel administrator places 3.0 early elsewhere: prod still takes it after the hold
		if _, err := en.rel.SetChannelHold(ctx, admin, "acme", "prod", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "other", "acl", "upstream:acme-repo", "manual"); err != nil {
			t.Fatal(err)
		}
		put("3.0", 3)
		if _, _, err := en.rel.Place(ctx, admin, "acme", "other", "acl", release.PlaceOptions{Source: "upstream:acme-repo", Version: "3.0", Early: true}); err != nil {
			t.Fatal(err)
		}
		pass(false)
		if got := versions("prod"); got != "2.0 1.0" {
			t.Fatalf("prod while held: %q", got)
		}
		clock = clock.Add(61 * time.Minute)
		pass(false)
		if got := versions("prod"); got != "3.0 2.0 1.0" {
			t.Fatalf("prod after the hold, placed early elsewhere first: %q", got)
		}
		// the upstream moves on to 5.0 while 4.0 is held: the sweep keeps 4.0, placed when its hold
		// ends, then 5.0
		put("4.0", 4)
		clock = clock.Add(30 * time.Minute)
		put("5.0", 5)
		clock = clock.Add(31 * time.Minute)
		pass(true)
		if got := versions("prod"); got != "4.0 3.0 2.0 1.0" {
			t.Fatalf("prod after 4.0's hold, its cell moved on: %q", got)
		}
		clock = clock.Add(30 * time.Minute)
		pass(true)
		if got := versions("prod"); got != "5.0 4.0 3.0 2.0 1.0" {
			t.Fatalf("prod after 5.0's hold: %q", got)
		}
	})
}

// Spec 0033 phase A2: the pass places a mirror's held version in its home channel only as the run
// would take it then: not while the upstream is paused (placed once it is resumed), not once its
// name or version leaves the allowlist, and a name "*" took is checked again (a name reserved in
// the tenant meanwhile is refused).
func TestUpstreamHoldHomeChecks(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)
		en.st.Now = func() time.Time { return clock }
		pass := func(sweep bool) {
			(&release.Placer{Service: en.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}).Once(ctx, sweep)
		}
		versions := func() string {
			var vs []string
			for _, r := range en.releases(t, "acl") {
				vs = append(vs, r.PackageVersion)
			}
			return strings.Join(vs, " ")
		}
		put := func(v string, n int) {
			en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp(v, "v2.0.0"), n, "acl_duckdb_cpp_init"))
			if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
				t.Fatalf("%s held: %+v", v, r)
			}
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}))
		if err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0); err != nil {
			t.Fatal(err)
		}
		// paused: nothing placed; resumed: placed
		put("2.0", 2)
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamPaused, 0); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(61 * time.Minute)
		pass(false)
		if got := versions(); got != "1.0" {
			t.Fatalf("placed while the upstream is paused: %q", got)
		}
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamActive, 0); err != nil {
			t.Fatal(err)
		}
		pass(true)
		if got := versions(); got != "2.0 1.0" {
			t.Fatalf("after the upstream is resumed: %q", got)
		}
		// a version the allowlist no longer takes
		put("3.0", 3)
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "acl", Versions: []string{"9.0"}}); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(61 * time.Minute)
		pass(true)
		if got := versions(); got != "2.0 1.0" {
			t.Fatalf("placed a version out of the allowlist: %q", got)
		}
		// the name taken by "*" (as a pull-through's or lazy mirror's), then reserved server-wide: the
		// pass makes the run's check under the insert's locks, and refuses it
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "acl"}); err != nil {
			t.Fatal(err)
		}
		put("4.0", 4)
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.PutUpstreamEntry(ctx, u.ID, store.UpstreamEntry{Name: "*"}); err != nil {
					return err
				}
				return tx.RemoveUpstreamEntry(ctx, u.ID, "acl")
			})
		})
		if _, err := en.ten.Reserve(ctx, admin, "", []string{"acl"}, store.ReservedCommunity, true); err != nil {
			t.Fatal(err)
		}
		// the server's list outlives a test on a shared database (storetest.Reset keeps it), and the
		// process's view follows this test's store
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		t.Cleanup(func() { _ = en.ten.Unreserve(ctx, admin, "", "acl") })
		clock = clock.Add(61 * time.Minute)
		pass(true)
		if got := versions(); got != "2.0 1.0" {
			t.Fatalf("placed a name \"*\" took, reserved server-wide since: %q", got)
		}
		if v, err := en.rel.Source(ctx, admin, "acme", "prod", "acl"); err != nil || !strings.Contains(v.Outcome, "4.0") {
			t.Fatalf("the refusal, the rule's outcome: %+v %v", v, err)
		}
	})
}

// Spec 0033 phase A2: a held version left to the pass is an eager mirror's: once the mirror is made
// lazy it is not placed in the home channel (its askers' to fetch, or placed by hand); and while its
// upstream is paused, the trim keeps it (landed) after its hold, to be placed once it is resumed.
func TestUpstreamHoldHomeLazyPaused(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)
		en.st.Now = func() time.Time { return clock }
		pass := func(sweep bool) {
			(&release.Placer{Service: en.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}).Once(ctx, sweep)
		}
		versions := func() string {
			var vs []string
			for _, r := range en.releases(t, "acl") {
				vs = append(vs, r.PackageVersion)
			}
			return strings.Join(vs, " ")
		}
		put := func(v string, n int) {
			en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp(v, "v2.0.0"), n, "acl_duckdb_cpp_init"))
			if r := en.sync(t, false); r.Counts[store.CellHeld] != 1 {
				t.Fatalf("%s held: %+v", v, r)
			}
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}))
		if err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", time.Hour, 0); err != nil {
			t.Fatal(err)
		}
		// paused with 2.0's hold ended and its cell moved on to 3.0, still held: the sweep keeps 2.0
		// landed
		put("2.0", 2)
		clock = clock.Add(50 * time.Minute)
		put("3.0", 3)
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamPaused, 0); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(20 * time.Minute)
		pass(true)
		bs, err := en.st.SourceBuilds(ctx, u.TenantID, "duckdb_extension", store.UpstreamSource(u.ID), "acl")
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bs {
			if b.PackageVersion == "2.0" && !b.Landed {
				t.Fatalf("a held version of a paused upstream trimmed: %+v", b)
			}
		}
		if got := versions(); got != "1.0" {
			t.Fatalf("placed while paused: %q", got)
		}
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamActive, 0); err != nil {
			t.Fatal(err)
		}
		pass(true)
		if got := versions(); got != "2.0 1.0" {
			t.Fatalf("after the upstream is resumed: %q", got)
		}
		clock = clock.Add(time.Hour)
		pass(true)
		if got := versions(); got != "3.0 2.0 1.0" {
			t.Fatalf("after 3.0's hold: %q", got)
		}
		// made lazy with 4.0 held: not placed by the pass
		put("4.0", 4)
		if _, err := en.up.SetHold(ctx, admin, "acme", "acme-repo", 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.SetFetchMode(ctx, admin, "acme", "acme-repo", store.FetchLazy, 0); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(2 * time.Hour)
		pass(true)
		if got := versions(); got != "3.0 2.0 1.0" {
			t.Fatalf("a lazy mirror's held version placed by the pass: %q", got)
		}
	})
}
