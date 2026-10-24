-- +min_reader 34
-- +foreign_keys_off
-- spec 0033 phase A1: a build belongs to its source, and a slot is a source's: two sources may
-- release the same version of a name in one channel. An older replica would refuse the second, or
-- read another source's release as the slot's, so it no longer runs here. SQLite rebuilds the
-- tables to change their unique keys, with foreign keys off and checked before the commit.
CREATE TABLE builds_new (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	source           TEXT NOT NULL,
	name             TEXT NOT NULL,
	ext_version      TEXT NOT NULL,
	platform         TEXT NOT NULL,
	abi              TEXT NOT NULL CHECK (abi IN ('cpp', 'c_struct', 'c_struct_unstable')),
	duckdb_version   TEXT,
	c_api_major      INTEGER,
	c_api_minor      INTEGER,
	c_api_patch      INTEGER,
	body_hash        TEXT NOT NULL,
	origin           TEXT NOT NULL CHECK (origin IN ('admin', 'publication', 'upstream')),
	origin_signature BLOB,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	checked          INTEGER NOT NULL DEFAULT 0,
	used_at          TEXT,
	CONSTRAINT ux_builds_source UNIQUE (tenant_id, source, name, body_hash),
	UNIQUE (id, tenant_id)
)
-- +statement
-- a build's source is its first release's (they all agree but where spec 0031's same-body rule
-- gave a slot's release to another source); a build without releases (purged, the collector not past
-- it yet) gets it from who made it, as migration 0032 read releases' actors
INSERT INTO builds_new (id, tenant_id, source, name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor,
	c_api_patch, body_hash, origin, origin_signature, created_at, created_by, checked, used_at)
SELECT id, tenant_id, coalesce((SELECT r.source FROM releases r WHERE r.build_id = builds.id ORDER BY r.created_at, r.id LIMIT 1),
		CASE origin WHEN 'upstream' THEN 'upstream:' || (SELECT min(u.id) FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
			WHERE u.tenant_id = builds.tenant_id AND builds.created_by = 'system:upstream:' || t.name || '/' || u.name)
		WHEN 'publication' THEN 'publisher:' || (SELECT min(p.id) FROM publishers p JOIN tenants t ON t.id = p.tenant_id
			WHERE p.tenant_id = builds.tenant_id AND (builds.created_by = 'publisher:' || t.name || '/' || p.name
				OR builds.created_by LIKE 'publisher:' || t.name || '/' || p.name || ',%')) END, 'tenant'),
	name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor, c_api_patch, body_hash, origin, origin_signature,
	created_at, created_by, checked, used_at
FROM builds
-- +statement
-- a build whose releases name another source too: one build per source, with the same keys
INSERT INTO builds_new (id, tenant_id, source, name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor,
	c_api_patch, body_hash, origin, origin_signature, created_at, created_by, checked, used_at)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-' ||
		substr('89ab', 1 + abs(random()) % 4, 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))),
	b.tenant_id, x.source, b.name, b.ext_version, b.platform, b.abi, b.duckdb_version, b.c_api_major, b.c_api_minor, b.c_api_patch,
	b.body_hash, b.origin, b.origin_signature, b.created_at, b.created_by, b.checked, b.used_at
FROM (SELECT DISTINCT build_id, source FROM releases) x JOIN builds_new b ON b.id = x.build_id
WHERE x.source <> b.source
-- +statement
-- a source's licence-key records go with its build: those another source recorded stay on the first
INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at)
SELECT k.tenant_id, n.id, k.kid, k.pem, k.source_kind, k.source_id, k.recorded_at
FROM build_license_keys k JOIN builds_new o ON o.id = k.build_id
JOIN builds_new n ON n.tenant_id = o.tenant_id AND n.name = o.name AND n.body_hash = o.body_hash AND n.id <> o.id
WHERE n.source = k.source_kind || ':' || k.source_id
-- +statement
DELETE FROM build_license_keys WHERE EXISTS (SELECT 1 FROM builds_new o JOIN builds_new n ON n.tenant_id = o.tenant_id AND n.name = o.name
	AND n.body_hash = o.body_hash AND n.id <> o.id
	WHERE o.id = build_license_keys.build_id AND n.source = build_license_keys.source_kind || ':' || build_license_keys.source_id)
