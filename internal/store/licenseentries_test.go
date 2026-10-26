package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// nameLicense is a tenant's one licence of a package, whatever its entry (spec 0035: a licence per
// catalogue entry); ErrNotFound for none.
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

// Migration 0043 (spec 0035 phase 1), on a populated database at level 42: relayed licences move
// into their relaying upstream's entry (the row there staying, of several movers the one that
// expires last, a chain of moves meeting nothing, visibility kept), imports of no entry go and the
// others stay, every relaying upstream is asked for a run, and the owners' table and its index go.
func TestMigrationLicenseEntries(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 42); err != nil {
			t.Fatal(err)
		}
		at := store.TimeArg(s, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		const tid = "t0000000-0000-0000-0000-000000000000"
		cid := id("c")
		exec := func(qs ...[]any) {
			t.Helper()
			for _, q := range qs {
				if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
					t.Fatalf("%s: %v", q[0], err)
				}
			}
		}
		upstream := func(k, mode string, credential any) []any {
			return []any{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, " +
				"created_by, credential) VALUES (?, ?, ?, 'repository', ?, ?, 'public', 'active', 1, ?, ?, 'os:1:t', ?)", id(k), tid, "u-" + k, cid, mode,
				false, at, credential}
		}
		exec(
			[]any{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, at},
			[]any{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, at},
			upstream("a", "mirror", "enterest"), upstream("b", "mirror", "enterest"), upstream("d", "bundle", nil), upstream("e", "mirror", nil),
			[]any{"INSERT INTO license_syncs (upstream_id, tenant_id, last_run_at, last_result) VALUES (?, ?, ?, '{}')", id("a"), tid, at},
		)
		ua, ub := "upstream:"+id("a"), "upstream:"+id("b")
		const pub = "publisher:p"
		// (name, licence id, entry, how it arrived, its relaying upstream, expires, visibility)
		lic := func(name, lid, entry, source string, upstreamID any, expires int, visibility string) []any {
			return []any{"INSERT INTO licenses (tenant_id, name, owner_source, license_id, issuer, holder_id, holder, plan_name, token, key_fp, " +
				"issued_at, expires_at, grace_until, source, upstream_id, visibility, actor, updated_at, version) VALUES (?, ?, ?, ?, 'i', 'acme', '', " +
				"'pro', ?, 'kid', 1, ?, 4102444800, ?, ?, ?, 'os:1:t', ?, 1)", tid, name, entry, lid, "t." + lid, expires, source, upstreamID, visibility, at}
		}
		exec(
			// geo: a's own row stays; b's copy under another entry moves to b's, public; a's copy under the
			// publisher's goes, a's entry holding one
			lic("geo", "l1", ua, "upstream", id("a"), 10, "private"),
			lic("geo", "l2", "publisher:q", "upstream", id("b"), 10, "public"),
			lic("geo", "l3", pub, "upstream", id("a"), 30, "private"),
			// tools: two of b's movers into b's empty entry: the one that expires last stays
			lic("tools", "l4", pub, "upstream", id("b"), 10, "private"),
			lic("tools", "l5", "unknown", "upstream", id("b"), 20, "public"),
			// keep: an import into a's entry stays, a's relayed one of no entry goes
			lic("keep", "l6", ua, "import", nil, 10, "private"),
			lic("keep", "l7", "unknown", "upstream", id("a"), 50, "private"),
			// chain: a's row under b's entry moves to a's, b's under the publisher's to b's
			lic("chain", "l8", ub, "upstream", id("a"), 10, "private"),
			lic("chain", "l9", pub, "upstream", id("b"), 10, "private"),
			// a bundle's licence moves to its bundle-mode upstream's entry
			lic("bun", "la", pub, "upstream", id("d"), 10, "public"),
			// imports: of no entry they go, of one they stay
			lic("imp", "lb", "unknown", "import", nil, 10, "private"),
			lic("imp2", "lc", pub, "import", nil, 10, "public"),
			// no relaying upstream: a relayed row without one (none is written so) goes
			lic("lost", "ld", pub, "upstream", nil, 10, "private"),
		)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		ls, err := s.ListLicenses(ctx, tid, pkgtype.DuckDBExtension)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range ls {
			got = append(got, fmt.Sprintf("%s/%s@%s:%s", l.Name, l.LicenseID, strings.TrimSuffix(l.Entry, "0000000-0000-0000-0000-000000000000"), l.Visibility))
		}
		want := "bun/la@upstream:d:public chain/l8@upstream:a:private chain/l9@upstream:b:private geo/l1@upstream:a:private " +
			"geo/l2@upstream:b:public imp2/lc@publisher:p:public keep/l6@upstream:a:private tools/l5@upstream:b:public"
		if strings.Join(got, " ") != want {
			t.Fatalf("the licences:\n%s\nwant\n%s", strings.Join(got, " "), want)
		}
		for _, l := range ls {
			if l.Source == store.LicenseUpstream && l.Entry != store.UpstreamSource(l.UpstreamID) {
				t.Errorf("a relayed licence outside its upstream's entry: %+v", l)
			}
		}
		// every relaying upstream is asked for a run: a's row requested, b's inserted; the bundle-mode
		// and credential-less upstreams relay nothing
		if n, err := store.CountRaw(ctx, s, "license_syncs", "requested_at IS NOT NULL AND upstream_id IN (?, ?)", id("a"), id("b")); err != nil || n != 2 {
			t.Fatalf("relay runs requested: %d %v", n, err)
		}
		if n, err := store.CountRaw(ctx, s, "license_syncs", "upstream_id IN (?, ?)", id("d"), id("e")); err != nil || n != 0 {
			t.Fatalf("relay rows of upstreams that do not relay: %d %v", n, err)
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 43 AND min_reader = 43"); err != nil || n != 1 {
			t.Fatalf("0043's min_reader: %d %v", n, err)
		}
		if _, err := store.CountRaw(ctx, s, "license_key_owners", "1 = 1"); err == nil {
			t.Fatal("the owners' table is still there")
		}
		idx := map[string][2]string{"sqlite": {"sqlite_master", "type = 'index' AND name = ?"}, "postgres": {"pg_indexes", "indexname = ?"},
			"sqlserver": {"sys.indexes", "name = ?"}}[e.Name]
		if n, err := store.CountRaw(ctx, s, idx[0], idx[1], "ix_build_license_keys_kid"); err != nil || n != 0 {
			t.Fatalf("the kid index: %d %v", n, err)
		}
	})
}

