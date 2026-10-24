-- +min_reader 35
-- spec 0033 phase A1 part 2: a channel's rule for a name may have a mode, set by an administrator:
-- auto (each new version of its source is placed) or manual (the versions the channel holds are
-- completed, nothing newer is placed); a rule an arrival wrote has none (the channel serves what lands
-- in it, as before). The placement pass's bookkeeping; a build carries its visibility and provenance,
-- so a placement makes its release as its arrival would have, and whether it landed in the catalogue
-- without a release (kept from the collector until it is placed). An older replica would place a
-- manual rule's new versions, or collect a landed build, so it no longer runs here.
ALTER TABLE channel_sources ADD COLUMN mode TEXT NOT NULL DEFAULT ''
-- +statement
ALTER TABLE channel_sources ADD COLUMN checked_at TEXT
-- +statement
ALTER TABLE channel_sources ADD COLUMN outcome TEXT
-- +statement
ALTER TABLE channel_sources ADD COLUMN outcome_at TEXT
-- +statement
ALTER TABLE builds ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private'
-- +statement
ALTER TABLE builds ADD COLUMN provenance TEXT
-- +statement
-- as its first release came: a promotion's facts are not the build's
UPDATE builds SET visibility = coalesce((SELECT r.visibility FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion'
		ORDER BY r.created_at, r.id LIMIT 1), 'private'),
	provenance = (SELECT r.provenance FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion' ORDER BY r.created_at, r.id LIMIT 1)
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, source, name, created_at)
-- +statement
ALTER TABLE builds ADD COLUMN landed INTEGER NOT NULL DEFAULT 0
