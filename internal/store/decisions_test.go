package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0041: each held version's end of hold is the latest of its builds held now; one whose hold
// ended leaves it; a raised hold moves it.
func TestHeldUntil(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, _ := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.SetPublisherHold(ctx, tn.ID, "ci", time.Hour); return err })
		pub := store.PublisherSource(p.ID)
		build := func(ver, platform, hash string) store.Build {
			b := cppBuild(tn, "geo", ver, hash)
			b.Source, b.Platform = pub, platform
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		first := build("1.0", "linux_amd64", "a")
		clock = clock.Add(10 * time.Minute)
		second := build("1.0", "osx_arm64", "b")
		until := func() map[string]time.Time {
			t.Helper()
			hv, err := s.HeldVersions(ctx, tn.ID, "duckdb_extension")
			if err != nil {
				t.Fatal(err)
			}
			return hv[[2]string{pub, "geo"}]
		}
		if u := until(); len(u) != 1 || !u["1.0"].Equal(second.AvailableAt) {
			t.Fatalf("two builds of one version, one held longer: %v (%s %s)", u, first.AvailableAt, second.AvailableAt)
		}
		// the first build's hold ended: the version is still held, until the second's ends
		clock = first.AvailableAt.Add(time.Second)
		if u := until(); !u["1.0"].Equal(second.AvailableAt) {
			t.Fatalf("one hold ended: %v", u)
		}
		// a raised hold holds the second longer
		inTx(t, s, func(tx *store.Tx) error {
			if _, err := tx.SetPublisherHold(ctx, tn.ID, "ci", 3*time.Hour); err != nil {
				return err
			}
			_, err := tx.RaiseHeld(ctx, tn.ID, pub, 3*time.Hour)
			return err
		})
		if u := until(); !u["1.0"].Equal(second.CreatedAt.Add(3 * time.Hour)) {
			t.Fatalf("a raised hold: %v", u)
		}
		clock = second.CreatedAt.Add(3*time.Hour + time.Second)
		if u := until(); len(u) != 0 {
			t.Fatalf("every hold ended: %v", u)
		}
	})
}

