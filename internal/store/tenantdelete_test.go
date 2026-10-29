package store_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// fullTenant creates a tenant with a row in every table the reaper deletes from, and returns it
// with its channel. n makes its ids and fingerprints distinct from another fullTenant's.
func fullTenant(t *testing.T, s *store.Store, name, n string) (store.Tenant, store.Channel) {
	t.Helper()
	tn := store.Tenant{Name: name, DisplayName: "The " + name, HolderID: "org-" + name}
	ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
	inTx(t, s, func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		return tx.CreateChannel(ctx, &ch)
	})
	fp := func(c string) string { return "sha256:" + strings.Repeat(n, 63) + c }
	key := addKey(t, s, ch, fp("a"), store.KeyActive)
	att := store.Key{TenantID: tn.ID, Fingerprint: fp("b"), SignerRef: "file:att", PublicKey: []byte{1}, State: store.KeyActive}
	inTx(t, s, func(tx *store.Tx) error { return tx.InsertAttestationKey(ctx, &att, "os:1:t") })
	now := store.TimeArg(s, time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC))
	id := func(k string) string { return n + "0000000-0000-0000-0000-00000000000" + k }
	hash := strings.Repeat(n, 64)
	sig := make([]byte, 256)
	tid, cid := tn.ID, ch.ID
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO duckdb_versions (id, name, kind, created_at) VALUES (?, ?, 'release', ?)", []any{id("v"), "v2.0." + n, now}},
		{"INSERT INTO channel_duckdb_versions (channel_id, duckdb_version_id) VALUES (?, ?)", []any{cid, id("v")}},
		{"INSERT INTO key_events (id, key_id, to_state, actor, forced, at) VALUES (?, ?, 'active', 'os:1:t', ?, ?)", []any{id("e"), key.ID, false, now}},
		{"INSERT INTO builds (id, tenant_id, source, name, package_version, platform, abi, duckdb_version, slot, body_hash, origin, created_at, created_by, checked) " +
			"VALUES (?, ?, 'tenant', 'acl', '1.0', 'linux_amd64', 'cpp', 'v2.0.0', 'v2.0.0', ?, 'admin', ?, 'os:1:t', ?)", []any{id("b"), tid, hash, now, true}},
		{"INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state, visibility, seq, created_at, " +
			"created_by, state_changed_at, state_changed_by, version, origin) VALUES (?, ?, ?, ?, 'acl', '1.0', 'linux_amd64', 'v2.0.0', 'active', " +
			"'public', 1, ?, 'os:1:t', ?, 'os:1:t', 1, 'admin')", []any{id("r"), tid, cid, id("b"), now, now}},
		{"INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at) VALUES (?, ?, ?, 'pem', 'upstream', ?, ?)",
			[]any{tid, id("b"), "sha256:" + hash, id("u"), now}},
		{"INSERT INTO release_signatures (release_id, channel_id, key_id, signature, created_at) VALUES (?, ?, ?, ?, ?)", []any{id("r"), cid, key.ID, sig, now}},
		{"UPDATE channels SET serving_key_id = ? WHERE id = ?", []any{key.ID, cid}},
		{"INSERT INTO purged_slots (channel_id, source, name, package_version, platform, slot, abi, body_hash, seq, release_id, purged_at) " +
			"VALUES (?, 'tenant', 'old', '0.1', 'linux_amd64', 'v2.0.0', 'cpp', ?, 1, ?, ?)", []any{cid, hash, id("q"), now}},
		{"INSERT INTO blocks (tenant_id, body_hash, reason, created_at, created_by) VALUES (?, ?, 'CVE', ?, 'os:1:t')", []any{tid, hash, now}},
		{"INSERT INTO shadows (tenant_id, name, created_at, created_by) VALUES (?, 'json', ?, 'os:1:t')", []any{tid, now}},
		{"INSERT INTO removed_distributor_publishers (tenant_id, publisher_id, name, distributor, removed_at, removed_by) VALUES (?, ?, 'dist', 'enterest', ?, 'os:1:t')",
			[]any{tid, id("d"), now}},
		{"INSERT INTO withdrawn_versions (tenant_id, package_type, source, name, package_version, reason, withdrawn_by, withdrawn_at) " +
			"VALUES (?, 'duckdb_extension', ?, 'geo', '1.0', 'CVE', 'os:1:t', ?)", []any{tid, "publisher:" + id("d"), now}},
		{"INSERT INTO entry_meta (tenant_id, package_type, source, name, summary, icon_sha256, icon_width, icon_height, icon_bytes, set_by, set_at) " +
			"VALUES (?, 'duckdb_extension', 'tenant', 'acl', 'Access control', ?, 64, 64, 100, 'os:1:t', ?)", []any{tid, hash, now}},
		{"INSERT INTO channel_sources (channel_id, name, source, set_by, set_at) VALUES (?, 'json', 'tenant', 'os:1:t', ?)", []any{cid, now}},
		{"INSERT INTO channel_follows (channel_id, source, set_by, set_at) VALUES (?, 'publisher:x', 'os:1:t', ?)", []any{cid, now}},
		{"INSERT INTO follow_skips (channel_id, source, name, reason) VALUES (?, 'publisher:x', 'json', 'reserved')", []any{cid}},
		{"INSERT INTO bundle_imports (id, tenant_id, manifest_id, manifest_created_at, manifest_sha256, exporter, bundle_key, dry_run, " +
			"state, counts, started_at, actor) VALUES (?, ?, ?, ?, ?, '{}', ?, ?, 'done', '{}', ?, 'os:1:t')",
			[]any{id("m"), tid, id("n"), now, hash, fp("d"), false, now}},
		{"INSERT INTO upstreams (id, tenant_id, name, kind, channel_id, mode, visibility, state, version, request_dry_run, created_at, created_by) " +
			"VALUES (?, ?, 'core', 'duckdb-core', ?, 'mirror', 'public', 'active', 1, ?, ?, 'os:1:t')", []any{id("u"), tid, cid, false, now}},
		{"INSERT INTO upstream_keys (upstream_id, fingerprint) VALUES (?, ?)", []any{id("u"), fp("c")}},
		{"INSERT INTO upstream_platforms (upstream_id, platform) VALUES (?, 'linux_amd64')", []any{id("u")}},
		{"INSERT INTO upstream_entries (upstream_id, name, allow_reserved) VALUES (?, 'json', ?)", []any{id("u"), false}},
		{"INSERT INTO upstream_versions (upstream_id, name, package_version) VALUES (?, 'json', '1.0')", []any{id("u")}},
		{"INSERT INTO upstream_cells (upstream_id, duckdb_version, platform, name, outcome, fetched_at) VALUES (?, 'v2.0.0', 'linux_amd64', 'json', 'missing', ?)",
			[]any{id("u"), now}},
		{"INSERT INTO upstream_asks (upstream_id, duckdb_version, platform, name, asked_at) VALUES (?, 'v2.0.0', 'linux_amd64', 'json', ?)",
			[]any{id("u"), now}},
		{"INSERT INTO licenses (tenant_id, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at, " +
			"grace_until, source, upstream_id, visibility, actor, updated_at, version, entry_source) VALUES (?, 'json', 'l1', 'https://i.example', 'acme', '', " +
			"'pro', 't', ?, 1, 2, 3, 'upstream', ?, 'private', 'system:licenses', ?, 1, ?)", []any{tid, fp("l"), id("u"), now, "upstream:" + id("u")}},
		{"INSERT INTO license_syncs (upstream_id, tenant_id, last_run_at, last_result) VALUES (?, ?, ?, '{}')", []any{id("u"), tid, now}},
		{"INSERT INTO usage_forwards (upstream_id, tenant_id, last_run_at, last_result) VALUES (?, ?, ?, '{}')", []any{id("u"), tid, now}},
		{"INSERT INTO usage_received (tenant_id, channel_id, sender, day, name, package_version, event, counter, users, value) " +
			"VALUES (?, ?, 'x', '2026-10-11', 'acl', '1.0', 'load', '', 0, 4)", []any{tid, cid}},
		{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, 'ci', ?, 'os:1:t')", []any{id("p"), tid, now}},
		{"INSERT INTO publisher_keys (id, tenant_id, publisher_id, type, public_key, fingerprint, title, created_at, created_by) " +
			"VALUES (?, ?, ?, 'ssh-ed25519', 'ssh-ed25519 AAAA', ?, '', ?, 'os:1:t')", []any{id("k"), tid, id("p"), "SHA256:" + hash[:43], now}},
		{"INSERT INTO issuers (id, tenant_id, name, url, algorithms, required_claims, roles_claim, groups_claim, client_claim, max_token_lifetime, " +
			"created_at, created_by) VALUES (?, ?, 'corp', 'https://idp.example', '[]', '{}', '', '', '', 3600, ?, 'os:1:t')", []any{id("i"), tid, now}},
		{"INSERT INTO issuer_console_clients (issuer_id, client_id, scopes, audience_parameter, audience, created_at, created_by) " +
			"VALUES (?, 'kista', '[]', '', 'api://x', ?, 'os:1:t')", []any{id("i"), now}},
		{"INSERT INTO grants (id, tenant_id, issuer_id, kind, value, package_type, verbs, created_at, created_by) VALUES (?, ?, ?, 'role', 'admin', '*', 'admin', ?, 'os:1:t')",
			[]any{id("a"), tid, id("i"), now}},
		{"INSERT INTO tenant_audiences (audience, tenant_id, created_at, created_by) VALUES (?, ?, ?, 'os:1:t')", []any{"api://" + name, tid, now}},
		{"INSERT INTO package_groups (id, tenant_id, name, description, version, created_at, created_by) VALUES (?, ?, 'g', '', 1, ?, 'os:1:t')",
			[]any{id("x"), tid, now}},
		{"INSERT INTO package_group_members (group_id, tenant_id, package, added_at, added_by) VALUES (?, ?, 'acl', ?, 'os:1:t')", []any{id("x"), tid, now}},
		{"INSERT INTO package_group_grants (id, tenant_id, group_id, issuer_id, kind, value, verbs, created_at, created_by) " +
			"VALUES (?, ?, ?, ?, 'role', 'dev', 'install', ?, 'os:1:t')", []any{id("y"), tid, id("x"), id("i"), now}},
		{"INSERT INTO attachments (id, tenant_id, body_hash, kind, media_type, title, size, content_hash, summary, origin, attached_by, visibility, " +
			"feedable, version, created_at, issuer, issuer_id, issued_at) VALUES (?, ?, ?, 'notes', 'text/markdown', 'n', 1, ?, '{}', 'tenant', " +
			"'os:1:t', 'administrators', ?, 1, ?, '', ?, ?)", []any{id("d"), tid, hash, hash, false, now, id("d"), now}},
		{"INSERT INTO statement_counters (tenant_id, seq) VALUES (?, 1)", []any{tid}},
		{"INSERT INTO statements (tenant_id, seq, issuer_id, body_hash, withdrawn, statement, signature, issued_at) VALUES (?, 1, ?, ?, ?, '{}', 'x', ?)",
			[]any{tid, id("d"), hash, false, now}},
		{"INSERT INTO feeds (id, tenant_id, name, url, kinds, readers, offer, state, version, status, created_at, created_by) " +
			"VALUES (?, ?, 'reviews', 'https://src.example/api/v1/tenants/x', '[]', ?, 'channels', 'active', 1, 'new', ?, 'os:1:t')",
			[]any{id("f"), tid, false, now}},
		{"INSERT INTO feed_keys (feed_id, fingerprint) VALUES (?, ?)", []any{id("f"), fp("d")}},
		{"INSERT INTO feed_channels (feed_id, channel_id) VALUES (?, ?)", []any{id("f"), cid}},
		{"INSERT INTO feed_cursors (feed_id, body_hash, seq) VALUES (?, ?, 1)", []any{id("f"), hash}},
		{"INSERT INTO feed_refusals (feed_id, body_hash, seq, reason, statement, signature, refused_at) VALUES (?, ?, 2, 'kind', '{}', 'x', ?)",
			[]any{id("f"), hash, now}},
		{"INSERT INTO bundle_uploads (id, tenant_id, size, chunk_size, options, state, attempts, created_by, created_at, updated_at) " +
			"VALUES (?, ?, 10, 10, '{}', 'uploading', 0, 'os:1:t', ?, ?)", []any{id("u"), tid, now, now}},
		{"INSERT INTO bundle_upload_chunks (upload_id, n, object_key, size, sha256, received_at) VALUES (?, 0, 'uploads/x', 10, ?, ?)",
			[]any{id("u"), hash, now}},
		{"INSERT INTO feed_kept (tenant_id, issuer, issuer_id, body_hash, seq, statement, signature, kept_at) VALUES (?, 'https://src.example/api/v1/tenants/x', ?, ?, 3, '{}', 'x', ?)",
			[]any{tid, id("k"), hash, now}},
		{"INSERT INTO download_counts (tenant_id, channel_id, name, package_version, platform, duckdb_version, day, authenticated, count) " +
			"VALUES (?, ?, 'acl', '1.0', 'linux_amd64', 'v2.0.0', '2026-10-11', ?, 3)", []any{tid, cid, false}},
		{"INSERT INTO download_installers (tenant_id, channel_id, name, package_version, platform, day, installers) " +
			"VALUES (?, ?, 'acl', '1.0', 'linux_amd64', '2026-10-11', 2)", []any{tid, cid}},
		{"INSERT INTO usage_counts (tenant_id, channel_id, name, package_version, event, counter, status, day, authenticated, value) " +
			"VALUES (?, ?, 'acl', '1.0', 'load', '', 'valid', '2026-10-11', ?, 3)", []any{tid, cid, true}},
		{"INSERT INTO usage_users (tenant_id, channel_id, name, package_version, day, users) VALUES (?, ?, 'acl', '1.0', '2026-10-11', 2)", []any{tid, cid}},
	} {
		if err := store.ExecRaw(ctx, s, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	return tn, ch
}

// rowsOf counts a tenant's rows per reaped table (purged_slots through its channel).
func rowsOf(t *testing.T, s *store.Store, tenantID, channelID string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, table := range store.ReapTables() {
		where, arg := "tenant_id = ?", tenantID
		switch table {
		case "feed_cursors", "feed_channels", "feed_keys", "feed_refusals":
			where = "feed_id IN (SELECT id FROM feeds WHERE tenant_id = ?)"
		case "bundle_upload_chunks":
			where = "upload_id IN (SELECT id FROM bundle_uploads WHERE tenant_id = ?)"
		case "upstream_asks", "upstream_cells", "upstream_versions", "upstream_entries", "upstream_platforms", "upstream_keys":
			where = "upstream_id IN (SELECT id FROM upstreams WHERE tenant_id = ?)"
		case "issuer_console_clients":
			where = "issuer_id IN (SELECT id FROM issuers WHERE tenant_id = ?)"
		case "release_signatures", "channel_duckdb_versions", "purged_slots", "channel_sources", "channel_follows", "follow_skips":
			where, arg = "channel_id = ?", channelID
		case "key_events":
			where = "key_id IN (SELECT id FROM channel_keys WHERE tenant_id = ?)"
		}
		n, err := store.CountRaw(ctx, s, table, where, arg)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// Spec 0019 on every dialect: a deleting tenant is closed, the reaper removes every row of it and
// nothing of another tenant, resumably, and the tombstone keeps the name.
func TestReapTenant(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		gone, goneCh := fullTenant(t, s, "gone", "1")
		kept, keptCh := fullTenant(t, s, "kept", "2")
		before := rowsOf(t, s, kept.ID, keptCh.ID)
		for table, n := range rowsOf(t, s, gone.ID, goneCh.ID) {
			if n == 0 {
				t.Fatalf("the fixture has no row in %s", table)
			}
		}

		// only a suspended tenant starts deleting; the transition records the keys and channels
		start := func(tn *store.Tenant) (store.Deletion, error) {
			var d store.Deletion
			err := s.InTx(ctx, "", func(tx *store.Tx) error {
				cur, err := tx.GetTenantAnyForUpdate(ctx, tn.Name)
				if err != nil {
					return err
				}
				*tn = cur
				d, err = tx.StartDeletion(ctx, tn, "os:1:t")
				return err
			})
			return d, err
		}
		if _, err := start(&gone); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("an active tenant: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantForUpdate(ctx, "gone")
			if err != nil {
				return err
			}
			if err := tx.SetTenantState(ctx, &cur, store.TenantSuspended); err != nil {
				return err
			}
			if cur.SuspendedAt.IsZero() {
				t.Error("suspending sets suspended_at")
			}
			return nil
		})
		ups, _ := s.DueUpstreams(ctx, s.Now(), true)
		feeds, _ := s.DueFeeds(ctx, s.Now(), true)
		if len(ups) != 2 || len(feeds) != 2 {
			t.Fatalf("due before: %v %v", ups, feeds)
		}
		d, err := start(&gone)
		if err != nil {
			t.Fatal(err)
		}
		// the runners skip it; no statistics and no Build are written for it
		if ups, err := s.DueUpstreams(ctx, s.Now(), true); err != nil || len(ups) != 1 || strings.HasPrefix(ups[0], "1") {
			t.Fatalf("due upstreams: %v %v", ups, err)
		}
		if feeds, err := s.DueFeeds(ctx, s.Now(), true); err != nil || len(feeds) != 1 || strings.HasPrefix(feeds[0], "1") {
			t.Fatalf("due feeds: %v %v", feeds, err)
		}
		dk := store.DownloadKey{TenantID: gone.ID, ChannelID: goneCh.ID, Name: "acl", PackageVersion: "9.9", Platform: "linux_amd64",
			DuckDBVersion: "v2.0.0", Day: "2026-10-12"}
		if done, err := s.AddDownloadCounts(ctx, map[store.DownloadKey]int64{dk: 1}); err != nil || len(done) != 1 {
			t.Fatalf("counts: %v %v", done, err)
		}
		if n, _ := store.CountRaw(ctx, s, "download_counts", "tenant_id = ? AND package_version = '9.9'", gone.ID); n != 0 {
			t.Error("a download count written for a deleting tenant")
		}
		ik := store.InstallerKey{TenantID: gone.ID, ChannelID: goneCh.ID, Name: "acl", PackageVersion: "9.9", Platform: "linux_amd64", Day: "2026-10-12"}
		if err := s.PutDaily(ctx, "2026-10-12", store.Daily{Installers: map[store.InstallerKey]int64{ik: 1},
			Users: map[store.UsageUserKey]int64{{TenantID: gone.ID, ChannelID: goneCh.ID, Name: "acl", PackageVersion: "9.9", Day: "2026-10-12"}: 1}}); err != nil {
			t.Fatal(err)
		}
		if n, _ := store.CountRaw(ctx, s, "download_installers", "tenant_id = ? AND day = '2026-10-12'", gone.ID); n != 0 {
			t.Error("installers written for a deleting tenant")
		}
		if n, _ := store.CountRaw(ctx, s, "usage_users", "tenant_id = ? AND day = '2026-10-12'", gone.ID); n != 0 {
			t.Error("usage users written for a deleting tenant")
		}
		uk := store.UsageKey{TenantID: gone.ID, ChannelID: goneCh.ID, Name: "acl", PackageVersion: "9.9", Event: "load", Status: "valid", Day: "2026-10-12"}
		if done, err := s.AddUsageCounts(ctx, map[store.UsageKey]int64{uk: 1}); err != nil || len(done) != 1 {
			t.Fatalf("usage counts: %v %v", done, err)
		}
		if n, _ := store.CountRaw(ctx, s, "usage_counts", "tenant_id = ? AND package_version = '9.9'", gone.ID); n != 0 {
			t.Error("a usage count written for a deleting tenant")
		}
		err = s.InTx(ctx, "", func(tx *store.Tx) error {
			b := store.Build{TenantID: gone.ID, Name: "acl", PackageVersion: "2.0", Platform: "linux_amd64", ABI: "cpp", DuckDBVersion: "v2.0.0",
				BodyHash: strings.Repeat("9", 64), Origin: "admin", CreatedBy: "os:1:t"}
			return tx.FindOrInsertBuild(ctx, &b)
		})
		if !errors.Is(err, store.ErrDeleting) {
			t.Fatalf("a Build for a deleting tenant: %v", err)
		}
		if len(d.Keys) != 2 || d.Keys[0].Kind != "channel" || d.Keys[1].Kind != "attestation" || d.Keys[0].SignerRef == "" ||
			!slices.Equal(d.ChannelIDs, []string{goneCh.ID}) {
			t.Fatalf("deletion record: %+v", d)
		}
		if got, err := s.GetDeletion(ctx, gone.ID); err != nil || len(got.Keys) != 2 || got.StartedBy != "os:1:t" {
			t.Fatalf("stored record: %+v %v", got, err)
		}

		// closed: lookups by name answer like a missing tenant, writes are refused, resume too
		if _, err := s.GetTenant(ctx, "gone"); !errors.Is(err, store.ErrDeleting) || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetTenant: %v", err)
		}
		if _, err := s.GetChannel(ctx, "gone", "prod"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetChannel: %v", err)
		}
		if _, err := s.GetServeChannel(ctx, "gone", "prod"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetServeChannel: %v", err)
		}
		if err := s.InTenantTx(ctx, gone.ID, nil, func(*store.Tx) error { return nil }); !errors.Is(err, store.ErrDeleting) {
			t.Fatalf("InTenantTx: %v", err)
		}
		err = s.InTx(ctx, "", func(tx *store.Tx) error {
			_, err := tx.ChannelByID(ctx, goneCh.ID)
			return err
		})
		if !errors.Is(err, store.ErrDeleting) {
			t.Fatalf("ChannelByID: %v", err)
		}
		g := gone
		g.State = store.TenantSuspended
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetTenantState(ctx, &g, store.TenantActive) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("resuming a deleting tenant: %v", err)
		}
		if ids, err := s.DueDeletions(ctx, s.Now().Add(time.Hour)); err != nil || !slices.Equal(ids, []string{gone.ID}) {
			t.Fatalf("due: %v %v", ids, err)
		}

		// interrupted after two batches, then run again: it completes
		stop := errors.New("stop")
		batches := 0
		_, err = s.ReapTenant(ctx, gone, d, func(string, int64) error {
			if batches++; batches == 2 {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) {
			t.Fatalf("interrupted: %v", err)
		}
		n, err := s.ReapTenant(ctx, gone, d, nil)
		if err != nil || n == 0 {
			t.Fatalf("reap: %d %v", n, err)
		}
		for table, n := range rowsOf(t, s, gone.ID, goneCh.ID) {
			if n != 0 {
				t.Errorf("%s: %d rows left", table, n)
			}
		}
		if after := rowsOf(t, s, kept.ID, keptCh.ID); !mapsEqual(after, before) {
			t.Errorf("the other tenant changed: %v, was %v", after, before)
		}
		tomb, err := s.GetTenantAny(ctx, "gone")
		if err != nil || tomb.Phase() != store.TenantDeleted || tomb.DisplayName != "" || tomb.HolderID != "" { // spec 0028: no holder kept
			t.Fatalf("tombstone: %+v %v", tomb, err)
		}
		if _, err := s.GetTenant(ctx, "gone"); !errors.Is(err, store.ErrDeleted) {
			t.Fatalf("a tombstone by name: %v", err)
		}
		for table, where := range map[string]string{"tenant_audiences": "tenant_id = ?", "key_fingerprints": "key_id IN (?, ?)"} {
			args := []any{gone.ID}
			if table == "key_fingerprints" {
				args = []any{d.Keys[0].ID, d.Keys[1].ID}
			}
			if n, err := store.CountRaw(ctx, s, table, where, args...); err != nil || n == 0 {
				t.Errorf("%s kept: %d %v", table, n, err)
			}
		}
		if n, err := store.CountRaw(ctx, s, "events", "tenant_id = ? AND kind = 'tenant.purge'", gone.ID); err != nil || n != 1 {
			t.Errorf("tenant.purge: %d %v", n, err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			tn := store.Tenant{Name: "gone"}
			return tx.CreateTenant(ctx, &tn)
		}); !errors.Is(err, store.ErrExists) {
			t.Errorf("a tombstone's name reused: %v", err)
		}

		// a row that appears under the tombstone is found by the late sweep
		if err := store.ExecRaw(ctx, s, "INSERT INTO download_counts (tenant_id, channel_id, name, package_version, platform, duckdb_version, day, "+
			"authenticated, count) VALUES (?, ?, 'acl', '1.0', 'linux_amd64', 'v2.0.0', '2026-10-12', ?, 1)", gone.ID, goneCh.ID, false); err != nil {
			t.Fatal(err)
		}
		if ids, err := s.LateSweeps(ctx, s.Now().Add(-time.Hour), s.Now().Add(time.Hour)); err != nil || !slices.Equal(ids, []string{gone.ID}) {
			t.Fatalf("late sweeps: %v %v", ids, err)
		}
		if n, err := s.SweepTombstone(ctx, tomb, d); err != nil || n != 1 {
			t.Fatalf("sweep: %d %v", n, err)
		}
		if n, err := store.CountRaw(ctx, s, "events", "tenant_id = ? AND kind = 'tenant.purge'", gone.ID); err != nil || n != 2 {
			t.Errorf("a late tenant.purge: %d %v", n, err)
		}
	})
}

func mapsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Every table of the schema is reaped, kept or the server's: a new table must be classified.
func TestReapCompleteness(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tables, err := store.Tables(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		known := map[string]bool{}
		for _, list := range [][]string{store.ReapTables(), store.KeptTables, store.GlobalTables} {
			for _, x := range list {
				known[x] = true
			}
		}
		for _, x := range tables {
			if !known[x] && !strings.HasPrefix(x, "sqlite_") {
				t.Errorf("table %s is neither reaped, kept nor global (spec 0019)", x)
			}
		}
	})
}

// A batch deletes at most ReapBatch rows, even with composite keys (SQL Server's parameters).
func TestReapBatch(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		for i := range store.ReapBatch + 5 {
			if err := store.ExecRaw(ctx, s, "INSERT INTO download_counts (tenant_id, channel_id, name, package_version, platform, duckdb_version, day, "+
				"authenticated, count) VALUES (?, ?, 'acl', ?, 'linux_amd64', 'v2.0.0', '2026-10-11', ?, 1)", tn.ID, ch.ID, "1."+strconv.Itoa(i), false); err != nil {
				t.Fatal(err)
			}
		}
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantForUpdate(ctx, tn.Name)
			if err != nil {
				return err
			}
			return tx.SetTenantState(ctx, &cur, store.TenantSuspended)
		})
		var d store.Deletion
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantAnyForUpdate(ctx, tn.Name)
			if err != nil {
				return err
			}
			tn = cur
			d, err = tx.StartDeletion(ctx, &tn, "os:1:t")
			return err
		})
		var sizes []int64
		if _, err := s.ReapTenant(ctx, tn, d, func(table string, n int64) error {
			if table == "download_counts" {
				sizes = append(sizes, n)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sizes, []int64{store.ReapBatch, 5}) {
			t.Fatalf("batches: %v", sizes)
		}
	})
}