// Spec 0035: the live keys of an entry are those its source recorded on its own builds of the name:
// live (placed early included) or of its newest available version; never a held, revoked,
// withdrawn or blocked build's, another source's build's, or a removed source's.
func TestLiveKeysPerEntry(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		clock := time.Now().UTC().Truncate(time.Second)
		s.Now = func() time.Time { return clock }
		tn, prod := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "p", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &p); err != nil {
				return err
			}
			_, err := tx.SetPublisherLicenseKeys(ctx, tn.ID, "p", true)
			return err
		})
		uid := store.NewID()
		if err := store.ExecRaw(ctx, s, "INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, "+
			"request_dry_run, created_at, created_by) VALUES (?, ?, 'u', 'repository', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')",
			uid, tn.ID, prod.ID, false, store.TimeArg(s, clock)); err != nil {
			t.Fatal(err)
		}
		ps, us := store.PublisherSource(p.ID), store.UpstreamSource(uid)
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
		key := func(k string) store.LicenseKey {
			return store.LicenseKey{Kid: "sha256:" + strings.Repeat(k, 64), PEM: "pem-" + k}
		}
		record := func(b store.Build, src, k string) {
			t.Helper()
			kind, id, _ := strings.Cut(src, ":")
			inTx(t, s, func(tx *store.Tx) error {
				_, err := tx.RecordLicenseKeys(ctx, tn.ID, b.ID, kind, id, []store.LicenseKey{key(k)})
				return err
			})
		}
		live := func(name, src string) string {
			t.Helper()
			var ks []store.LicenseKey
			inTx(t, s, func(tx *store.Tx) (err error) {
				ks, err = tx.LiveLicenseKeys(ctx, tn.ID, pkgtype.DuckDBExtension, name, src)
				return err
			})
			var out []string
			for _, k := range ks {
				out = append(out, k.Kid[7:8])
			}
			return strings.Join(out, ",")
		}
		listed := func(name string) string {
			t.Helper()
			ks, err := s.TenantLicenseKeys(ctx, tn.ID, pkgtype.DuckDBExtension, name, "")
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, k := range ks {
				for _, src := range k.Sources {
					out = append(out, k.Kid[7:8]+"@"+src.Kind)
				}
			}
			return strings.Join(out, ",")
		}

		// own builds only: the upstream's record on the publisher's build counts for neither
		geo := build(ps, "geo", 0)
		record(geo, us, "1")
		if live("geo", ps)+live("geo", us)+listed("geo") != "" {
			t.Fatalf("a record on another source's build: %q %q %q", live("geo", ps), live("geo", us), listed("geo"))
		}
		record(geo, ps, "2")
		if live("geo", ps) != "2" || listed("geo") != "2@publisher" {
			t.Fatalf("the publisher's own: %q %q", live("geo", ps), listed("geo"))
		}

		// held: nothing; placed early: live; its hold ended: of its newest available version
		held := build(us, "held", time.Hour)
		record(held, us, "3")
		if live("held", us)+listed("held") != "" {
			t.Fatalf("a held build's: %q %q", live("held", us), listed("held"))
		}
		r := store.Release{TenantID: tn.ID, ChannelID: prod.ID, BuildID: held.ID, Name: "held", Source: us, PackageVersion: held.PackageVersion,
			Platform: held.Platform, Slot: held.Slot(), State: store.ReleaseActive, Visibility: store.Private}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertRelease(ctx, &r, "os:1:t"); err != nil {
				return err
			}
			_, err := tx.SetChannelSource(ctx, prod.ID, "held", us, "os:1:t")
			return err
		})
		if live("held", us) != "3" || listed("held") != "3@upstream" {
			t.Fatalf("placed early: %q %q", live("held", us), listed("held"))
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.SetReleaseState(ctx, &r, store.ReleaseYanked, "os:1:t") })
		if live("held", us) != "" {
			t.Fatalf("yanked while held: %q", live("held", us))
		}
		clock = clock.Add(2 * time.Hour)
		if live("held", us) != "3" || listed("held") != "3@upstream" {
			t.Fatalf("after the hold: %q %q", live("held", us), listed("held"))
		}

		// revoked, withdrawn, blocked: the newest available version is the one before, or none
		for i, c := range []struct{ name, set string }{
			{"revoked", "UPDATE builds SET revoked_at = ? WHERE id = ?"},
			{"withdrawn", "UPDATE builds SET withdrawn_at = ? WHERE id = ?"},
			{"blocked", ""},
		} {
			k := fmt.Sprintf("%d", 4+i)
			b := build(ps, c.name, 0)
			record(b, ps, k)
			if live(c.name, ps) != k {
				t.Fatalf("%s before: %q", c.name, live(c.name, ps))
			}
			if c.set != "" {
				if err := store.ExecRaw(ctx, s, c.set, store.TimeArg(s, clock), b.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				inTx(t, s, func(tx *store.Tx) error {
					return tx.InsertBlock(ctx, &store.Block{TenantID: tn.ID, BodyHash: b.BodyHash, Reason: "CVE", CreatedBy: "os:1:t"})
				})
			}
			if live(c.name, ps)+listed(c.name) != "" {
				t.Fatalf("%s: %q %q", c.name, live(c.name, ps), listed(c.name))
			}
		}

		// a removed source: its keys count for nothing, recorded or not
		if err := store.ExecRaw(ctx, s, "DELETE FROM upstreams WHERE id = ?", uid); err != nil {
			t.Fatal(err)
		}
		if live("held", us)+listed("held") != "" {
			t.Fatalf("a removed source's: %q %q", live("held", us), listed("held"))
		}
	})
}
