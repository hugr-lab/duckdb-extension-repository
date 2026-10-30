package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0048 (spec 0039) needs a reader at 48: a level-47 binary would take a body an upstream
// blocked from that upstream again.
func TestMigrationRevocations(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 47); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 48 AND min_reader = 48"); err != nil || n != 1 {
			t.Fatalf("migration 0048 needs a reader at 48: %d %v", n, err)
		}
		if err := store.CheckAs(ctx, s, 47); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("a level-47 binary on a level-48 database: %v", err)
		}
	})
}

// Spec 0039's tables on every dialect: a stored revocation is due while its state is stronger than
// what was applied and lifted of it; an upstream's block only strengthens; a yank record keeps its
// first prior state and goes with its release's purge, an administrator's block of its body and its
// upstream's removal.
func TestRevocationRows(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fullTenant(t, s, "acme", "a")
		id := func(k string) string { return "a0000000-0000-0000-0000-00000000000" + k }
		upID, hash := id("u"), strings.Repeat("e", 64)
		now := store.TimeArg(s, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC))
		for _, q := range [][]any{
			{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, created_at, created_by, checked) " +
				"VALUES (?, ?, ?, 'json', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'upstream', ?, 'os:1:t', ?)", id("c"), tn.ID, store.UpstreamSource(upID), hash, now, true},
			{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state, visibility, seq, created_at, " +
				"created_by, state_changed_at, state_changed_by, version, origin, source) VALUES (?, ?, ?, ?, 'json', '1.0', 'linux_amd64', 'v2.0.0', 'yanked', " +
				"'public', 2, ?, 'os:1:t', ?, 'os:1:t', 1, 'upstream', ?)", id("s"), tn.ID, ch.ID, id("c"), now, now, store.UpstreamSource(upID)},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}
		builds, err := s.UpstreamBuilds(ctx, tn.ID, upID)
		if err != nil || builds[[4]string{"json", "1.0", "linux_amd64", hash}] != id("c") || len(builds) != 1 {
			t.Fatalf("the upstream's builds: %v %v", builds, err)
		}
		rev := store.UpstreamRevocation{UpstreamID: upID, BuildID: id("c"), TenantID: tn.ID, BodyHash: hash, Name: "json", PackageVersion: "1.0",
			Platform: "linux_amd64", State: store.RevokeYanked, Reason: "old"}
		stored := func() store.UpstreamRevocation {
			t.Helper()
			var r store.UpstreamRevocation
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				r, err = tx.GetUpstreamRevocation(ctx, upID, id("c"))
				return err
			})
			return r
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.PutUpstreamRevocation(ctx, rev) })
		if r := stored(); !r.Due() || r.Reason != "old" {
			t.Fatalf("stored: %+v", r)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.MarkRevocationApplied(ctx, upID, id("c"), store.RevokeYanked) })
		if r := stored(); r.Due() || r.AppliedState != store.RevokeYanked || r.AppliedAt.IsZero() {
			t.Fatalf("applied: %+v", r)
		}
		rev.State, rev.Reason = store.RevokeWithdrawn, "CVE"
		inTx(t, s, func(tx *store.Tx) error { return tx.PutUpstreamRevocation(ctx, rev) })
		if r := stored(); !r.Due() || r.Reason != "CVE" || r.AppliedState != store.RevokeYanked {
			t.Fatalf("stronger, kept what was applied: %+v", r)
		}
		if r := stored(); !r.Liftable() {
			t.Fatalf("a yank applied is liftable: %+v", r)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.MarkRevocationLifted(ctx, upID, id("c"), store.RevokeWithdrawn) })
		if r := stored(); r.Due() || r.LiftedState != store.RevokeWithdrawn || r.Liftable() {
			t.Fatalf("lifted: %+v", r)
		}
		// unlisted, a row something was lifted of is kept, not due; listed again, it is as it was
		lifted := stored()
		inTx(t, s, func(tx *store.Tx) error { return tx.ForgetUpstreamRevocation(ctx, lifted) })
		if r := stored(); r.UnlistedAt.IsZero() || r.LiftedState != store.RevokeWithdrawn {
			t.Fatalf("unlisted: %+v", r)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.PutUpstreamRevocation(ctx, rev) })
		if r := stored(); !r.UnlistedAt.IsZero() || r.LiftedState != store.RevokeWithdrawn || r.Due() {
			t.Fatalf("listed again: %+v", r)
		}
		// a row nothing was applied or lifted of goes
		other := rev
		other.BuildID = id("o")
		inTx(t, s, func(tx *store.Tx) error { return tx.PutUpstreamRevocation(ctx, other) })
		inTx(t, s, func(tx *store.Tx) error { return tx.ForgetUpstreamRevocation(ctx, other) })
		if n, err := store.CountRaw(ctx, s, "upstream_revocations", "build_id = ?", id("o")); err != nil || n != 0 {
			t.Fatalf("a row nothing was applied of, forgotten: %d %v", n, err)
		}

		// an upstream's block strengthens only
		blocked := func() bool {
			t.Helper()
			ok, err := s.UpstreamBlocked(ctx, upID, hash)
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		// and reports a change: new, stronger, or another reason at the same state
		for _, tc := range []struct {
			state, reason string
			changed       bool
		}{{store.RevokeBlocked, "b", true}, {store.RevokeWithdrawn, "w", true}, {store.RevokeBlocked, "b", false},
			{store.RevokeWithdrawn, "w", false}, {store.RevokeWithdrawn, "w2", true}, {store.RevokeWithdrawn, store.RevokeWithdrawn, true}} {
			inTx(t, s, func(tx *store.Tx) error {
				changed, err := tx.PutUpstreamBlock(ctx, store.UpstreamBlock{UpstreamID: upID, BodyHash: hash, TenantID: tn.ID, State: tc.state,
					Reason: tc.reason})
				if err == nil && changed != tc.changed {
					t.Errorf("%+v: changed %v", tc, changed)
				}
				return err
			})
		}
		if bs, err := s.UpstreamBlocks(ctx, tn.ID); err != nil || bs[[2]string{upID, hash}].State != store.RevokeWithdrawn || !blocked() {
			t.Fatalf("the block: %+v %v", bs, err)
		}

		// a yank record keeps its first prior state, and reads its channel
		for _, prior := range []string{store.ReleaseActive, store.ReleaseDeprecated} {
			inTx(t, s, func(tx *store.Tx) error {
				return tx.RecordUpstreamYank(ctx, store.UpstreamYank{ReleaseID: id("s"), TenantID: tn.ID, UpstreamID: upID, BuildID: id("c"),
					BodyHash: hash, PriorState: prior, Cause: store.YankCauseBlock})
			})
		}
		ys, err := s.UpstreamYanksOf(ctx, upID, id("c"))
		if err != nil || len(ys) != 1 || ys[0].PriorState != store.ReleaseActive || ys[0].ChannelID != ch.ID {
			t.Fatalf("the yank record: %+v %v", ys, err)
		}
		// an administrator's block takes over
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteUpstreamYanksWithBody(ctx, tn.ID, hash) })
		if ys, _ := s.UpstreamYanksOf(ctx, upID, id("c")); len(ys) != 0 {
			t.Fatalf("taken over: %+v", ys)
		}
		// a purge takes its release's record
		inTx(t, s, func(tx *store.Tx) error {
			return tx.RecordUpstreamYank(ctx, store.UpstreamYank{ReleaseID: id("s"), TenantID: tn.ID, UpstreamID: upID, BuildID: id("c"),
				BodyHash: hash, PriorState: store.ReleaseActive, Cause: store.YankCauseYank})
		})
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			c, err := tx.ReleaseByID(ctx, id("s"))
			if err != nil {
				return err
			}
			return tx.PurgeRelease(ctx, c)
		}); err != nil {
			t.Fatal(err)
		}
		if ys, _ := s.UpstreamYanksOf(ctx, upID, id("c")); len(ys) != 0 {
			t.Fatalf("purged: %+v", ys)
		}

		// the upstream's removal takes its revocations, blocks and records
		inTx(t, s, func(tx *store.Tx) error {
			return tx.RecordUpstreamYank(ctx, store.UpstreamYank{ReleaseID: id("r"), TenantID: tn.ID, UpstreamID: upID, BuildID: id("c"),
				BodyHash: hash, PriorState: store.ReleaseActive, Cause: store.YankCauseYank})
		})
		inTx(t, s, func(tx *store.Tx) error {
			u, err := tx.GetUpstreamByID(ctx, upID)
			if err != nil {
				return err
			}
			return tx.DeleteUpstream(ctx, u, 0)
		})
		for _, table := range []string{"upstream_revocations", "upstream_blocks", "upstream_yanks"} {
			if n, err := store.CountRaw(ctx, s, table, "upstream_id = ?", upID); err != nil || n != 0 {
				t.Fatalf("%s after the removal: %d %v", table, n, err)
			}
		}
	})
}

// A remote's reason is cleaned, never refused: printable, at most 400 bytes, cut at a rune.
func TestCleanReason(t *testing.T) {
	for in, want := range map[string]string{
		"CVE-2026\x00-1\n":       "CVE-2026 -1 ",
		strings.Repeat("é", 300): strings.Repeat("é", 200),
		"\xffbad":                " bad",
	} {
		if got := store.CleanReason(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