// Spec 0041: one decision per (scope, kind, source, name), a later one replacing it with its row
// version moving on; a channel's and the tenant's decision of one name both kept; an Undo at a stale
// row version refused; decisions deleted with their source, and with the last build of their
// (source, name) whether the collector or a purge removes it.
func TestDecisions(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		// an older replica's reaper does not know the table, which references channels
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 51 AND min_reader = 51"); err != nil || n != 1 {
			t.Fatalf("migration 51 raises min_reader: %d %v", n, err)
		}
		tn, ch := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		pub := store.PublisherSource(p.ID)
		set := func(d store.Decision) (store.Decision, store.Decision, bool) {
			t.Helper()
			d.TenantID, d.PackageType, d.DecidedBy = tn.ID, "duckdb_extension", "os:1:t"
			var before store.Decision
			var had bool
			if err := s.InTx(ctx, store.DecisionLock(tn.ID), func(tx *store.Tx) error {
				var err error
				before, had, err = tx.SetDecision(ctx, &d)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return d, before, had
		}
		skip, _, had := set(store.Decision{ChannelID: ch.ID, Kind: store.DecisionSkip, Source: pub, Name: "acl", PackageVersion: "0.4.0"})
		if had || skip.Version != 1 || skip.ID == "" {
			t.Fatalf("a new skip: %+v %v", skip, had)
		}
		again, before, had := set(store.Decision{ChannelID: ch.ID, Kind: store.DecisionSkip, Source: pub, Name: "acl", PackageVersion: "0.4.1"})
		if !had || before.PackageVersion != "0.4.0" || again.ID != skip.ID || again.Version != 2 {
			t.Fatalf("a replacement: %+v %+v %v", again, before, had)
		}
		// a dismissal of the same name in the channel, and the tenant's: three decisions
		set(store.Decision{ChannelID: ch.ID, Kind: store.DecisionDismiss, Source: pub, Name: "acl", Situation: `"reserved"`})
		tenant, _, had := set(store.Decision{Kind: store.DecisionDismiss, Source: pub, Name: "acl", Situation: `["prod"]`})
		if had {
			t.Fatal("the tenant's decision replaced a channel's")
		}
		ds, err := s.ListDecisions(ctx, tn.ID, store.DecisionFilter{})
		if err != nil || len(ds) != 3 {
			t.Fatalf("the decisions: %+v %v", ds, err)
		}
		if ds, err := s.ListDecisions(ctx, tn.ID, store.DecisionFilter{ChannelID: ch.ID}); err != nil || len(ds) != 2 || ds[0].Channel != "prod" {
			t.Fatalf("the channel's: %+v %v", ds, err)
		}
		if ds, err := s.ListDecisions(ctx, tn.ID, store.DecisionFilter{TenantOnly: true}); err != nil || len(ds) != 1 || ds[0].Situation != `["prod"]` {
			t.Fatalf("the tenant's: %+v %v", ds, err)
		}
		// paged by id
		page, err := s.ListDecisions(ctx, tn.ID, store.DecisionFilter{Limit: 2})
		if err != nil || len(page) != 2 {
			t.Fatalf("a page: %+v %v", page, err)
		}
		if rest, err := s.ListDecisions(ctx, tn.ID, store.DecisionFilter{After: page[1].ID}); err != nil || len(rest) != 1 || rest[0].ID <= page[1].ID {
			t.Fatalf("the next page: %+v %v", rest, err)
		}
		// an Undo at a stale version, then at the current one; another tenant's id is not found
		err = s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.DeleteDecision(ctx, tn.ID, skip.ID, 1); return err })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale Undo: %v", err)
		}
		err = s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.DeleteDecision(ctx, "another", tenant.ID, 0); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another tenant's: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.DeleteDecision(ctx, tn.ID, skip.ID, 2); return err })

		// a decision deleted between a replacement's read and its write (its source removed, its
		// entry's last build collected) is not answered as written
		restore := store.SetAfterDecisionRead(func(_ *store.Tx, exec func(string, ...any) error) {
			if err := exec("DELETE FROM decisions WHERE id = ?", tenant.ID); err != nil {
				t.Fatal(err)
			}
		})
		err = s.InTx(ctx, store.DecisionLock(tn.ID), func(tx *store.Tx) error {
			d := store.Decision{TenantID: tn.ID, PackageType: "duckdb_extension", Kind: store.DecisionDismiss, Source: pub, Name: "acl",
				Situation: `["prod","staging"]`, DecidedBy: "os:1:t"}
			_, _, err := tx.SetDecision(ctx, &d)
			return err
		})
		restore()
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a replacement of a decision deleted meanwhile: %v", err)
		}
		if n, err := store.CountRaw(ctx, s, "decisions", "id = ?", tenant.ID); err != nil || n != 1 {
			t.Fatalf("the replaced decision rolled back with it: %d %v", n, err)
		}

		// the collector's last build of (publisher, geo) takes geo's decisions; acl's stay
		b := cppBuild(tn, "geo", "1.0", "a")
		b.Source = pub
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		set(store.Decision{ChannelID: ch.ID, Kind: store.DecisionSkip, Source: pub, Name: "geo", PackageVersion: "1.0"})
		if ok, err := s.DeleteBuild(ctx, b.ID, time.Now().Add(time.Hour)); err != nil || !ok {
			t.Fatalf("collect: %v %v", ok, err)
		}
		if n, err := store.CountRaw(ctx, s, "decisions", "name = ?", "geo"); err != nil || n != 0 {
			t.Fatalf("geo's decisions after its last build: %d %v", n, err)
		}
		// a purge of the last build too; with another build left, the decisions stay
		two := []store.Build{cppBuild(tn, "geo", "1.0", "b"), cppBuild(tn, "geo", "1.1", "c")}
		for i := range two {
			two[i].Source = pub
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &two[i]) })
		}
		set(store.Decision{ChannelID: ch.ID, Kind: store.DecisionSkip, Source: pub, Name: "geo", PackageVersion: "1.1"})
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.DeleteEntry(ctx, tn.ID, two[0].ID); return err })
		if n, err := store.CountRaw(ctx, s, "decisions", "name = ?", "geo"); err != nil || n != 1 {
			t.Fatalf("geo's decision with a build left: %d %v", n, err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.DeleteEntry(ctx, tn.ID, two[1].ID); return err })
		if n, err := store.CountRaw(ctx, s, "decisions", "name = ?", "geo"); err != nil || n != 0 {
			t.Fatalf("geo's decision after a purge of its last build: %d %v", n, err)
		}
		// a removed source takes its decisions
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "ci", "os:1:t") })
		if n, err := store.CountRaw(ctx, s, "decisions", "tenant_id = ?", tn.ID); err != nil || n != 0 {
			t.Fatalf("the decisions of a removed source: %d %v", n, err)
		}
	})
}

