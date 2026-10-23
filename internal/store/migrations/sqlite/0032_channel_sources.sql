-- +min_reader 32
-- spec 0031: a name's source in a channel. Each release records its source (upstream:<id>,
-- publisher:<id>, tenant); each channel serves a name from its selected source only. An older replica
-- would serve another source's releases, so it no longer runs here.
ALTER TABLE releases ADD COLUMN source TEXT NOT NULL DEFAULT 'tenant'
-- +statement
CREATE TABLE channel_sources (
	channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	name       TEXT NOT NULL,
	source     TEXT NOT NULL,
	set_by     TEXT NOT NULL,
	set_at     TEXT NOT NULL,
	PRIMARY KEY (channel_id, name)
)
-- +statement
-- a mirrored release: the upstream of its channel its actor names (system:upstream:<tenant>/<name>),
-- else the one whose cell holds the body, else one listing the name; a removed upstream's:
-- "upstream:"
UPDATE releases SET source = 'upstream:' || coalesce(
	(SELECT min(u.id) FROM upstreams u JOIN tenants t ON t.id = u.tenant_id
		WHERE u.channel_id = releases.channel_id AND releases.created_by = 'system:upstream:' || t.name || '/' || u.name),
	(SELECT min(u.id) FROM upstreams u JOIN upstream_cells c ON c.upstream_id = u.id JOIN builds b ON b.id = releases.build_id
		WHERE u.channel_id = releases.channel_id AND c.name = releases.name AND c.body_hash = b.body_hash),
	(SELECT min(u.id) FROM upstreams u JOIN upstream_entries e ON e.upstream_id = u.id
		WHERE u.channel_id = releases.channel_id AND e.name = releases.name),
	'')
WHERE origin = 'upstream'
-- +statement
-- a publisher's publication: the publisher its actor names (publisher:<tenant>/<name>[,…])
UPDATE releases SET source = 'publisher:' || (SELECT min(p.id) FROM publishers p JOIN tenants t ON t.id = p.tenant_id
		WHERE p.tenant_id = releases.tenant_id AND (releases.created_by = 'publisher:' || t.name || '/' || p.name
			OR releases.created_by LIKE 'publisher:' || t.name || '/' || p.name || ',%'))
WHERE origin = 'publication' AND EXISTS (SELECT 1 FROM publishers p JOIN tenants t ON t.id = p.tenant_id
		WHERE p.tenant_id = releases.tenant_id AND (releases.created_by = 'publisher:' || t.name || '/' || p.name
			OR releases.created_by LIKE 'publisher:' || t.name || '/' || p.name || ',%'))
-- +statement
-- a promotion: the source of the earliest release of its build that is not a promotion
UPDATE releases SET source = coalesce((SELECT o.source FROM releases o WHERE o.build_id = releases.build_id AND o.origin <> 'promotion'
		ORDER BY o.created_at, o.id LIMIT 1), 'tenant')
WHERE origin = 'promotion'
-- +statement
-- each channel's selection: the source of the release it serves now, a replacement before a mirror
-- (spec 0009), the newest current one first
INSERT INTO channel_sources (channel_id, name, source, set_by, set_at)
SELECT channel_id, name, source, 'system:migration', created_at FROM (
	SELECT channel_id, name, source, created_at, ROW_NUMBER() OVER (PARTITION BY channel_id, name
		ORDER BY CASE WHEN source LIKE 'upstream:%' THEN 1 ELSE 0 END, CASE WHEN seq IS NULL THEN 1 ELSE 0 END, seq DESC, created_at DESC, id DESC) AS n
	FROM releases WHERE state <> 'yanked') r
WHERE n = 1
