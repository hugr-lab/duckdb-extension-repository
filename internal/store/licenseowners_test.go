package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// nameLicense is a tenant's one licence of a package, whatever its source (spec 0033 phase B: a
// licence per (name, source)); ErrNotFound for none.
func nameLicense(ctx context.Context, s *store.Store, tenantID, packageType, name string) (store.License, error) {
	ls, err := s.ListLicenses(ctx, tenantID, packageType)
	if err != nil {
		return store.License{}, err
	}
	var out []store.License
	for _, l := range ls {
		if l.Name == name {
			out = append(out, l)
		}
	}
	switch len(out) {
	case 0:
		return store.License{}, store.ErrNotFound
	case 1:
		return out[0], nil
	}
	return store.License{}, fmt.Errorf("%d licences of %s", len(out), name)
}

// Migration 0042 (spec 0033 phase B part 2), on a populated database at level 41: each key's owner
// for a name is its first recorder among the records that counted, as kista computes it (a source's
// own entry from its recording, the end of its hold or its first release; not revoked, not blocked;
// a distributor's publisher's not for a name a repository upstream lists; none while another
// source's earlier record is on an entry still held), and each licence gets its key's owner (a
// relayed one, an imported one), or unknown.
func TestMigrationLicenseOwners(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 41); err != nil {
			t.Fatal(err)
		}
		// the migration reads the database's clock: these hours are past, the year 2100 is to come
		at := func(h int) any { return store.TimeArg(s, time.Date(2026, 1, 10, h, 0, 0, 0, time.UTC)) }
		future := store.TimeArg(s, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		kid := func(k string) string { return "sha256:" + strings.Repeat(k, 64) }
		const tid = "t0000000-0000-0000-0000-000000000000"
		cid, uid, pid, did := id("c"), id("u"), id("p"), id("d")
		exec := func(qs ...[]any) {
			t.Helper()
			for _, q := range qs {
				if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
					t.Fatalf("%s: %v", q[0], err)
				}
			}
		}
		exec(
			[]any{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, at(1)},
			[]any{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, at(1)},
			[]any{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, created_by) " +
				"VALUES (?, ?, 'mirror', 'repository', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')", uid, tid, cid, false, at(1)},
			[]any{"INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES (?, 'listed', ?)", uid, false},
			[]any{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by, hold_seconds, license_keys) VALUES (?, ?, 'ci', ?, 'os:1:t', 0, ?)", pid, tid, at(1), true},
			[]any{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by, hold_seconds, license_keys, distributor) " +
				"VALUES (?, ?, 'enterest-ci', ?, 'os:1:t', 0, ?, 'enterest')", did, tid, at(1), true},
		)
		// builds: (id, source, name, version, available_at, revoked_at)
		build := func(b, src, name, ver string, available, revoked any) {
			t.Helper()
			exec([]any{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked, available_at, revoked_at) VALUES (?, ?, ?, ?, ?, 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, " +
				"'publication', ?, 'os:1:t', ?, ?, ?)", id(b), tid, src, name, ver, strings.Repeat(b, 64), at(2), true, available, revoked})
		}
		record := func(b, kind, src, k string, h int) {
			t.Helper()
			exec([]any{"INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at) VALUES (?, ?, ?, 'pem', ?, ?, ?)",
				tid, id(b), kid(k), kind, src, at(h)})
		}
		up, pub, dist := "upstream:"+uid, "publisher:"+pid, "publisher:"+did
		// geo: the mirror records 1 first, the publisher's copy later; the publisher's record on the
		// mirror's own build (not its source's) never counts
		build("1", up, "geo", "1.0", nil, nil)
		build("2", pub, "geo", "1.0", nil, nil)
		record("2", "publisher", pid, "1", 4)
		record("1", "upstream", uid, "1", 3)
		record("1", "publisher", pid, "2", 2)
		build("3", pub, "geo", "2.0", nil, nil)
		record("3", "publisher", pid, "2", 5)
		// listed: a distributor's record does not count for a name the tenant's repository upstream lists
		build("4", dist, "listed", "1.0", nil, nil)
		record("4", "publisher", did, "3", 2)
		// held: recorded while held, the hold still on: no owner, the mirror's later record neither;
		// then a revoked entry and a blocked body: neither counts
		build("5", pub, "held", "1.0", future, nil)
		record("5", "publisher", pid, "4", 3)
		build("7", up, "held", "1.0", nil, nil)
		record("7", "upstream", uid, "4", 4)
		build("6", pub, "revoked", "1.0", nil, at(3))
		record("6", "publisher", pid, "5", 2)
		build("8", pub, "blocked", "1.0", nil, nil)
		record("8", "publisher", pid, "8", 2)
		exec([]any{"INSERT INTO blocks (tenant_id, body_hash, reason, created_at, created_by) VALUES (?, ?, 'CVE', ?, 'os:1:t')",
			tid, strings.Repeat("8", 64), at(3)})
		// late: held until 5, recorded at 3, never placed: it counts from 5, before the mirror's at 6
		build("9", pub, "late", "1.0", at(5), nil)
		record("9", "publisher", pid, "6", 3)
		build("a", up, "late", "1.0", nil, nil)
		record("a", "upstream", uid, "6", 6)
		// early: held until 20, recorded at 1, placed early at 8: it counts from 8, after the mirror's at 4
		build("b", pub, "early", "1.0", at(20), nil)
		record("b", "publisher", pid, "7", 1)
		exec([]any{"INSERT INTO releases (id, tenant_id, channel_id, build_id, source, name, package_version, platform, slot, state, visibility, " +
			"seq, created_at, created_by, state_changed_at, state_changed_by, version, origin) VALUES (?, ?, ?, ?, ?, 'early', '1.0', " +
			"'linux_amd64', 'v2.0.0', 'active', 'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'publication')", id("r"), tid, cid, id("b"), pub, at(8), at(8)})
		build("e", up, "early", "1.0", nil, nil)
		record("e", "upstream", uid, "7", 4)
		// licences: geo relayed by the mirror (its key's owner), acl imported (no record: unknown)
		exec(
			[]any{"INSERT INTO licenses (tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at, " +
				"grace_until, source, upstream_id, visibility, actor, updated_at, version) VALUES (?, 'geo', 'l1', 'i', 'acme', '', 'pro', 't1', ?, " +
				"1, 2, 3, 'upstream', ?, 'private', 'system:licenses', ?, 1)", tid, kid("1"), uid, at(5)},
			[]any{"INSERT INTO licenses (tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at, " +
				"grace_until, source, visibility, actor, updated_at, version) VALUES (?, 'geo2', 'l2', 'i', 'acme', '', 'pro', 't2', ?, " +
				"1, 2, 3, 'import', 'private', 'os:1:t', ?, 1)", tid, kid("2"), at(5)},
			[]any{"INSERT INTO licenses (tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at, " +
				"grace_until, source, visibility, actor, updated_at, version) VALUES (?, 'listed', 'l3', 'i', 'acme', '', 'pro', 't3', ?, " +
				"1, 2, 3, 'import', 'private', 'os:1:t', ?, 1)", tid, kid("3"), at(5)},
		)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		s.Now = func() time.Time { return time.Date(2026, 1, 10, 22, 0, 0, 0, time.UTC) }
		owner := func(name, k string) (string, bool) {
			t.Helper()
			var o store.KeyOwner
			inTx(t, s, func(tx *store.Tx) (err error) {
				o, err = tx.KeyOwnerOf(ctx, tid, pkgtype.DuckDBExtension, name, kid(k))
				return err
			})
			return o.Source, o.Recorded
		}
		for _, c := range []struct {
			name, k, want string
			recorded      bool
		}{
			{"geo", "1", up, true},     // the mirror recorded first
			{"geo", "2", pub, true},    // on its own builds only
			{"listed", "3", "", false}, // a distributor's, for a name the mirror lists
			{"held", "4", "", false},   // held still, recorded before the mirror's
			{"revoked", "5", "", false},
			{"blocked", "8", "", false},
			{"late", "6", pub, true}, // its hold ended before the mirror recorded
			{"early", "7", up, true}, // placed after the mirror recorded
		} {
			if got, rec := owner(c.name, c.k); got != c.want || rec != c.recorded {
				t.Errorf("%s/%s: owner %q (recorded %v), want %q (%v)", c.name, c.k, got, rec, c.want, c.recorded)
			}
		}
		ls, err := s.ListLicenses(ctx, tid, pkgtype.DuckDBExtension)
		if err != nil || len(ls) != 3 {
			t.Fatalf("the licences: %+v %v", ls, err)
		}
		got := map[string]string{}
		for _, l := range ls {
			got[l.Name] = l.Owner
		}
		// no record of geo2's key for geo2, none counting for listed's: unknown
		if got["geo"] != up || got["geo2"] != store.LicenseUnknown || got["listed"] != store.LicenseUnknown {
			t.Fatalf("the licences' sources: %v", got)
		}
		// once the hold ended, the first record to count is the mirror's: computed, claimed by the
		// first act after
		s.Now = func() time.Time { return time.Date(2100, 1, 2, 0, 0, 0, 0, time.UTC) }
		if got, rec := owner("held", "4"); got != up || rec {
			t.Fatalf("after the hold: %q %v", got, rec)
		}
	})
}

