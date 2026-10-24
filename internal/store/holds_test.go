package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0040 (spec 0033 phase A2) adds holds: on a populated database at level 39, every
// upstream, publisher and channel has none, every build is available (none left to the pass for its
// mirror's home channel), and a rule has no hold to wait for; the publisher's hold set afterwards
// holds what it publishes next.
func TestMigrationHolds(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 39); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		const tid = "t0000000-0000-0000-0000-000000000000"
		cid := id("c")
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, now},
			{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, 'ci', ?, 'os:1:t')", id("p"), tid, now},
			{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, created_by) " +
				"VALUES (?, ?, 'core', 'duckdb-core', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')", id("u"), tid, cid, false, now},
			{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked) VALUES (?, ?, ?, 'acl', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'publication', ?, 'os:1:t', ?)",
				id("b"), tid, "publisher:" + id("p"), strings.Repeat("1", 64), now, true},
			{"INSERT INTO channel_sources (channel_id, name, source, mode, set_by, set_at) VALUES (?, 'acl', ?, 'auto', 'os:1:t', ?)",
				cid, "publisher:" + id("p"), now},
			// a mirror's build landed in its home channel's catalogue, under the rule its first arrival wrote
			{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked, landed) VALUES (?, ?, ?, 'json', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'upstream', ?, " +
				"'os:1:t', ?, ?)", id("m"), tid, "upstream:" + id("u"), strings.Repeat("3", 64), now, true, true},
			{"INSERT INTO channel_sources (channel_id, name, source, mode, set_by, set_at) VALUES (?, 'json', ?, '', 'os:1:t', ?)",
				cid, "upstream:" + id("u"), now},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if ch, err := s.GetChannel(ctx, "acme", "prod"); err != nil || ch.Hold != 0 {
			t.Fatalf("the channel: %+v %v", ch, err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if p, err := tx.GetPublisher(ctx, tid, "ci"); err != nil || p.Hold != 0 {
				t.Errorf("the publisher: %+v %v", p, err)
			}
			if u, err := tx.GetUpstream(ctx, tid, "core"); err != nil || u.Hold != 0 {
				t.Errorf("the upstream: %+v %v", u, err)
			}
			b, err := tx.GetBuild(ctx, id("b"))
			if err != nil || !b.AvailableAt.IsZero() || b.Held(time.Now()) || b.PublisherKeyID != "" {
				t.Errorf("the build: %+v %v", b, err)
			}
			return nil
		})
		// the auto rule is due (never looked at); the mirror's rule without a mode is not the pass's:
		// its build was available on arrival (available_at NULL), never left to the pass
		if rs, err := s.DueRules(ctx, true); err != nil || len(rs) != 1 || rs[0].Name != "acl" {
			t.Fatalf("the rules the pass looks at: %+v %v", rs, err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetPublisherHold(ctx, tid, "ci", time.Hour); return err })
		b := store.Build{TenantID: tid, Source: "publisher:" + id("p"), Name: "acl", PackageVersion: "1.1", Platform: "linux_amd64",
			ABI: store.ABICPP, DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat("2", 64), Origin: store.OriginPublication, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		if !b.Held(s.Now()) || b.AvailableAt.Sub(b.CreatedAt) != time.Hour {
			t.Fatalf("a new build of a held publisher: %+v", b)
		}
	})
}

