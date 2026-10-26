-- +min_reader 43
-- spec 0035 phase 1: a licence belongs to a catalogue entry (tenant, type, name, source), named by
-- every write; key owners go. A relayed licence moves into its relaying upstream's entry: the row
-- already there stays, and of several rows moving into one empty entry the one that expires last
-- (then by licence id, then by its entry before) stays; the movers pass through a temporary entry,
-- so no move meets another on the key. Imports of no entry ('unknown') go; the others keep theirs.
-- Every relaying upstream is asked for a run. The column becomes entry_source, and the owners'
-- table and the index only the first recorder read go. An older replica reads and writes the
-- owners and relies on the relay's exceptions, so it no longer runs here.
DELETE FROM licenses WHERE source = 'upstream' AND upstream_id IS NULL
-- +statement
DELETE FROM licenses WHERE source = 'upstream' AND owner_source <> 'upstream:' || upstream_id
AND EXISTS (SELECT 1 FROM licenses o WHERE o.tenant_id = licenses.tenant_id AND o.package_type = licenses.package_type
	AND o.name = licenses.name AND o.owner_source = 'upstream:' || licenses.upstream_id
	AND NOT (o.source = 'upstream' AND o.owner_source <> 'upstream:' || o.upstream_id))
-- +statement
DELETE FROM licenses WHERE source = 'upstream' AND owner_source <> 'upstream:' || upstream_id
AND EXISTS (SELECT 1 FROM licenses o WHERE o.tenant_id = licenses.tenant_id AND o.package_type = licenses.package_type
	AND o.name = licenses.name AND o.source = 'upstream' AND o.upstream_id = licenses.upstream_id AND o.owner_source <> 'upstream:' || o.upstream_id
	AND (o.expires_at > licenses.expires_at OR o.expires_at = licenses.expires_at AND (o.license_id < licenses.license_id
		OR o.license_id = licenses.license_id AND o.owner_source < licenses.owner_source)))
-- +statement
UPDATE licenses SET owner_source = 'moving:' || owner_source WHERE source = 'upstream' AND owner_source <> 'upstream:' || upstream_id
-- +statement
UPDATE licenses SET owner_source = 'upstream:' || upstream_id WHERE owner_source LIKE 'moving:%'
-- +statement
DELETE FROM licenses WHERE owner_source = 'unknown'
-- +statement
UPDATE license_syncs SET requested_at = now() WHERE upstream_id IN (SELECT id FROM upstreams
	WHERE kind = 'repository' AND mode <> 'bundle' AND credential IS NOT NULL)
-- +statement
INSERT INTO license_syncs (upstream_id, tenant_id, requested_at)
SELECT u.id, u.tenant_id, now() FROM upstreams u WHERE u.kind = 'repository' AND u.mode <> 'bundle' AND u.credential IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM license_syncs l WHERE l.upstream_id = u.id)
-- +statement
ALTER TABLE licenses RENAME COLUMN owner_source TO entry_source
-- +statement
DROP TABLE license_key_owners
-- +statement
DROP INDEX ix_build_license_keys_kid
