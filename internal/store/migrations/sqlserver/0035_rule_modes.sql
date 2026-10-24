-- +min_reader 35
-- spec 0033 phase A1 part 2: a channel's rule for a name may have a mode, set by an administrator:
-- auto (each new version of its source is placed) or manual (the versions the channel holds are
-- completed, nothing newer is placed); a rule an arrival wrote has none (the channel serves what lands
-- in it, as before). The placement pass's bookkeeping; a build carries its visibility and provenance,
-- so a placement makes its release as its arrival would have, and whether it landed in the catalogue
-- without a release (kept from the collector until it is placed). An older replica would place a
-- manual rule's new versions, or collect a landed build, so it no longer runs here.
ALTER TABLE channel_sources ADD mode nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_channel_sources_mode DEFAULT ''
-- +statement
ALTER TABLE channel_sources ADD checked_at datetime2(6) NULL
-- +statement
ALTER TABLE channel_sources ADD outcome nvarchar(400) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE channel_sources ADD outcome_at datetime2(6) NULL
-- +statement
ALTER TABLE builds ADD visibility nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_builds_visibility DEFAULT 'private'
-- +statement
ALTER TABLE builds ADD provenance nvarchar(4000) NULL
-- +statement
-- as its first release came: a promotion's facts are not the build's
UPDATE builds SET visibility = coalesce((SELECT TOP 1 r.visibility FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion'
		ORDER BY r.created_at, r.id), 'private'),
	provenance = (SELECT TOP 1 r.provenance FROM releases r WHERE r.build_id = builds.id AND r.origin <> 'promotion' ORDER BY r.created_at, r.id)
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, source, name, created_at)
-- +statement
ALTER TABLE builds ADD landed bit NOT NULL CONSTRAINT df_builds_landed DEFAULT 0