// Spec 0033 phase A2: an entry arriving from a source with a hold is held until its arrival plus
// the hold (an eager mirror's, a publisher's; never a lazy mirror's, nor the tenant's); raising the
// hold holds the entries held now longer, lowering it holds what arrives next shorter; a channel's
// hold adds to an automatic placement's wait.
func TestHolds(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		mirror := func(name, fetch string) store.Upstream {
			u := store.Upstream{TenantID: tn.ID, Name: name, Kind: store.UpstreamRepo, Prefix: "https://" + name + ".example", ChannelID: ch.ID,
				Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: fetch,
				Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: name + "x"}}}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
			return u
		}
		eager, lazy := mirror("eager", store.FetchEager), mirror("lazy", store.FetchLazy)
		build := func(source, name, ver, hash string) store.Build {
			b := cppBuild(tn, name, ver, hash)
			b.Source = source
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		for _, d := range []time.Duration{-time.Second, store.MaxHold + time.Second, 1500 * time.Millisecond} {
			if err := store.ValidHold(d); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("hold %s: %v", d, err)
			}
		}
		pub := store.PublisherSource(p.ID)
		// a publisher's hold
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetPublisherHold(ctx, tn.ID, "ci", time.Hour)
			if err == nil && before.Hold != 0 {
				t.Errorf("the hold before: %s", before.Hold)
			}
			return err
		})
		held := build(pub, "geo", "1.0", "a")
		if !held.Held(clock) || !held.AvailableAt.Equal(clock.Add(time.Hour)) {
			t.Fatalf("a held publisher's entry: %+v", held)
		}
		if b := build(store.SourceTenant, "geo", "1.0", "b"); b.Held(clock) {
			t.Fatal("the tenant's own entry held")
		}
		// an eager mirror's hold; a lazy mirror takes none, whatever its column says
		inTx(t, s, func(tx *store.Tx) error { return tx.SetUpstreamHold(ctx, &eager, 2*time.Hour, 0) })
		if err := store.ExecRaw(ctx, s, "UPDATE upstreams SET hold_seconds = 60 WHERE id = ?", lazy.ID); err != nil {
			t.Fatal(err)
		}
		if b := build(store.UpstreamSource(eager.ID), "eagerx", "1.0", "c"); !b.AvailableAt.Equal(clock.Add(2 * time.Hour)) {
			t.Fatalf("an eager mirror's entry: %+v", b)
		}
		if b := build(store.UpstreamSource(lazy.ID), "lazyx", "1.0", "d"); b.Held(clock) {
			t.Fatalf("a lazy mirror's entry held: %+v", b)
		}
		// raising the hold holds the entries held now longer; one available is left as it is
		clock = clock.Add(90 * time.Minute) // held is available now
		later := build(pub, "geo", "1.1", "e")
		var n int
		inTx(t, s, func(tx *store.Tx) error {
			var err error
			if _, err = tx.SetPublisherHold(ctx, tn.ID, "ci", 3*time.Hour); err == nil {
				n, err = tx.RaiseHeld(ctx, tn.ID, pub, 3*time.Hour)
			}
			return err
		})
		read := func(id string) store.Build {
			var b store.Build
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				b, err = tx.GetBuild(ctx, id)
				return err
			})
			return b
		}
		if n != 1 || !read(later.ID).AvailableAt.Equal(later.CreatedAt.Add(3*time.Hour)) || read(held.ID).Held(clock) {
			t.Fatalf("raised: %d, %+v, %+v", n, read(later.ID), read(held.ID))
		}
		// lowering it changes no entry held now; what arrives next is held the shorter time
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetPublisherHold(ctx, tn.ID, "ci", 10*time.Minute); return err })
		if b := read(later.ID); !b.AvailableAt.Equal(later.CreatedAt.Add(3 * time.Hour)) {
			t.Fatalf("lowered: %+v", b)
		}
		if b := build(pub, "geo", "1.2", "f"); !b.AvailableAt.Equal(clock.Add(10 * time.Minute)) {
			t.Fatalf("after lowering: %+v", b)
		}
		// a copy (a successor's of a released build) keeps the original's arrival, availability and
		// revoked marker, not its new source's hold
		nh := cppBuild(tn, "geo", "1.3", "g")
		nh.Source, nh.Copied, nh.CreatedAt = pub, true, clock.Add(-time.Hour)
		nh.AvailableAt, nh.RevokedAt = clock.Add(-30*time.Minute), clock.Add(-time.Minute)
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &nh) })
		if b := read(nh.ID); b.Held(clock) || !b.CreatedAt.Equal(clock.Add(-time.Hour)) || !b.AvailableAt.Equal(clock.Add(-30*time.Minute)) ||
			!b.RevokedAt.Equal(clock.Add(-time.Minute)) || !b.Revoked() {
			t.Fatalf("a copy: %+v", b)
		}
		// a channel's hold: an automatic placement waits for the later of the two
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetChannelHold(ctx, ch.ID, 5*time.Hour)
			if err == nil && before != 0 {
				t.Errorf("the channel's hold before: %s", before)
			}
			return err
		})
		c, err := s.GetChannel(ctx, "acme", "prod")
		if err != nil || c.Hold != 5*time.Hour {
			t.Fatalf("the channel's hold: %+v %v", c, err)
		}
		if at := read(later.ID).AvailableIn(c.Hold); !at.Equal(later.CreatedAt.Add(5 * time.Hour)) {
			t.Fatalf("in a channel with a longer hold: %s", at)
		}
		if at := read(later.ID).AvailableIn(time.Hour); !at.Equal(later.CreatedAt.Add(3 * time.Hour)) {
			t.Fatalf("in a channel with a shorter hold: %s", at)
		}
		// a channel with a hold asks no lazy mirror
		if pt, _, err := s.ChannelPullThrough(ctx, ch.ID); err != nil || len(pt) != 0 {
			t.Fatalf("pull-through with a channel hold: %+v %v", pt, err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetChannelHold(ctx, ch.ID, 0); return err })
		if pt, _, err := s.ChannelPullThrough(ctx, ch.ID); err != nil || len(pt) != 1 {
			t.Fatalf("pull-through without a channel hold: %+v %v", pt, err)
		}
		// the catalogue's held versions
		hv, err := s.HeldVersions(ctx, tn.ID, "duckdb_extension")
		if err != nil || len(hv[[2]string{pub, "geo"}]) != 2 || len(hv[[2]string{store.UpstreamSource(eager.ID), "eagerx"}]) != 1 {
			t.Fatalf("held versions: %v %v", hv, err)
		}
	})
}

