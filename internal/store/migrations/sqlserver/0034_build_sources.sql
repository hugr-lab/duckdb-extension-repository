-- +min_reader 34
-- spec 0033 phase A1: a build belongs to its source, and a slot is a source's: two sources may
-- release the same version of a name in one channel. An older replica would refuse the second, or
-- read another source's release as the slot's, so it no longer runs here.
ALTER TABLE builds ADD source nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_builds_source DEFAULT 'tenant'
-- +statement
-- a build's source is its first release's (they all agree but where spec 0031's same-body rule
-- gave a slot's release to another source)
UPDATE builds SET source = (SELECT TOP 1 r.source FROM releases r WHERE r.build_id = builds.id ORDER BY r.created_at, r.id)
WHERE EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)
-- +statement
-- a build without releases (its releases purged, the collector not past it yet): by who made it, as
-- migration 0032 read releases' actors, so its source finds it again
UPDATE builds SET source = coalesce(
	CASE origin WHEN 'upstream' THEN 'upstream:' + (SELECT min(u.id) FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
		WHERE u.tenant_id = builds.tenant_id AND builds.created_by = 'system:upstream:' + t.name + '/' + u.name)
	WHEN 'publication' THEN 'publisher:' + (SELECT min(p.id) FROM publishers p JOIN tenants t ON t.id = p.tenant_id
		WHERE p.tenant_id = builds.tenant_id AND (builds.created_by = 'publisher:' + t.name + '/' + p.name
			OR builds.created_by LIKE 'publisher:' + t.name + '/' + p.name + ',%')) END, 'tenant')
WHERE NOT EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id)
-- +statement
ALTER TABLE builds DROP CONSTRAINT df_builds_source
-- +statement
DECLARE @n sysname = (SELECT kc.name FROM sys.key_constraints kc WHERE kc.parent_object_id = OBJECT_ID('builds') AND kc.type = 'UQ'
	AND EXISTS (SELECT 1 FROM sys.index_columns ic JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id AND c.name = 'body_hash'));
IF @n IS NULL THROW 50000, 'migration 0034: the builds (tenant_id, name, body_hash) constraint was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE builds DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE builds ADD CONSTRAINT ux_builds_source UNIQUE (tenant_id, source, name, body_hash)
-- +statement
-- a build whose releases name another source too: one build per source, with the same keys
INSERT INTO builds (id, tenant_id, name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor, c_api_patch,
	body_hash, origin, origin_signature, created_at, created_by, checked, used_at, source)
SELECT lower(CONVERT(nvarchar(36), NEWID())), b.tenant_id, b.name, b.ext_version, b.platform, b.abi, b.duckdb_version, b.c_api_major,
	b.c_api_minor, b.c_api_patch, b.body_hash, b.origin, b.origin_signature, b.created_at, b.created_by, b.checked, b.used_at, x.source
FROM (SELECT DISTINCT build_id, source FROM releases) x JOIN builds b ON b.id = x.build_id
WHERE x.source <> b.source
-- +statement
-- a source's licence-key records go with its build: those another source recorded stay on the first
INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at)
SELECT k.tenant_id, n.id, k.kid, k.pem, k.source_kind, k.source_id, k.recorded_at
FROM build_license_keys k JOIN builds o ON o.id = k.build_id
JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name AND n.body_hash = o.body_hash AND n.id <> o.id
WHERE n.source = k.source_kind + ':' + k.source_id
-- +statement
DELETE FROM build_license_keys WHERE EXISTS (SELECT 1 FROM builds o JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name
	AND n.body_hash = o.body_hash AND n.id <> o.id
	WHERE o.id = build_license_keys.build_id AND n.source = build_license_keys.source_kind + ':' + build_license_keys.source_id)
-- +statement
UPDATE releases SET build_id = (SELECT n.id FROM builds o JOIN builds n ON n.tenant_id = o.tenant_id AND n.name = o.name
		AND n.body_hash = o.body_hash AND n.source = releases.source WHERE o.id = releases.build_id)
WHERE source <> (SELECT b.source FROM builds b WHERE b.id = releases.build_id)
-- +statement
DECLARE @n sysname = (SELECT kc.name FROM sys.key_constraints kc WHERE kc.parent_object_id = OBJECT_ID('releases') AND kc.type = 'UQ'
	AND EXISTS (SELECT 1 FROM sys.index_columns ic JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id AND c.name = 'slot'));
IF @n IS NULL THROW 50000, 'migration 0034: the releases slot constraint was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE releases DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE releases ADD CONSTRAINT ux_releases_slot UNIQUE (channel_id, source, name, ext_version, platform, slot)
-- +statement
-- a slot purged before: its source is gone with its release, so it stays taken for every source
ALTER TABLE purged_slots ADD source nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_purged_slots_source DEFAULT '*'
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('purged_slots') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0034: the purged_slots primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE purged_slots DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE purged_slots ADD CONSTRAINT pk_purged_slots PRIMARY KEY (channel_id, source, name, ext_version, platform, slot)
-- +statement
ALTER TABLE purged_slots DROP CONSTRAINT df_purged_slots_source