// Migration 0019 starts the minimum suspension of a tenant already suspended at the migration, in
// each dialect's own time form.
func TestMigrationSuspendedAt(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 18); err != nil {
			t.Fatal(err)
		}
		old := store.TimeArg(s, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		for _, x := range [][2]string{{"a", store.TenantActive}, {"s", store.TenantSuspended}} {
			if err := store.ExecRaw(ctx, s, "INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, ?, '', ?, ?, 1)",
				x[0]+"0000000-0000-0000-0000-000000000000", "t-"+x[0], x[1], old); err != nil {
				t.Fatal(err)
			}
		}
		before := time.Now().Add(-time.Minute)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		sus, err := s.GetTenant(ctx, "t-s")
		if err != nil || sus.SuspendedAt.Before(before) || sus.SuspendedAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("suspended: %+v %v", sus, err)
		}
		if act, err := s.GetTenant(ctx, "t-a"); err != nil || !act.SuspendedAt.IsZero() {
			t.Fatalf("active: %+v %v", act, err)
		}
	})
}

// A write holding the tenant open delays the deletion's transition, and none is admitted after it.
func TestWritableTenantSerialises(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantForUpdate(ctx, tn.Name)
			if err != nil {
				return err
			}
			return tx.SetTenantState(ctx, &cur, store.TenantSuspended)
		})
		holding, release := make(chan struct{}), make(chan struct{})
		writer := make(chan error, 1)
		go func() {
			writer <- s.InTenantTx(ctx, tn.ID, nil, func(tx *store.Tx) error {
				select {
				case <-holding:
				default:
					close(holding)
				}
				<-release
				return nil
			})
		}()
		<-holding
		deleted := make(chan error, 1)
		go func() {
			deleted <- s.InTx(ctx, "", func(tx *store.Tx) error {
				cur, err := tx.GetTenantAnyForUpdate(ctx, tn.Name)
				if err != nil {
					return err
				}
				_, err = tx.StartDeletion(ctx, &cur, "os:1:t")
				return err
			})
		}()
		select {
		case err := <-deleted:
			t.Fatalf("the deletion did not wait for the writer: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		if err := <-writer; err != nil {
			t.Fatalf("the writer: %v", err)
		}
		if err := <-deleted; err != nil {
			t.Fatalf("the deletion: %v", err)
		}
		if err := s.InTenantTx(ctx, tn.ID, nil, func(*store.Tx) error { return nil }); !errors.Is(err, store.ErrDeleting) {
			t.Fatalf("a write after the deletion: %v", err)
		}
	})
}