// Spec 0033 phase A2: the placement pass looks at a rule again when the hold it waits for ends;
// a mirror's rule without a mode in its home channel is the pass's while the mirror has entries of
// the name held at their arrival (landed or not: another channel may have released one), and no
// other rule without a mode is; a look begun before a recheck does not record itself over it, and
// a look that failed leaves the rule due.
func TestDueRulesHolds(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		other := store.Channel{TenantID: tn.ID, Name: "dev", Kind: store.ChannelSigned}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &other) })
		u := store.Upstream{TenantID: tn.ID, Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager,
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "geo"}}}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
		src := store.UpstreamSource(u.ID)
		for _, c := range []store.Channel{ch, other} {
			inTx(t, s, func(tx *store.Tx) error {
				_, err := tx.SetChannelRule(ctx, c.ID, "geo", src, store.RuleNone, "os:1:t")
				return err
			})
		}
		due := func() []store.DueRule {
			rs, err := s.DueRules(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			return rs
		}
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = src
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		if rs := due(); len(rs) != 0 {
			t.Fatalf("rules without a mode, nothing held at its arrival: %+v", rs)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetLanded(ctx, b.ID) })
		if rs := due(); len(rs) != 0 {
			t.Fatalf("rules without a mode, an entry landed but not held at its arrival: %+v", rs)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.HeldAtArrival(ctx, b.ID) })
		// another channel releases it (no longer landed): the home rule stays the pass's
		inTx(t, s, func(tx *store.Tx) error {
			r := store.Release{TenantID: tn.ID, ChannelID: other.ID, BuildID: b.ID, Name: "geo", Source: src, PackageVersion: "1.0",
				Platform: "linux_amd64", Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPlacement}
			return tx.InsertRelease(ctx, &r, "os:1:t")
		})
		rs := due()
		if len(rs) != 1 || rs[0].ChannelID != ch.ID || rs[0].Mode != store.RuleNone {
			t.Fatalf("the home rule with an entry held at its arrival: %+v", rs)
		}
		if got, err := s.SourceBuilds(ctx, tn.ID, "duckdb_extension", src, "geo"); err != nil || len(got) != 1 || got[0].Landed ||
			!got[0].AvailableAt.Equal(got[0].CreatedAt) || got[0].Held(clock) {
			t.Fatalf("marked held at its arrival: %+v %v", got, err)
		}
		// looked at, waiting for a hold an hour away: not due until then
		clock = clock.Add(time.Minute)
		if err := s.SetRuleChecked(ctx, rs[0], clock, clock, "", clock.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if rs := due(); len(rs) != 0 {
			t.Fatalf("due before the hold ends: %+v", rs)
		}
		clock = clock.Add(time.Hour)
		if rs := due(); len(rs) != 1 {
			t.Fatalf("not due when the hold ends: %+v", rs)
		}
		if err := s.SetRuleChecked(ctx, rs[0], clock, clock, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if rs := due(); len(rs) != 0 {
			t.Fatalf("due after the look that waits for nothing: %+v", rs)
		}
		// a look begun before the channel's rules were made to be looked at again records nothing
		start := clock
		clock = clock.Add(time.Second)
		inTx(t, s, func(tx *store.Tx) error { return tx.RecheckRules(ctx, ch.ID) })
		if err := s.SetRuleChecked(ctx, rs[0], start, start, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if rs := due(); len(rs) != 1 {
			t.Fatalf("a recheck after a look began, overwritten by it: %+v", rs)
		}
		// a look that failed records its outcome and leaves the rule due
		if err := s.SetRuleOutcome(ctx, rs[0], clock, "boom"); err != nil {
			t.Fatal(err)
		}
		if rs := due(); len(rs) != 1 {
			t.Fatalf("a failed look: %+v", rs)
		}
		if cs, err := s.ChannelSources(ctx, ch.ID); err != nil || cs["geo"].Outcome != "boom" {
			t.Fatalf("a failed look's outcome: %+v %v", cs["geo"], err)
		}
		// a look begun after the recheck records itself
		if err := s.SetRuleChecked(ctx, rs[0], clock, clock, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if rs := due(); len(rs) != 0 {
			t.Fatalf("due after a look begun after the recheck: %+v", rs)
		}
	})
}

// Spec 0033 phase A2: revoking a key purges the entries it published that are still held; one
// available, one released and another key's stay; removing the publisher's held entries takes the
// rest.
func TestPurgeHeld(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		addKey(t, s, ch, "sha256:"+strings.Repeat("a", 64), store.KeyActive)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		src := store.PublisherSource(p.ID)
		build := func(ver, hash, key string) store.Build {
			b := cppBuild(tn, "geo", ver, hash)
			b.Source, b.PublisherKeyID = src, key
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		k1, k2 := "k1000000-0000-0000-0000-000000000000", "k2000000-0000-0000-0000-000000000000"
		available := build("0.9", "a", k1)
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetPublisherHold(ctx, tn.ID, "ci", time.Hour); return err })
		heldK1, heldK2, released := build("1.0", "b", k1), build("1.1", "c", k2), build("1.2", "d", k1)
		if heldK1.PublisherKeyID != k1 {
			t.Fatalf("the key: %+v", heldK1)
		}
		inTx(t, s, func(tx *store.Tx) error {
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: released.ID, Name: "geo", Source: src, PackageVersion: "1.2",
				Platform: "linux_amd64", Slot: released.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPlacement}
			return tx.InsertRelease(ctx, &r, "os:1:t")
		})
		purge := func(key string) ([]string, []store.KeptEntry) {
			var ids []string
			var kept []store.KeptEntry
			inTx(t, s, func(tx *store.Tx) error {
				bs, ks, err := tx.PurgeHeld(ctx, tn.ID, src, key)
				for _, b := range bs {
					ids = append(ids, b.ID)
				}
				kept = ks
				return err
			})
			return ids, kept
		}
		// the released one is kept, revoked, with the channels holding it
		ids, kept := purge(k1)
		if len(ids) != 1 || ids[0] != heldK1.ID || len(kept) != 1 || kept[0].ID != released.ID || kept[0].Channels != 1 ||
			!kept[0].RevokedAt.Equal(clock) {
			t.Fatalf("the revoked key's held entries: %v %+v", ids, kept)
		}
		clock = clock.Add(time.Minute)
		if ids, kept := purge(""); len(ids) != 1 || ids[0] != heldK2.ID || len(kept) != 1 || !kept[0].RevokedAt.Equal(clock.Add(-time.Minute)) {
			t.Fatalf("the publisher's held entries: %v %+v", ids, kept)
		}
		read := func(id string) (b store.Build) {
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				b, err = tx.GetBuild(ctx, id)
				if err != nil {
					t.Errorf("%s: %v", id, err)
				}
				return nil
			})
			return b
		}
		if b := read(available.ID); b.Revoked() {
			t.Fatalf("an available entry revoked: %+v", b)
		}
		if b := read(released.ID); !b.Revoked() {
			t.Fatalf("the kept entry: %+v", b)
		}
		if rv, err := s.RevokedVersions(ctx, tn.ID, "duckdb_extension"); err != nil || len(rv[[2]string{src, "geo"}]) != 1 {
			t.Fatalf("revoked versions: %v %v", rv, err)
		}
		// a release insert under the channel lock refuses the revoked entry
		inTx(t, s, func(tx *store.Tx) error {
			_, _, revoked, err := tx.BuildAvailability(ctx, released.ID)
			if err == nil && revoked.IsZero() {
				t.Error("the revoked marker not read")
			}
			return err
		})
	})
}

