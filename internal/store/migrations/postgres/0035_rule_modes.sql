-- +min_reader 35
-- spec 0033 phase A1 part 2: a channel's rule for a name has a mode, auto (each new version of its
-- source is placed) or manual (the versions the channel holds are completed, nothing newer is
-- placed), and the placement pass's bookkeeping; a build carries its visibility and provenance, so
-- a placement makes its release as its arrival would have. An older replica would place a manual
-- rule's new versions, so it no longer runs here.
ALTER TABLE channel_sources ADD COLUMN mode varchar(8) NOT NULL DEFAULT 'manual'
-- +statement
ALTER TABLE channel_sources ADD COLUMN checked_at timestamptz
-- +statement
ALTER TABLE channel_sources ADD COLUMN outcome varchar(400)
-- +statement
ALTER TABLE channel_sources ADD COLUMN outcome_at timestamptz
-- +statement
-- an upstream's names in its own channel are taken automatically, as its runs did; every other
-- rule (a publisher's, the tenant's, an upstream's in a channel promoted into) is manual
UPDATE channel_sources SET mode = 'auto' WHERE EXISTS (SELECT 1 FROM upstreams u
	WHERE u.channel_id = channel_sources.channel_id AND channel_sources.source = 'upstream:' || u.id)
-- +statement
ALTER TABLE builds ADD COLUMN visibility varchar(16) NOT NULL DEFAULT 'private'
-- +statement
ALTER TABLE builds ADD COLUMN provenance varchar(4000)
-- +statement
-- as its first release came: a promotion's facts are not the build's
UPDATE builds SET visibility = coalesce((SELECT r.visibility FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion'
		ORDER BY r.created_at, r.id LIMIT 1), 'private'),
	provenance = (SELECT r.provenance FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion' ORDER BY r.created_at, r.id LIMIT 1)
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, source, name, created_at)
