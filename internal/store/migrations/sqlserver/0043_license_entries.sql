-- +min_reader 43
-- spec 0035 phase 1: a licence belongs to a catalogue entry (tenant, type, name, source), named by
-- every write; key owners go. A relayed licence in its relaying upstream's entry stays relayed. One
-- in another relaying upstream's own entry goes: that upstream's relay, requested below, fills its
-- entry again and governs what it stores, where an import there would never be revoked. One in any
-- other entry (a publisher's, a bundle-mode or credential-less upstream's: migration 0042's owner,
-- or an administrator's act, put it there), or whose relayer is gone, stays where it is served, as
-- an import (its relay no longer governs it; the relayer's own entry is filled again by its next
-- run). One of no entry ('unknown') moves into its live relayer's entry, unless a row is there
-- already; the remaining 'unknown' licences go. Every relaying upstream is asked for a run. The
-- column becomes entry_source, and the owners' table and the index only the first recorder read go.
-- An older replica reads and writes the owners and relies on the relay's exceptions, so it no longer
-- runs here.
-- sp_rename renames the column, the primary key following.
DELETE FROM licenses WHERE source = 'upstream' AND owner_source <> 'unknown'
AND (upstream_id IS NULL OR owner_source <> 'upstream:' + upstream_id)
AND EXISTS (SELECT 1 FROM upstreams y WHERE y.tenant_id = licenses.tenant_id AND licenses.owner_source = 'upstream:' + y.id
	AND y.kind = 'repository' AND y.mode <> 'bundle' AND y.credential IS NOT NULL)
-- +statement
UPDATE licenses SET source = 'import', upstream_id = NULL
WHERE source = 'upstream' AND owner_source <> 'unknown' AND (upstream_id IS NULL OR owner_source <> 'upstream:' + upstream_id)
-- +statement
DELETE FROM licenses WHERE source = 'upstream' AND owner_source = 'unknown'
AND (NOT EXISTS (SELECT 1 FROM upstreams u WHERE u.id = licenses.upstream_id AND u.tenant_id = licenses.tenant_id)
	OR EXISTS (SELECT 1 FROM licenses o WHERE o.tenant_id = licenses.tenant_id AND o.package_type = licenses.package_type
		AND o.name = licenses.name AND o.owner_source = 'upstream:' + licenses.upstream_id))
-- +statement
UPDATE licenses SET owner_source = 'upstream:' + upstream_id WHERE source = 'upstream' AND owner_source = 'unknown'
-- +statement
DELETE FROM licenses WHERE owner_source = 'unknown'
-- +statement
UPDATE license_syncs SET requested_at = SYSUTCDATETIME() WHERE upstream_id IN (SELECT id FROM upstreams
	WHERE kind = 'repository' AND mode <> 'bundle' AND credential IS NOT NULL)
-- +statement
INSERT INTO license_syncs (upstream_id, tenant_id, requested_at)
SELECT u.id, u.tenant_id, SYSUTCDATETIME() FROM upstreams u WHERE u.kind = 'repository' AND u.mode <> 'bundle' AND u.credential IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM license_syncs l WHERE l.upstream_id = u.id)
-- +statement
EXEC sp_rename N'licenses.owner_source', N'entry_source', N'COLUMN'
-- +statement
DROP TABLE license_key_owners
-- +statement
DROP INDEX ix_build_license_keys_kid ON build_license_keys