// Spec 0033 phase A2: an arrival from a removed source fails closed (a removed upstream or
// publisher brings nothing), and a publication signed with a key removed meanwhile is refused.
func TestSourceGone(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		src := store.PublisherSource(p.ID)
		gone := func(source string) {
			t.Helper()
			b := cppBuild(tn, "geo", "1.0", "a")
			b.Source = source
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) }); !errors.Is(err, store.ErrSourceGone) {
				t.Fatalf("a build of %s: %v", source, err)
			}
			if _, err := s.SourceHold(ctx, source); !errors.Is(err, store.ErrSourceGone) {
				t.Fatalf("the hold of %s: %v", source, err)
			}
		}
		gone(store.PublisherSource("00000000-0000-0000-0000-000000000000"))
		gone(store.UpstreamSource("00000000-0000-0000-0000-000000000000"))
		live := func(keyID string) error {
			return s.InTx(ctx, store.PublisherAuthLock(tn.ID), func(tx *store.Tx) error { return tx.PublicationLive(ctx, src, keyID) })
		}
		if err := live(""); err != nil {
			t.Fatalf("a live publisher: %v", err)
		}
		if err := live("k1000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a removed key: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.PublicationLive(ctx, store.SourceTenant, "x") }); err != nil {
			t.Fatalf("the tenant's own source: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "ci") })
		if err := live(""); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a removed publisher: %v", err)
		}
		gone(src)
	})
}

