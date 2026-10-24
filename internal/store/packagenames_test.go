package store_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Migration 0038 (spec 0034 phase 1) renames the schema to the package vocabulary: every row of a
// populated database is read back through the store, unchanged, and no table, column, index or
// constraint keeps an old name (SQLite's constraint names inside a table's definition aside).
func TestMigrationPackageNames(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 37); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
		id := func(k string) string { return k + "0000000-0000-0000-0000-000000000000" }
		hash := func(c string) string { return strings.Repeat(c, 64) }
		const tid = "t0000000-0000-0000-0000-000000000000"
		cid := id("c")
		for _, q := range [][]any{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", tid, now},
			{"INSERT INTO channels (id, tenant_id, name, kind, created_at, version, release_version) VALUES (?, ?, 'prod', 'signed', ?, 1, 1)", cid, tid, now},
			{"INSERT INTO issuers (id, tenant_id, name, url, algorithms, required_claims, roles_claim, groups_claim, client_claim, max_token_lifetime, " +
				"created_at, created_by) VALUES (?, ?, 'corp', 'https://idp.example', '[]', '{}', '[]', '[]', '[]', 3600, ?, 'os:1:t')", id("i"), tid, now},
			{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, 'ci', ?, 'os:1:t')", id("p"), tid, now},
			// grants: on a name in a channel, a publisher's on a name, and one on the tenant
			{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, channel_id, extension, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'role', 'dev', ?, 'acl', 'install', ?, 'os:1:t')", id("1"), tid, id("i"), cid, now},
			{"INSERT INTO grants (id, tenant_id, publisher_id, kind, value, extension, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'publisher', ?, 'tresor', 'publish', ?, 'os:1:t')", id("2"), tid, id("p"), id("p"), now},
			{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, 'role', 'admin', 'admin', ?, 'os:1:t')", id("3"), tid, id("i"), now},
			// a group, its members and a grant on it
			{"INSERT INTO extension_groups (id, tenant_id, name, description, version, created_at, created_by) " +
				"VALUES (?, ?, 'platform', 'hugr', 3, ?, 'os:1:t')", id("g"), tid, now},
			{"INSERT INTO extension_group_members (group_id, tenant_id, extension, added_at, added_by) VALUES (?, ?, 'acl', ?, 'os:1:t')", id("g"), tid, now},
			{"INSERT INTO extension_group_members (group_id, tenant_id, extension, added_at, added_by) VALUES (?, ?, 'tresor', ?, 'os:1:t')", id("g"), tid, now},
			{"INSERT INTO extension_group_grants (id, tenant_id, group_id, issuer_id, kind, value, channel_id, verbs, created_at, created_by) " +
				"VALUES (?, ?, ?, ?, 'role', 'ops', ?, 'install', ?, 'os:1:t')", id("4"), tid, id("g"), id("i"), cid, now},
			// a build, its release, a purged slot
			{"INSERT INTO builds (id, tenant_id, source, name, ext_version, platform, abi, duckdb_version, slot, body_hash, origin, created_at, created_by, checked) " +
				"VALUES (?, ?, 'tenant', 'acl', '1.2.3', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'admin', ?, 'os:1:t', ?)", id("b"), tid, hash("1"), now, true},
			{"INSERT INTO releases (id, tenant_id, channel_id, build_id, source, name, ext_version, platform, slot, state, visibility, seq, created_at, " +
				"created_by, state_changed_at, state_changed_by, version, origin) VALUES (?, ?, ?, ?, 'tenant', 'acl', '1.2.3', 'linux_amd64', 'v2.0.0', " +
				"'active', 'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'admin')", id("r"), tid, cid, id("b"), now, now},
			{"INSERT INTO purged_slots (channel_id, source, name, ext_version, platform, slot, abi, body_hash, seq, release_id, purged_at) " +
				"VALUES (?, 'tenant', 'acl', '0.9', 'linux_amd64', 'v2.0.0', 'cpp', ?, 7, ?, ?)", cid, hash("2"), id("q"), now},
			// an upstream's allowlist with versions
			{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, created_by) " +
				"VALUES (?, ?, 'core', 'duckdb-core', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')", id("u"), tid, cid, false, now},
			{"INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES (?, 'json', ?)", id("u"), false},
			{"INSERT INTO upstream_versions (upstream_id, name, ext_version) VALUES (?, 'json', '1.0')", id("u")},
			// statistics and usage, and usage received from downstream
			{"INSERT INTO download_counts (tenant_id, channel_id, name, ext_version, platform, duckdb_version, day, authenticated, count) " +
				"VALUES (?, ?, 'acl', '1.2.3', 'linux_amd64', 'v2.0.0', '2026-10-11', ?, 3)", tid, cid, false},
			{"INSERT INTO download_installers (tenant_id, channel_id, name, ext_version, platform, day, installers) " +
				"VALUES (?, ?, 'acl', '1.2.3', 'linux_amd64', '2026-10-11', 2)", tid, cid},
			{"INSERT INTO usage_counts (tenant_id, channel_id, name, ext_version, event, counter, status, day, authenticated, value) " +
				"VALUES (?, ?, 'acl', '1.2.3', 'load', '', 'valid', '2026-10-11', ?, 5)", tid, cid, true},
			{"INSERT INTO usage_users (tenant_id, channel_id, name, ext_version, day, users) VALUES (?, ?, 'acl', '1.2.3', '2026-10-11', 4)", tid, cid},
			{"INSERT INTO usage_received (tenant_id, channel_id, sender, day, name, ext_version, event, counter, users, value) " +
				"VALUES (?, ?, 'x', '2026-10-11', 'tresor', '0.4', 'load', '', 0, 6)", tid, cid},
		} {
			if err := store.ExecRaw(ctx, s, q[0].(string), q[1:]...); err != nil {
				t.Fatalf("%s: %v", q[0], err)
			}
		}

		// the old names in the schema: tables, columns, indexes and constraints
		var q string
		switch e.Name {
		case "postgres":
			q = `SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema = current_schema()
	AND column_name IN ('ext_version', 'extension')
UNION ALL SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'extension%'
UNION ALL SELECT conname FROM pg_constraint WHERE connamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())
	AND (conname LIKE '%extension%' OR conname LIKE '%ext\_version%')
UNION ALL SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND (indexname LIKE '%extension%' OR indexname LIKE '%ext\_version%')`
		case "sqlserver":
			q = `SELECT table_name + '.' + column_name FROM information_schema.columns WHERE column_name IN ('ext_version', 'extension')
UNION ALL SELECT name FROM sys.objects WHERE is_ms_shipped = 0 AND (name LIKE 'extension%' OR name LIKE '%[_]extension[_]group%' OR name LIKE '%ext[_]version%')
UNION ALL SELECT name FROM sys.indexes WHERE name LIKE '%extension[_]group%' OR name LIKE '%ext[_]version%'`
		default:
			q = `SELECT m.name || '.' || p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table'
	AND p.name IN ('ext_version', 'extension')
UNION ALL SELECT name FROM sqlite_master WHERE name LIKE '%extension%' OR name LIKE '%ext\_version%' ESCAPE '\'`
		}
		oldNames := func() (left []string) {
			inTx(t, s, func(tx *store.Tx) (err error) {
				left, err = store.QueryStrings(ctx, tx, q)
				return err
			})
			return left
		}
		if left := oldNames(); !slices.Contains(left, "builds.ext_version") || !slices.Contains(left, "grants.extension") ||
			!slices.Contains(left, "ix_extension_group_members_tenant") || e.Name != "sqlite" && !slices.Contains(left, "uq_extension_groups_name") {
			t.Fatalf("the old names before: %v", left)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}

		// grants, the group, its members and the grant on it
		a, err := s.GetTenantAuth(ctx, tid)
		if err != nil {
			t.Fatal(err)
		}
		grants := map[string]store.Grant{}
		for _, g := range a.Grants {
			grants[g.ID] = g
		}
		if g := grants[id("1")]; len(grants) != 3 || g.Package != "acl" || g.ChannelID != cid || g.Value != "dev" || !slices.Equal(g.Verbs, []string{"install"}) {
			t.Fatalf("grants: %+v", a.Grants)
		}
		if g := grants[id("2")]; g.Package != "tresor" || g.PublisherID != id("p") || grants[id("3")].Package != "" || grants[id("3")].ChannelID != "" {
			t.Fatalf("grants: %+v", a.Grants)
		}
		if len(a.GroupGrants) != 1 || a.GroupGrants[0].PackageGroupID != id("g") || a.GroupGrants[0].PackageGroupName != "platform" ||
			a.GroupGrants[0].Package != "" || a.GroupGrants[0].ChannelID != cid || !a.GroupGrants[0].Members["acl"] || !a.GroupGrants[0].Members["tresor"] {
			t.Fatalf("group grants: %+v", a.GroupGrants)
		}
		g, err := s.GetPackageGroup(ctx, tid, "platform")
		if err != nil || g.ID != id("g") || g.Description != "hugr" || g.Version != 3 || g.Members != 2 || g.Grants != 1 {
			t.Fatalf("the group: %+v %v", g, err)
		}
		ms, err := s.GroupMembers(ctx, g.ID)
		if err != nil || len(ms) != 2 || ms[0].Name != "acl" || ms[1].Name != "tresor" {
			t.Fatalf("members: %+v %v", ms, err)
		}
		if gs, err := s.ListPackageGroups(ctx, tid, "tresor"); err != nil || len(gs) != 1 || gs[0].Name != "platform" {
			t.Fatalf("groups holding tresor: %+v %v", gs, err)
		}

		// the build, its release, the purged slot, the upstream's versions
		var b store.Build
		var r store.Release
		var purged []store.Candidate
		var u store.Upstream
		inTx(t, s, func(tx *store.Tx) error {
			if b, err = tx.GetBuild(ctx, id("b")); err != nil {
				return err
			}
			if r, err = tx.GetRelease(ctx, cid, id("r")); err != nil {
				return err
			}
			if purged, err = tx.SlotCandidates(ctx, cid, "tenant", "acl", "0.9", "linux_amd64"); err != nil {
				return err
			}
			u, err = tx.GetUpstream(ctx, tid, "core")
			return err
		})
		if b.PackageVersion != "1.2.3" || b.Name != "acl" || b.BodyHash != hash("1") || r.PackageVersion != "1.2.3" || r.BuildID != id("b") {
			t.Fatalf("the build %+v, the release %+v", b, r)
		}
		if len(purged) != 1 || purged[0].State != store.ReleasePurged || purged[0].Seq != 7 || purged[0].PackageVersion != "0.9" {
			t.Fatalf("the purged slot: %+v", purged)
		}
		if len(u.Entries) != 1 || u.Entries[0].Name != "json" || !slices.Equal(u.Entries[0].Versions, []string{"1.0"}) {
			t.Fatalf("the upstream's entries: %+v", u.Entries)
		}

		// statistics and usage
		f := store.StatsFilter{From: "2026-10-01", To: "2026-10-31"}
		ds, err := s.DownloadRows(ctx, tid, f, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		var count, installers int64
		for _, d := range ds {
			if d.PackageVersion != "1.2.3" || d.Name != "acl" || d.Day != "2026-10-11" {
				t.Fatalf("downloads: %+v", ds)
			}
			count, installers = count+d.Count, installers+d.Installers
		}
		if count != 3 || installers != 2 {
			t.Fatalf("downloads: %+v", ds)
		}
		us, err := s.UsageRows(ctx, tid, f, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, x := range us {
			got[x.Name+"@"+x.PackageVersion+" "+x.Event] += x.Value + x.Users
		}
		if len(got) != 3 || got["acl@1.2.3 load"] != 5 || got["acl@1.2.3 "] != 4 || got["tresor@0.4 load"] != 6 {
			t.Fatalf("usage: %+v", us)
		}

		if left := oldNames(); len(left) != 0 {
			t.Fatalf("old names left: %v", left)
		}
	})
}