// Spec 0041: told_version is cleared by a change of the rule's source or mode; a tell requires the
// rule as its look read it, so of two looks reading one told_version only the first tells.
func TestTellWaiting(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		_, ch := fixture(t, s)
		setRule := func(source, mode string) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				_, err := tx.SetChannelRule(ctx, ch.ID, "acl", source, mode, "os:1:t")
				return err
			})
		}
		told := func() string { // of the versions told here
			t.Helper()
			for _, v := range []string{"1.0", "1.1"} {
				n, err := store.CountRaw(ctx, s, "channel_sources", "channel_id = ? AND name = 'acl' AND told_version = ?", ch.ID, v)
				if err != nil {
					t.Fatal(err)
				}
				if n == 1 {
					return v
				}
			}
			return ""
		}
		tell := func(r store.DueRule, version string) bool {
			t.Helper()
			wrote := false
			ok, err := s.TellWaiting(ctx, r, clock, version, func(*store.Tx) error { wrote = true; return nil })
			if err != nil || ok != wrote {
				t.Fatalf("tell %s: %v %v %v", version, ok, wrote, err)
			}
			return ok
		}
		setRule("tenant", store.RuleManual)
		clock = clock.Add(time.Second)
		r := store.DueRule{ChannelID: ch.ID, Name: "acl", Source: "tenant", Mode: store.RuleManual}
		if !tell(r, "1.0") || told() != "1.0" {
			t.Fatalf("the first tell: %q", told())
		}
		if tell(r, "1.0") { // a second look that read told_version empty, as the first did
			t.Fatal("a second look told again")
		}
		r.Told = "1.0"
		if !tell(r, "1.1") || told() != "1.1" {
			t.Fatalf("a newer version: %q", told())
		}
		// a mode change clears it; a look begun before the change tells nothing
		clock = clock.Add(time.Second)
		setRule("tenant", store.RuleAuto)
		if told() != "" {
			t.Fatalf("after a mode change: %q", told())
		}
		setRule("tenant", store.RuleManual)
		if ok, err := s.TellWaiting(ctx, store.DueRule{ChannelID: ch.ID, Name: "acl", Source: "tenant", Mode: store.RuleManual}, clock.Add(-time.Second),
			"1.1", func(*store.Tx) error { return nil }); err != nil || ok {
			t.Fatalf("a look begun before the rule changed told: %v %v", ok, err)
		}
		clock = clock.Add(time.Second)
		if !tell(store.DueRule{ChannelID: ch.ID, Name: "acl", Source: "tenant", Mode: store.RuleManual}, "1.1") {
			t.Fatal("after a mode change and back, no tell")
		}
		// a source change clears it too
		u := strings.Repeat("0", 8) + "-0000-0000-0000-000000000000"
		setRule(store.UpstreamSource(u), store.RuleManual)
		if told() != "" {
			t.Fatalf("after a source change: %q", told())
		}
	})
}

// Spec 0041: a block says what its body is in the tenant's catalogue: every build of it, any
// source; a body the tenant holds nothing of has none.
func TestBlockedBuilds(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ci"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		for _, src := range []string{store.SourceTenant, store.PublisherSource(p.ID)} {
			b := cppBuild(tn, "tresor", "1.3.0", "a")
			b.Source = src
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
		}
		for _, h := range []string{"a", "f"} {
			inTx(t, s, func(tx *store.Tx) error {
				return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: strings.Repeat(h, 64), Reason: "CVE", CreatedBy: "os:1:t"})
			})
		}
		all, err := s.BlockedBuilds(ctx, tn.ID, "")
		if err != nil || len(all[strings.Repeat("a", 64)]) != 2 || len(all[strings.Repeat("f", 64)]) != 0 {
			t.Fatalf("the blocks' builds: %+v %v", all, err)
		}
		one, err := s.BlockedBuilds(ctx, tn.ID, strings.Repeat("a", 64))
		if bs := one[strings.Repeat("a", 64)]; err != nil || len(one) != 1 || len(bs) != 2 || bs[0].Name != "tresor" || bs[0].Version != "1.3.0" ||
			bs[0].Platform != "linux_amd64" || bs[0].Slot != "v2.0.0" {
			t.Fatalf("one block's builds: %+v %v", one, err)
		}
	})
}
