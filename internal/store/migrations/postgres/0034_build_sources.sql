-- +min_reader 34
-- spec 0033 phase A1: a build belongs to its source, and a slot is a source's: two sources may
-- release the same version of a name in one channel. An older replica would refuse the second, or
-- read another source's release as the slot's, so it no longer runs here.
ALTER TABLE builds ADD COLUMN source varchar(64) NOT NULL DEFAULT 'tenant'
-- +statement
-- a build's source is its first release's (they all agree but where spec 0031's same-body rule
-- gave a slot's release to another source)
UPDATE builds SET source = (SELECT r.source FROM releases r WHERE r.build_id = builds.id ORDER BY r.created_at, r.id LIMIT 1)
WHERE EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)
-- +statement
ALTER TABLE builds DROP CONSTRAINT builds_tenant_id_name_body_hash_key
-- +statement
ALTER TABLE builds ADD CONSTRAINT ux_builds_source UNIQUE (tenant_id, source, name, body_hash)
-- +statement
-- a build whose releases name another source too: one build per source, with the same keys
INSERT INTO builds (id, tenant_id, name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor, c_api_patch,
	body_hash, origin, origin_signature, created_at, created_by, checked, used_at, source)
SELECT gen_random_uuid()::text, b.tenant_id, b.name, b.ext_version, b.platform, b.abi, b.duckdb_version, b.c_api_major,
	b.c_api_minor, b.c_api_patch, b.body_hash, b.origin, b.origin_signature, b.created_at, b.created_by, b.checked, b.used_at, x.source
FROM (SELECT DISTINCT build_id, source FROM releases) x JOIN builds b ON b.id = x.build_id
WHERE x.source <> b.source
-- +statement
INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at)
SELECT k.tenant_id, n.id, k.kid, k.pem, k.source_kind, k.source_id, k.recorded_at
FROM build_license_keys k JOIN builds o ON o.id = k.build_id
JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name AND n.body_hash = o.body_hash AND n.id <> o.id
-- +statement
UPDATE releases SET build_id = (SELECT n.id FROM builds o JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name
		AND n.body_hash = o.body_hash AND n.source = releases.source WHERE o.id = releases.build_id)
WHERE source <> (SELECT b.source FROM builds b WHERE b.id = releases.build_id)
-- +statement
ALTER TABLE releases DROP CONSTRAINT releases_channel_id_name_ext_version_platform_slot_key
-- +statement
ALTER TABLE releases ADD CONSTRAINT ux_releases_slot UNIQUE (channel_id, source, name, ext_version, platform, slot)
-- +statement
-- a slot purged before: its source is gone with its release, so it stays taken for every source
ALTER TABLE purged_slots ADD COLUMN source varchar(64) NOT NULL DEFAULT '*'
-- +statement
ALTER TABLE purged_slots DROP CONSTRAINT purged_slots_pkey
-- +statement
ALTER TABLE purged_slots ADD CONSTRAINT pk_purged_slots PRIMARY KEY (channel_id, source, name, ext_version, platform, slot)