// Spec 0033 phase A2: a successor's copy of a removed upstream's released build keeps the original's
// arrival, hold and revoked marker.
func TestSuccessorCopyHold(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		mirror := func(name string) store.Upstream {
			u := store.Upstream{TenantID: tn.ID, Name: name, Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
				Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
			return u
		}
		a := mirror("a")
		inTx(t, s, func(tx *store.Tx) error { return tx.SetUpstreamHold(ctx, &a, time.Hour, 0) })
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = store.UpstreamSource(a.ID)
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: "geo", Source: b.Source, PackageVersion: "1.0",
				Platform: "linux_amd64", Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginUpstream}
			return tx.InsertRelease(ctx, &r, "os:1:t")
		})
		if err := store.ExecRaw(ctx, s, "UPDATE builds SET revoked_at = created_at WHERE id = ?", b.ID); err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteUpstream(ctx, a, 0) })
		clock = clock.Add(10 * time.Minute)
		succ := mirror("b")
		inTx(t, s, func(tx *store.Tx) error {
			n, err := tx.TakeReleases(ctx, ch.ID, "geo", b.Source, store.UpstreamSource(succ.ID))
			if err == nil && n != 1 {
				t.Errorf("moved %d", n)
			}
			return err
		})
		bs, err := s.SourceBuilds(ctx, tn.ID, "duckdb_extension", store.UpstreamSource(succ.ID), "geo")
		if err != nil || len(bs) != 1 || !bs[0].CreatedAt.Equal(b.CreatedAt) || !bs[0].AvailableAt.Equal(b.CreatedAt.Add(time.Hour)) ||
			!bs[0].RevokedAt.Equal(b.CreatedAt) {
			t.Fatalf("the successor's copy: %+v %v (the original %+v)", bs, err, b)
		}
	})
}

// Spec 0033 phase A2: a build inserted while a hold is being raised is held the raised time: the
// insert reads its source's hold with a lock the raise's update waits for, so the raise then finds
// the build (on PostgreSQL the raise's read would miss the build otherwise).
func TestHoldReadLocked(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, ch := fixture(t, s)
		u := store.Upstream{TenantID: tn.ID, Name: "a", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &u) })
		inTx(t, s, func(tx *store.Tx) error { return tx.SetUpstreamHold(ctx, &u, time.Hour, 0) })
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = store.UpstreamSource(u.ID)
		raised := make(chan error, 1)
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			go func() {
				raised <- s.InTx(ctx, store.TenantUpstreamLock(tn.ID), func(tx *store.Tx) error {
					cur, err := tx.GetUpstreamByID(ctx, u.ID)
					if err != nil {
						return err
					}
					if err := tx.SetUpstreamHold(ctx, &cur, 3*time.Hour, 0); err != nil {
						return err
					}
					_, err = tx.RaiseHeld(ctx, tn.ID, b.Source, 3*time.Hour)
					return err
				})
			}()
			time.Sleep(300 * time.Millisecond) // the raise reaches its update meanwhile
			return nil
		})
		if err := <-raised; err != nil {
			t.Fatal(err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			got, err := tx.GetBuild(ctx, b.ID)
			if err == nil && !got.AvailableAt.Equal(b.CreatedAt.Add(3*time.Hour)) {
				t.Errorf("a build inserted during the raise: %+v", got)
			}
			return err
		})
	})
}
