package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// testType is a second package type, registered by the tests that need one (spec 0034: two names
// of one spelling in two types never collide).
const testType = "test_package"

// Migration 0039 (spec 0034 phase 2) gives every key that holds a name its package type: on a
// populated database at level 38, every row is duckdb_extension (a grant without a name "*"), is
// read back through the store, and the new keys take the same name in another type.
func TestMigrationPackageTypes(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 38); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		hash := func(c string) string { return strings.Repeat(c, 64) }
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
		exec(
			[]any{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			[]any{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, now},
			[]any{"INSERT INTO issuers (id, tenant_id, name, url, algorithms, required_claims, roles_claim, groups_claim, client_claim, max_token_lifetime, " +
				"created_at, created_by) VALUES (?, ?, 'corp', 'https://idp.example', '[]', '{}', '[]', '[]', '[]', 3600, ?, 'os:1:t')", id("i"), tid, now},
			// grants: on a name in a channel, on the channel, and on the tenant
			[]any{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, channel_id, package, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'role', 'dev', ?, 'acl', 'install', ?, 'os:1:t')", id("1"), tid, id("i"), cid, now},
			[]any{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, channel_id, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'role', 'ops', ?, 'admin', ?, 'os:1:t')", id("2"), tid, id("i"), cid, now},
			[]any{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'role', 'admin', 'admin', ?, 'os:1:t')", id("3"), tid, id("i"), now},
			// a group and its member
			[]any{"INSERT INTO package_groups (id, tenant_id, name, description, version, created_at, created_by) " +
				"VALUES (?, ?, 'platform', '', 1, ?, 'os:1:t')", id("g"), tid, now},
			[]any{"INSERT INTO package_group_members (group_id, tenant_id, package, added_at, added_by) VALUES (?, ?, 'acl', ?, 'os:1:t')", id("g"), tid, now},
			// a server reservation, a tenant's, a licence, a build
			[]any{"INSERT INTO reserved_names (name, kind, added_at) VALUES ('kista_x', 'core', ?)", now},
			[]any{"INSERT INTO reserved_name_sources (name, source, added_at) VALUES ('kista_x', 'admin:os:1:t', ?)", now},
			[]any{"INSERT INTO tenant_reserved_names (tenant_id, name, holder, added_at, added_by) VALUES (?, 'held', 'enterest', ?, 'os:1:t')", tid, now},
			[]any{"INSERT INTO licenses (tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at, " +
				"grace_until, source, visibility, actor, updated_at, version) VALUES (?, 'tresor', 'l1', 'https://issuer.example', 'acme', 'Acme', " +
				"'pro', 't.tresor', 'kid', 1, 4102444800, 4103049600, 'import', 'private', 'os:1:t', ?, 1)", tid, now},
			[]any{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, " +
				"created_at, created_by, checked) VALUES (?, ?, 'publisher:p', 'acl', '1.2.3', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, " +
				"'publication', ?, 'os:1:t', ?)", id("b"), tid, hash("1"), now, true},
			// rows referencing the build: its release and a licence key it declares (SQLite rebuilds
			// builds, and neither may go with it)
			[]any{"INSERT INTO releases (id, tenant_id, channel_id, build_id, source, name, package_version, platform, slot, state, visibility, seq, " +
				"created_at, created_by, state_changed_at, state_changed_by, version, origin) VALUES (?, ?, ?, ?, 'publisher:p', 'acl', '1.2.3', " +
				"'linux_amd64', 'v2.0.0', 'active', 'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'publication')", id("r"), tid, cid, id("b"), now, now},
			[]any{"INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at) " +
				"VALUES (?, ?, 'kid1', 'pem', 'publisher', ?, ?)", tid, id("b"), id("p"), now},
		)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}

		if ch, err := s.GetChannel(ctx, "acme", "prod"); err != nil || ch.PackageType != pkgtype.DuckDBExtension {
			t.Fatalf("the channel: %+v %v", ch, err)
		}
		a, err := s.GetTenantAuth(ctx, tid)
		if err != nil {
			t.Fatal(err)
		}
		types := map[string]string{}
		for _, g := range a.Grants {
			types[g.ID] = g.PackageType
		}
		if len(types) != 3 || types[id("1")] != pkgtype.DuckDBExtension || types[id("2")] != pkgtype.Any || types[id("3")] != pkgtype.Any {
			t.Fatalf("the grants' types: %v", types)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if n, err := tx.TenantAdminGrants(ctx, tid); err != nil || n != 1 {
				t.Errorf("tenant admin grants %d %v", n, err)
			}
			return nil
		})
		if ms, err := s.GroupMembers(ctx, id("g")); err != nil || len(ms) != 1 || ms[0].PackageType != pkgtype.DuckDBExtension || ms[0].Name != "acl" {
			t.Fatalf("members: %+v %v", ms, err)
		}
		if rs, err := s.ListReservedNames(ctx, pkgtype.DuckDBExtension, "kista_x"); err != nil || len(rs) != 1 || rs[0].Kind != store.ReservedCore ||
			!slices.Equal(rs[0].Sources, []string{"admin:os:1:t"}) {
			t.Fatalf("the reservation: %+v %v", rs, err)
		}
		if rs, err := s.ListTenantReservedNames(ctx, tid, pkgtype.DuckDBExtension, ""); err != nil || len(rs) != 1 || rs[0].Name != "held" {
			t.Fatalf("the tenant's reservations: %+v %v", rs, err)
		}
		if l, err := s.GetLicense(ctx, tid, pkgtype.DuckDBExtension, "tresor"); err != nil || l.LicenseID != "l1" || l.PackageType != pkgtype.DuckDBExtension {
			t.Fatalf("the licence: %+v %v", l, err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			b, err := tx.GetBuild(ctx, id("b"))
			if err != nil || b.PackageType != pkgtype.DuckDBExtension || b.Slot() != "v2.0.0" {
				t.Errorf("the build: %+v %v", b, err)
			}
			if kids, err := store.QueryStrings(ctx, tx, "SELECT kid FROM build_license_keys WHERE build_id = ?", id("b")); err != nil ||
				!slices.Equal(kids, []string{"kid1"}) {
				t.Errorf("the build's licence keys: %v %v", kids, err)
			}
			return nil
		})
		if rs, err := s.ListReleases(ctx, cid, "acl"); err != nil || len(rs) != 1 || rs[0].ID != id("r") || rs[0].BuildID != id("b") {
			t.Fatalf("the build's release: %+v %v", rs, err)
		}

		// a grant that names a package never holds "*"
		if err := store.ExecRaw(ctx, s, "INSERT INTO grants (id, tenant_id, issuer_id, kind, value, package_type, package, verbs, created_at, created_by) "+
			"VALUES (?, ?, ?, 'role', 'x', '*', 'acl', 'install', ?, 'os:1:t')", id("4"), tid, id("i"), now); err == nil {
			t.Fatal(`a named grant of "*" was stored`)
		}
		// the new keys take the same name in another type: a build of the same source, name and body,
		// its slot, a reservation of each kind, a licence, a member
		exec(
			[]any{"INSERT INTO builds (id, tenant_id, package_type, source, name, package_version, platform, abi, duckdb_version, slot, " +
				"body_hash, origin, created_at, created_by, checked) VALUES (?, ?, 'other_type', 'publisher:p', 'acl', '1.2.3', 'linux_amd64', " +
				"'cpp', 'v2.0.0', 'v2.0.0', ?, 'publication', ?, 'os:1:t', ?)", id("d"), tid, hash("1"), now, true},
			[]any{"INSERT INTO reserved_names (package_type, name, kind, added_at) VALUES ('other_type', 'kista_x', 'core', ?)", now},
			[]any{"INSERT INTO reserved_name_sources (package_type, name, source, added_at) VALUES ('other_type', 'kista_x', 'admin:os:1:t', ?)", now},
			[]any{"INSERT INTO tenant_reserved_names (tenant_id, package_type, name, holder, added_at, added_by) VALUES (?, 'other_type', 'held', 'x', ?, 'os:1:t')", tid, now},
			[]any{"INSERT INTO licenses (tenant_id, package_type, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, " +
				"expires_at, grace_until, source, visibility, actor, updated_at, version) VALUES (?, 'other_type', 'tresor', 'l2', " +
				"'https://issuer.example', 'acme', 'Acme', 'pro', 't.x', 'kid', 1, 4102444800, 4103049600, 'import', 'private', 'os:1:t', ?, 1)", tid, now},
			[]any{"INSERT INTO package_group_members (group_id, tenant_id, package_type, package, added_at, added_by) VALUES (?, ?, 'other_type', 'acl', ?, 'os:1:t')",
				id("g"), tid, now},
		)
		// within a type the slot holds one body
		if err := store.ExecRaw(ctx, s, "INSERT INTO builds (id, tenant_id, package_type, source, name, package_version, platform, abi, "+
			"duckdb_version, slot, body_hash, origin, created_at, created_by, checked) VALUES (?, ?, 'other_type', 'publisher:p', 'acl', "+
			"'1.2.3', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'publication', ?, 'os:1:t', ?)", id("e"), tid, hash("2"), now, true); err == nil {
			t.Fatal("a second body in a type's slot was stored")
		}
		// a reservation's source is of its own type's name
		if err := store.ExecRaw(ctx, s, "INSERT INTO reserved_name_sources (package_type, name, source, added_at) VALUES ('third_type', 'kista_x', 'builtin', ?)",
			now); err == nil && e.Name != "sqlite" { // SQLite checks foreign keys only where they are on
			t.Fatal("a source of a name not reserved in its type was stored")
		}
		// the longest key a source takes: a name of 64 characters and an administrator's source of 386
		// (SQL Server's clustered keys stop at 900 bytes; this one is nonclustered)
		long := strings.Repeat("n", 64)
		inTx(t, s, func(tx *store.Tx) error {
			if _, err := tx.ReserveName(ctx, pkgtype.DuckDBExtension, long, store.ReservedCore, "admin:"+strings.Repeat("a", 380)); err != nil {
				return err
			}
			return tx.BumpReservedVersion(ctx)
		})
		if rs, err := s.ListReservedNames(ctx, pkgtype.DuckDBExtension, long); err != nil || len(rs) != 1 || len(rs[0].Sources) != 1 ||
			len(rs[0].Sources[0]) != 386 {
			t.Fatalf("the longest source: %+v %v", rs, err)
		}
		// a type this binary does not know is read as nothing: the rows above stay unseen
		if ms, err := s.GroupMembers(ctx, id("g")); err != nil || len(ms) != 1 {
			t.Fatalf("members with an unknown type's: %+v %v", ms, err)
		}
		if gr, err := s.GetPackageGroup(ctx, tid, "platform"); err != nil || gr.Members != 1 {
			t.Fatalf("the member count with an unknown type's: %+v %v", gr, err)
		}
		if l, err := s.GetLicense(ctx, tid, pkgtype.DuckDBExtension, "tresor"); err != nil || l.LicenseID != "l1" {
			t.Fatalf("the licence: %+v %v", l, err)
		}
	})
}