-- +statement
DROP TABLE builds
-- +statement
ALTER TABLE builds_new RENAME TO builds
-- +statement
CREATE INDEX ix_builds_body ON builds (body_hash)
-- +statement
CREATE INDEX ix_builds_used ON builds (used_at)
-- +statement
CREATE TABLE releases_new (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL,
	channel_id       TEXT NOT NULL,
	build_id         TEXT NOT NULL,
	source           TEXT NOT NULL DEFAULT 'tenant',
	name             TEXT NOT NULL,
	ext_version      TEXT NOT NULL,
	platform         TEXT NOT NULL,
	slot             TEXT NOT NULL,
	state            TEXT NOT NULL CHECK (state IN ('active', 'deprecated', 'yanked')),
	visibility       TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
	seq              INTEGER,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	state_changed_at TEXT NOT NULL,
	state_changed_by TEXT NOT NULL,
	version          INTEGER NOT NULL,
	origin           TEXT NOT NULL DEFAULT 'admin',
	provenance       TEXT,
	origin_signature BLOB,
	publisher_statement TEXT,
	publisher_signature TEXT,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (build_id, tenant_id) REFERENCES builds (id, tenant_id) ON DELETE NO ACTION,
	CONSTRAINT ux_releases_slot UNIQUE (channel_id, source, name, ext_version, platform, slot),
	UNIQUE (id, channel_id)
)
-- +statement
INSERT INTO releases_new (id, tenant_id, channel_id, build_id, source, name, ext_version, platform, slot, state, visibility, seq,
	created_at, created_by, state_changed_at, state_changed_by, version, origin, provenance, origin_signature, publisher_statement,
	publisher_signature)
SELECT r.id, r.tenant_id, r.channel_id, (SELECT n.id FROM builds o JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name
		AND n.body_hash = o.body_hash AND n.source = r.source WHERE o.id = r.build_id),
	r.source, r.name, r.ext_version, r.platform, r.slot, r.state, r.visibility, r.seq, r.created_at, r.created_by, r.state_changed_at,
	r.state_changed_by, r.version, r.origin, r.provenance, r.origin_signature, r.publisher_statement, r.publisher_signature
FROM releases r
-- +statement
DROP TABLE releases
-- +statement
ALTER TABLE releases_new RENAME TO releases
-- +statement
CREATE UNIQUE INDEX ux_releases_seq ON releases (channel_id, seq) WHERE seq IS NOT NULL
-- +statement
CREATE INDEX ix_releases_resolve ON releases (channel_id, name, platform, state, seq)
-- +statement
CREATE INDEX ix_releases_build ON releases (build_id)
-- +statement
-- a slot purged before: its source is gone with its release, so it stays taken for every source
CREATE TABLE purged_slots_new (
	channel_id  TEXT NOT NULL,
	source      TEXT NOT NULL,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	platform    TEXT NOT NULL,
	slot        TEXT NOT NULL,
	abi         TEXT NOT NULL,
	body_hash   TEXT NOT NULL,
	seq         INTEGER,
	release_id  TEXT NOT NULL,
	purged_at   TEXT NOT NULL,
	CONSTRAINT pk_purged_slots PRIMARY KEY (channel_id, source, name, ext_version, platform, slot)
)
-- +statement
INSERT INTO purged_slots_new (channel_id, source, name, ext_version, platform, slot, abi, body_hash, seq, release_id, purged_at)
SELECT channel_id, '*', name, ext_version, platform, slot, abi, body_hash, seq, release_id, purged_at FROM purged_slots
-- +statement
DROP TABLE purged_slots
-- +statement
ALTER TABLE purged_slots_new RENAME TO purged_slots
