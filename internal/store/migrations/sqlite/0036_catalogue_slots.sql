-- +min_reader 36
-- spec 0033 phase A1 part 3: a version is immutable within its source in the whole tenant, not only
-- in a channel: a build has its slot, and two bodies of one (source, name, version, platform, slot)
-- are refused at arrival. Two bodies a source released in two channels before (possible until now)
-- are both kept, all but the oldest marked legacy_duplicate, outside the unique key. An older replica
-- would take a second body of a version in another channel, so it no longer runs here.
ALTER TABLE builds ADD COLUMN slot TEXT NOT NULL DEFAULT ''
-- +statement
UPDATE builds SET slot = CASE WHEN abi = 'c_struct' THEN 'capi:' || c_api_major ELSE duckdb_version END
-- +statement
ALTER TABLE builds ADD COLUMN legacy_duplicate INTEGER NOT NULL DEFAULT 0
-- +statement
UPDATE builds SET legacy_duplicate = 1 WHERE EXISTS (SELECT 1 FROM builds o WHERE o.tenant_id = builds.tenant_id
	AND o.source = builds.source AND o.name = builds.name AND o.ext_version = builds.ext_version AND o.platform = builds.platform
	AND o.slot = builds.slot AND (o.created_at < builds.created_at OR o.created_at = builds.created_at AND o.id < builds.id))
-- +statement
CREATE UNIQUE INDEX ux_builds_slot ON builds (tenant_id, source, name, ext_version, platform, slot) WHERE legacy_duplicate = 0