// Spec 0033 phase B: a key's owner for a name is its first recorder, claimed when it records (a
// license.key_owner event), kept whatever another source records later (a copied key: a
// license.key_conflict event, once per source and name), not a distributor's publisher's for a name
// a repository upstream of the tenant lists, nor a held entry's until its hold ends, nor a blocked
// body's; while another source's earlier record is held, a copier gets none; an administrator gives
// it to a source that declares it, the licences signed with it moving along; the owner's removal
// leaves it without one until a source records it afresh.
func TestKeyOwners(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, prod := fixture(t, s)
		publisher := func(name, distributor string) string {
			t.Helper()
			p := store.Publisher{TenantID: tn.ID, Name: name, CreatedBy: "os:1:t"}
			inTx(t, s, func(tx *store.Tx) error {
				if err := tx.InsertPublisher(ctx, &p); err != nil {
					return err
				}
				if distributor != "" {
					if _, err := tx.SetPublisherDistributor(ctx, tn.ID, name, distributor); err != nil {
						return err
					}
				}
				_, err := tx.SetPublisherLicenseKeys(ctx, tn.ID, name, true)
				return err
			})
			return store.PublisherSource(p.ID)
		}
		p, q, d := publisher("p", ""), publisher("q", ""), publisher("enterest-ci", "enterest")
		uid := store.NewID()
		if err := store.ExecRaw(ctx, s, "INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, "+
			"request_dry_run, created_at, created_by) VALUES (?, ?, 'u', 'repository', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')",
			uid, tn.ID, prod.ID, false, store.TimeArg(s, clock)); err != nil {
			t.Fatal(err)
		}
		if err := store.ExecRaw(ctx, s, "INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES (?, 'listed', ?)", uid, false); err != nil {
			t.Fatal(err)
		}
		u := store.UpstreamSource(uid)
		key := func(k string) store.LicenseKey {
			return store.LicenseKey{Kid: "sha256:" + strings.Repeat(k, 64), PEM: "pem-" + k}
		}
		n := 0
		build := func(src, name string, held time.Duration) store.Build {
			t.Helper()
			n++
			b := cppBuild(tn, name, fmt.Sprintf("1.%d", n), fmt.Sprintf("%x", n%16))
			b.Source, b.BodyHash = src, fmt.Sprintf("%064x", n)
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			if held > 0 {
				if err := store.ExecRaw(ctx, s, "UPDATE builds SET available_at = ? WHERE id = ?", store.TimeArg(s, clock.Add(held)), b.ID); err != nil {
					t.Fatal(err)
				}
			}
			return b
		}
		record := func(b store.Build, k string) {
			t.Helper()
			kind, id, _ := strings.Cut(b.Source, ":")
			inTx(t, s, func(tx *store.Tx) error {
				_, err := tx.RecordLicenseKeys(ctx, tn.ID, b.ID, kind, id, []store.LicenseKey{key(k)})
				return err
			})
			clock = clock.Add(time.Second)
		}
		owner := func(name, k string) store.KeyOwner {
			t.Helper()
			var o store.KeyOwner
			inTx(t, s, func(tx *store.Tx) (err error) {
				o, err = tx.KeyOwnerOf(ctx, tn.ID, pkgtype.DuckDBExtension, name, key(k).Kid)
				return err
			})
			return o
		}
		events := func(kind string) []string {
			t.Helper()
			es, err := s.ListEvents(ctx, tn.ID, store.EventFilter{Kind: kind, Ascending: true})
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, e := range es {
				out = append(out, e.Data)
			}
			return out
		}

		// the first recorder owns the key, claimed at once; another source's copy changes nothing
		record(build(p, "geo", 0), "1")
		if o := owner("geo", "1"); o.Source != p || !o.Recorded || o.SetBy != store.ActorLicenses {
			t.Fatalf("the first recorder: %+v", o)
		}
		record(build(q, "geo", 0), "1")
		record(build(q, "geo", 0), "1")
		if o := owner("geo", "1"); o.Source != p {
			t.Fatalf("after a copy: %+v", o)
		}
		if es := events("license.key_owner"); len(es) != 1 || !strings.Contains(es[0], `"source":"publisher:p"`) || !strings.Contains(es[0], `"first":true`) {
			t.Fatalf("the claim's events: %v", es)
		}
		if es := events("license.key_conflict"); len(es) != 1 || !strings.Contains(es[0], `"source":"publisher:q"`) ||
			!strings.Contains(es[0], `"owner":"publisher:p"`) {
			t.Fatalf("the conflict, told once: %v", es)
		}

		// a distributor's publisher records a key of a name the tenant's upstream lists: not its own
		record(build(d, "listed", 0), "2")
		if o := owner("listed", "2"); o.Source != "" {
			t.Fatalf("a distributor's for a listed name: %+v", o)
		}
		record(build(u, "listed", 0), "2")
		if o := owner("listed", "2"); o.Source != u || !o.Recorded {
			t.Fatalf("the upstream's: %+v", o)
		}
		// and owns one of a name no upstream lists
		record(build(d, "geo_tools", 0), "3")
		if o := owner("geo_tools", "3"); o.Source != d || !o.Recorded {
			t.Fatalf("a distributor's for its own name: %+v", o)
		}

		// a held entry's record counts once its hold ended, before a later recorder's
		record(build(p, "held", time.Hour), "4")
		if o := owner("held", "4"); o.Source != "" {
			t.Fatalf("a held entry's: %+v", o)
		}
		// a copier recording inside another source's hold gets nothing meanwhile; the hold resolved (the
		// held entry withdrawn), its record counts
		held := build(u, "copied", time.Hour)
		record(held, "5")
		record(build(q, "copied", 0), "5")
		if o := owner("copied", "5"); o.Source != "" || o.Recorded {
			t.Fatalf("a copier inside a hold: %+v", o)
		}
		if err := store.ExecRaw(ctx, s, "UPDATE builds SET withdrawn_at = ? WHERE id = ?", store.TimeArg(s, clock), held.ID); err != nil {
			t.Fatal(err)
		}
		if o := owner("copied", "5"); o.Source != q || o.Recorded {
			t.Fatalf("the hold withdrawn: %+v", o)
		}
		clock = clock.Add(2 * time.Hour)
		record(build(q, "held", 0), "4") // later than the end of p's hold: p's
		if o := owner("held", "4"); o.Source != p || !o.Recorded {
			t.Fatalf("after the hold: %+v", o)
		}
		// a blocked body's record counts for nothing
		blocked := build(p, "blocked", 0)
		if err := store.ExecRaw(ctx, s, "INSERT INTO blocks (tenant_id, body_hash, reason, created_at, created_by) VALUES (?, ?, 'CVE', ?, 'os:1:t')",
			tn.ID, blocked.BodyHash, store.TimeArg(s, clock)); err != nil {
			t.Fatal(err)
		}
		record(blocked, "6")
		if o := owner("blocked", "6"); o.Source != "" {
			t.Fatalf("a blocked body's: %+v", o)
		}

		// an administrator's act: to a source that declares the key; the licences signed with it move
		inTx(t, s, func(tx *store.Tx) error {
			if _, err := tx.SetKeyOwner(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", key("1").Kid, u, "os:1:admin"); !errors.Is(err, store.ErrNotDeclared) {
				t.Errorf("to a source that never declared it: %v", err)
			}
			l := store.License{TenantID: tn.ID, Name: "geo", Owner: p, LicenseID: "lic-geo", Issuer: "i", HolderID: "acme", Plan: "pro",
				Token: "t", Key: key("1").Kid, IssuedAt: 1, ExpiresAt: 2, GraceUntil: 4102444800, Source: store.LicenseImport,
				Visibility: store.Private, Actor: "os:1:t"}
			return tx.PutLicense(ctx, &l, 0)
		})
		inTx(t, s, func(tx *store.Tx) error {
			before, err := tx.SetKeyOwner(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", key("1").Kid, q, "os:1:admin")
			if err != nil || before.Source != p {
				t.Errorf("the act: %+v %v", before, err)
			}
			moved, err := tx.MoveLicenses(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", key("1").Kid, p, q)
			if err != nil || len(moved) != 1 || moved[0].Owner != p {
				t.Errorf("moved: %+v %v", moved, err)
			}
			return err
		})
		if o := owner("geo", "1"); o.Source != q || o.SetBy != "os:1:admin" {
			t.Fatalf("after the act: %+v", o)
		}
		if l, err := nameLicense(ctx, s, tn.ID, pkgtype.DuckDBExtension, "geo"); err != nil || l.Owner != q || l.Version != 2 {
			t.Fatalf("the licence moved: %+v %v", l, err)
		}

		// the owner's removal: its keys and its ownership go; the key has no owner until a source
		// records it afresh, p's records made while q owned it counting for nothing
		kind, qid, _ := strings.Cut(q, ":")
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteLicenseKeysOf(ctx, kind, qid) })
		clock = clock.Add(time.Second)
		if o := owner("geo", "1"); o.Source != "" || o.Recorded {
			t.Fatalf("after the owner's removal: %+v", o)
		}
		inTx(t, s, func(tx *store.Tx) error {
			o, claimed, err := tx.ClaimKeyOwner(ctx, tn.ID, pkgtype.DuckDBExtension, "geo", key("1").Kid)
			if err == nil && (claimed || o.Source != "") {
				t.Errorf("a claim after the removal: %+v %v", o, claimed)
			}
			return err
		})
		record(build(p, "geo", 0), "1")
		if o := owner("geo", "1"); o.Source != p || !o.Recorded {
			t.Fatalf("recorded afresh: %+v", o)
		}
		// nor does a distributor's record, which did not count while an upstream listed the name,
		// take the key when the upstream goes: one the upstream owns recorded (2), and one it is the
		// first recorder of, computed once its hold ended (7); removed as DeleteUpstream does, its
		// keys first while it still lists the name, then its entries
		record(build(u, "listed", time.Hour), "7")
		record(build(d, "listed", 0), "7")
		clock = clock.Add(2 * time.Hour)
		if o := owner("listed", "7"); o.Source != u || o.Recorded {
			t.Fatalf("the upstream's after its hold: %+v", o)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteLicenseKeysOf(ctx, store.KeySourceUpstream, uid) })
		if err := store.ExecRaw(ctx, s, "DELETE FROM upstream_entries WHERE upstream_id = ?", uid); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"2", "7"} {
			if o := owner("listed", k); o.Source != "" {
				t.Fatalf("the distributor's %s after the upstream's removal: %+v", k, o)
			}
		}
		if es := events("license.key_owner"); !slices.ContainsFunc(es, func(e string) bool { return strings.Contains(e, `"kid":"`+key("4").Kid) }) {
			t.Fatalf("a claim after a hold is an event too: %v", es)
		}
	})
}