// A second package type (spec 0034): the same name in two types never collides in the catalogue,
// its slots, the reservations, the licences or the groups, and grants and channels keep to their
// type. Once the type is not registered any more (a downgrade), its rows are read as nothing.
func TestPackageTypes(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		unregister := pkgtype.RegisterForTest(testType)
		defer func() { unregister() }()
		s := e.Fresh(t, 1)[0] // its own database: the server's reserved names are shared by every test's
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		tn, ch := fixture(t, s)
		drivers := store.Channel{TenantID: tn.ID, Name: "drivers", Kind: store.ChannelSigned, PackageType: testType}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &drivers) })
		for name, bad := range map[string]store.Channel{
			"passthrough of another type": {TenantID: tn.ID, Name: "core2", Kind: store.ChannelPassthrough, PackageType: testType},
			"an unknown type":             {TenantID: tn.ID, Name: "x", Kind: store.ChannelSigned, PackageType: "nope"},
			"any type":                    {TenantID: tn.ID, Name: "y", Kind: store.ChannelSigned, PackageType: pkgtype.Any},
		} {
			c := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateChannel(ctx, &c) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if ct, err := s.ChannelType(ctx, tn.ID, "drivers"); err != nil || ct != testType {
			t.Fatalf("the channel's type: %q %v", ct, err)
		}

		// the catalogue: one source's build of one name and body in each type, and their slots
		pub := store.Publisher{TenantID: tn.ID, Name: "p"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &pub) })
		psrc := store.PublisherSource(pub.ID)
		build := func(typ, body string) store.Build {
			b := cppBuild(tn, "snowflake", "1.0.0", body)
			b.PackageType, b.Source, b.Origin = typ, psrc, store.OriginPublication
			inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &b) })
			return b
		}
		x, y := build(pkgtype.DuckDBExtension, "1"), build(testType, "1")
		if x.ID == y.ID || !x.Inserted || !y.Inserted || y.PackageType != testType {
			t.Fatalf("one body in two types: %+v %+v", x, y)
		}
		// another body in the other type's slot is that type's own matter
		taken := cppBuild(tn, "snowflake", "1.0.0", "2")
		taken.PackageType, taken.Source = testType, psrc
		if err := s.SlotTaken(ctx, taken); !errors.Is(err, store.ErrCatalogueSlot) {
			t.Fatalf("the slot within a type: %v", err)
		}
		other := cppBuild(tn, "snowflake", "1.0.0", "3")
		q := store.Publisher{TenantID: tn.ID, Name: "q"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &q) })
		other.PackageType, other.Source, other.Origin = "", store.PublisherSource(q.ID), store.OriginPublication
		inTx(t, s, func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &other) })
		for typ, want := range map[string]string{pkgtype.DuckDBExtension: x.ID, testType: y.ID} {
			bs, err := s.SourceBuilds(ctx, tn.ID, typ, psrc, "snowflake")
			if err != nil || len(bs) != 1 || bs[0].ID != want {
				t.Fatalf("%s's builds: %+v %v", typ, bs, err)
			}
			vs, err := s.SourceVersions(ctx, tn.ID, typ)
			if err != nil || len(vs[[2]string{psrc, "snowflake"}]) != 1 {
				t.Fatalf("%s's catalogue: %v %v", typ, vs, err)
			}
			if b, ok, err := s.BuildOf(ctx, tn.ID, typ, psrc, "snowflake", strings.Repeat("1", 64)); err != nil || !ok || b.ID != want {
				t.Fatalf("%s's build of the body: %+v %v %v", typ, b, ok, err)
			}
		}

		// the reservations: the server's per (type, name), the tenant's too
		inTx(t, s, func(tx *store.Tx) error {
			changed, err := tx.ReserveName(ctx, testType, "httpfs", store.ReservedCommunity, "admin:os:1:t")
			if err != nil || !changed {
				t.Errorf("httpfs in another type: %v %v", changed, err)
			}
			if k, err := tx.ReservedKind(ctx, testType, "httpfs"); err != nil || k != store.ReservedCommunity {
				t.Errorf("its kind: %q %v", k, err)
			}
			if k, err := tx.ReservedKind(ctx, pkgtype.DuckDBExtension, "httpfs"); err != nil || k != store.ReservedCore {
				t.Errorf("DuckDB's httpfs: %q %v", k, err)
			}
			if _, err := tx.ReserveName(ctx, "nope", "x", store.ReservedCore, "admin:os:1:t"); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("an unknown type: %v", err)
			}
			return tx.BumpReservedVersion(ctx)
		})
		if ns, _, err := s.ReservedNames(ctx); err != nil || ns[pkgtype.Key{Type: testType, Name: "httpfs"}] != store.ReservedCommunity ||
			ns[pkgtype.Of("httpfs")] != store.ReservedCore {
			t.Fatalf("the list: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			for _, typ := range []string{pkgtype.DuckDBExtension, testType} {
				r := store.TenantReservedName{TenantID: tn.ID, PackageType: typ, Name: "snowflake", Holder: "h-" + typ, AddedBy: "os:1:t"}
				if _, changed, err := tx.ReserveTenantName(ctx, &r); err != nil || !changed {
					t.Errorf("%s: %v %v", typ, changed, err)
				}
			}
			if _, err := tx.ReleaseTenantName(ctx, tn.ID, pkgtype.DuckDBExtension, "snowflake"); err != nil {
				return err
			}
			held, err := tx.TenantReserves(ctx, tn.ID, testType, "snowflake")
			if err != nil || !held {
				t.Errorf("the other type's reservation: %v %v", held, err)
			}
			return nil
		})
		if set, err := s.TenantReservedSet(ctx, tn.ID, pkgtype.DuckDBExtension); err != nil || set["snowflake"] {
			t.Fatalf("DuckDB's set: %v %v", set, err)
		}

		// the licences: one per (type, name)
		for _, typ := range []string{pkgtype.DuckDBExtension, testType} {
			l := testLicense(tn.ID, "snowflake", "acme")
			l.PackageType, l.LicenseID = typ, "lic-"+typ
			if err := putLicense(s, l); err != nil {
				t.Fatalf("%s: %v", typ, err)
			}
		}
		for _, typ := range []string{pkgtype.DuckDBExtension, testType} {
			if l, err := s.GetLicense(ctx, tn.ID, typ, "snowflake"); err != nil || l.LicenseID != "lic-"+typ {
				t.Fatalf("%s's licence: %+v %v", typ, l, err)
			}
			if ls, err := s.ListLicenses(ctx, tn.ID, typ); err != nil || len(ls) != 1 || ls[0].PackageType != typ {
				t.Fatalf("%s's licences: %+v %v", typ, ls, err)
			}
		}

		// a group holds the name in each type; the grants keep to their type
		g := store.PackageGroup{TenantID: tn.ID, Name: "warehouse", CreatedBy: "os:1:t"}
		is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertIssuer(ctx, &is); err != nil {
				return err
			}
			if err := tx.InsertPackageGroup(ctx, &g); err != nil {
				return err
			}
			added, _, err := tx.ChangeGroupMembers(ctx, g, []pkgtype.Key{pkgtype.Of("snowflake"), {Type: testType, Name: "snowflake"}}, nil, "os:1:t")
			if err == nil && len(added) != 2 {
				t.Errorf("added %v", added)
			}
			return err
		})
		if gs, err := s.ListPackageGroups(ctx, tn.ID, testType, "snowflake"); err != nil || len(gs) != 1 {
			t.Fatalf("the groups holding the other type's name: %+v %v", gs, err)
		}
		grant := func(gr store.Grant) error {
			gr.TenantID, gr.IssuerID, gr.Kind, gr.CreatedBy = tn.ID, is.ID, store.PrincipalRole, "os:1:t"
			if gr.Value == "" {
				gr.Value = "team"
			}
			return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &gr) })
		}
		for name, bad := range map[string]store.Grant{
			`"*" with a name`:            {Package: "snowflake", PackageType: pkgtype.Any, Verbs: []string{"install"}},
			"an unknown type":            {PackageType: "nope", Verbs: []string{"install"}},
			"a channel of another type":  {ChannelID: ch.ID, PackageType: testType, Verbs: []string{"install"}},
			"a group grant with a type":  {PackageGroupID: g.ID, PackageType: testType, Verbs: []string{"install"}},
			"a named one of another one": {ChannelID: drivers.ID, Package: "snowflake", PackageType: pkgtype.DuckDBExtension, Verbs: []string{"install"}},
			"audit of one type":          {PackageType: testType, Verbs: []string{"audit"}},
			"audit of DuckDB's":          {PackageType: pkgtype.DuckDBExtension, Verbs: []string{"admin", "audit"}},
		} {
			if err := grant(bad); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for _, ok := range []store.Grant{
			{Value: "drivers", PackageType: testType, Verbs: []string{"admin"}},
			{Value: "drivers", ChannelID: drivers.ID, PackageType: testType, Verbs: []string{"install"}},
			{Value: "drivers", Package: "snowflake", PackageType: testType, Verbs: []string{"install"}},
			{Value: "all", Verbs: []string{"admin"}}, // "*": the tenant
		} {
			if err := grant(ok); err != nil {
				t.Fatalf("%+v: %v", ok, err)
			}
		}
		// a grant on a channel of the channel's type is "*" on it: stored so, and the same grant
		if err := grant(store.Grant{Value: "drivers", ChannelID: drivers.ID, Verbs: []string{"install"}}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same grant on the channel of any type: %v", err)
		}
		if err := grant(store.Grant{Value: "prod", ChannelID: ch.ID, Verbs: []string{"install"}}); err != nil {
			t.Fatal(err)
		}
		if err := grant(store.Grant{Value: "prod", ChannelID: ch.ID, PackageType: pkgtype.DuckDBExtension, Verbs: []string{"install"}}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same grant on the channel of its type: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			if n, err := tx.TenantAdminGrants(ctx, tn.ID); err != nil || n != 1 { // a typed grant administers no tenant
				t.Errorf("tenant admin grants %d %v", n, err)
			}
			return nil
		})
		a, err := s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(a.Grants) != 5 || len(a.Groups[g.ID].Members) != 2 {
			t.Fatalf("auth %+v %v", a, err)
		}
		for _, gr := range a.Grants {
			if gr.ChannelID != "" && (gr.PackageType != pkgtype.Any || gr.ChannelType != map[string]string{drivers.ID: testType, ch.ID: pkgtype.DuckDBExtension}[gr.ChannelID]) {
				t.Fatalf("a channel's grant: %+v", gr)
			}
		}

		// landed builds of the other type: a crowded name (two versions) and a mirror's stale cell
		more := cppBuild(tn, "snowflake", "1.1.0", "5")
		more.PackageType, more.Source, more.Origin = testType, psrc, store.OriginPublication
		mirrored := cppBuild(tn, "snowflake", "1.0.0", "6")
		gone := store.Upstream{TenantID: tn.ID, Name: "gone", Kind: store.UpstreamRepo, Prefix: "https://r.example", ChannelID: ch.ID,
			Mode: store.ModeMirror, Visibility: store.Public, State: store.UpstreamActive, CreatedBy: "os:1:t", FetchMode: store.FetchEager}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertUpstream(ctx, &gone) })
		mirrored.PackageType, mirrored.Source, mirrored.Origin = testType, store.UpstreamSource(gone.ID), store.OriginUpstream
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.FindOrInsertBuild(ctx, &more); err != nil {
				return err
			}
			if err := tx.FindOrInsertBuild(ctx, &mirrored); err != nil {
				return err
			}
			return tx.DeleteUpstream(ctx, gone, 0) // its builds stay, of a removed upstream
		})
		if err := store.ExecRaw(ctx, s, "UPDATE builds SET landed = ? WHERE package_type = ?", true, testType); err != nil {
			t.Fatal(err)
		}
		landedOf := func(f func(context.Context) ([]store.LandedBuild, error)) int {
			t.Helper()
			bs, err := f(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, b := range bs {
				if b.PackageType == testType {
					n++
				}
			}
			return n
		}
		crowded := func(ctx context.Context) ([]store.LandedBuild, error) { return s.CrowdedLanded(ctx, 1) }
		if c, m := landedOf(crowded), landedOf(s.StaleMirrorLanded); c != 2 || m != 1 {
			t.Fatalf("the other type's landed builds: %d crowded, %d stale", c, m)
		}

		// a downgrade: the type is not known here any more, and its rows are read as nothing
		unregister()
		unregister = func() {}
		if _, err := s.GetServeChannel(ctx, "acme", "drivers"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a channel of an unknown type is served: %v", err)
		}
		if chs, err := s.ListChannels(ctx, "acme"); err != nil || len(chs) != 2 || len(store.KnownChannels(chs)) != 1 ||
			store.KnownChannels(chs)[0].Name != "prod" {
			t.Fatalf("the known channels: %+v %v", chs, err)
		}
		a, err = s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(a.Grants) != 2 || a.Grants[0].Value != "all" || a.Grants[1].Value != "prod" || len(a.Groups[g.ID].Members) != 1 {
			t.Fatalf("auth with an unknown type's rows: %+v %v", a, err)
		}
		if ms, err := s.GroupMembers(ctx, g.ID); err != nil || len(ms) != 1 || ms[0].PackageType != pkgtype.DuckDBExtension {
			t.Fatalf("members: %+v %v", ms, err)
		}
		if gr, err := s.GetPackageGroup(ctx, tn.ID, "warehouse"); err != nil || gr.Members != 1 {
			t.Fatalf("the group's member count: %+v %v", gr, err)
		}
		if ls, err := s.ExpiredLicenses(ctx, 1<<40); err != nil || len(ls) != 1 || ls[0].PackageType != pkgtype.DuckDBExtension {
			t.Fatalf("expired licences of the types known: %+v %v", ls, err)
		}
		// nor are its landed builds unlanded here
		if c, m := landedOf(crowded), landedOf(s.StaleMirrorLanded); c != 0 || m != 0 {
			t.Fatalf("an unknown type's landed builds: %d crowded, %d stale", c, m)
		}
	})
}