// A failed pass counts failures in a row and says when the class changes; a pause ends the run of
// failures; a deletion already finished records no failure.
func TestDeletionFailed(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantForUpdate(ctx, tn.Name)
			if err != nil {
				return err
			}
			return tx.SetTenantState(ctx, &cur, store.TenantSuspended)
		})
		var d store.Deletion
		inTx(t, s, func(tx *store.Tx) error {
			cur, err := tx.GetTenantAnyForUpdate(ctx, tn.Name)
			if err != nil {
				return err
			}
			tn = cur
			d, err = tx.StartDeletion(ctx, &tn, "os:1:t")
			return err
		})
		var waits []int64
		backoff := func(n int64) time.Duration { waits = append(waits, n); return time.Minute }
		for i, class := range []string{"error", "error", "lease"} {
			changed, err := s.DeletionFailed(ctx, tn.ID, class, backoff, "builds", 1)
			if err != nil || changed != (i != 1) {
				t.Fatalf("%d: %v %v", i, changed, err)
			}
		}
		if err := s.DeletionPaused(ctx, tn.ID, s.Now(), "", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeletionFailed(ctx, tn.ID, "lease", backoff, "", 0); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(waits, []int64{1, 2, 3, 1}) {
			t.Fatalf("failures in a row: %v", waits)
		}
		got, err := s.GetDeletion(ctx, tn.ID)
		if err != nil || got.Passes != 4 || got.Table != "builds" || got.DeletedRows != 3 || got.LastError != "lease" {
			t.Fatalf("record: %+v %v", got, err)
		}
		if _, err := s.ReapTenant(ctx, tn, d, nil); err != nil {
			t.Fatal(err)
		}
		if changed, err := s.DeletionFailed(ctx, tn.ID, "interrupted", backoff, "", 0); err != nil || changed {
			t.Fatalf("a failure after the tombstone: %v %v", changed, err)
		}
		if got, _ := s.GetDeletion(ctx, tn.ID); got.LastError != "" || got.FinishedAt.IsZero() {
			t.Fatalf("finished record changed: %+v", got)
		}
	})
}
