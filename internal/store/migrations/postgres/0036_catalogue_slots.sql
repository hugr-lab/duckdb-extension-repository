-- +min_reader 36
-- spec 0033 phase A1 part 3: a version is immutable within its source in the whole tenant, not only
-- in a channel: a build has its slot, and two bodies of one (source, name, version, platform, slot)
-- are refused at arrival; the tenant's own source (everyone who may publish in it) keeps the rule per
-- channel. Two bodies a source released in two channels before (possible until now) are both kept:
-- the one released (else the oldest) holds the unique key, the others are legacy duplicates. An older replica
-- would take a second body of a version in another channel, so it no longer runs here.
ALTER TABLE builds ADD COLUMN slot varchar(72)
-- +statement
UPDATE builds SET slot = CASE WHEN abi = 'c_struct' THEN 'capi:' || c_api_major ELSE duckdb_version END
-- +statement
ALTER TABLE builds ALTER COLUMN slot SET NOT NULL
-- +statement
ALTER TABLE builds ADD COLUMN legacy_duplicate boolean NOT NULL DEFAULT false
-- +statement
UPDATE builds SET legacy_duplicate = true WHERE source <> 'tenant' AND EXISTS (SELECT 1 FROM builds o WHERE o.tenant_id = builds.tenant_id
	AND o.source = builds.source AND o.name = builds.name AND o.ext_version = builds.ext_version AND o.platform = builds.platform
	AND o.slot = builds.slot AND o.id <> builds.id AND ((CASE WHEN EXISTS (SELECT 1 FROM releases r WHERE r.build_id = o.id) THEN 0 ELSE 1 END) < (CASE WHEN EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id) THEN 0 ELSE 1 END) OR (CASE WHEN EXISTS (SELECT 1 FROM releases r WHERE r.build_id = o.id) THEN 0 ELSE 1 END) = (CASE WHEN EXISTS (SELECT 1 FROM releases r WHERE r.build_id = builds.id) THEN 0 ELSE 1 END)
		AND (o.created_at < builds.created_at OR o.created_at = builds.created_at AND o.id < builds.id)))
-- +statement
CREATE UNIQUE INDEX ux_builds_slot ON builds (tenant_id, source, name, ext_version, platform, slot) WHERE NOT legacy_duplicate AND source <> 'tenant'
