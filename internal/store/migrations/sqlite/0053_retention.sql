-- +min_reader 53
-- +foreign_keys_off
-- spec 0042: version retention in channels. A release gains the state retired (not served, restored
-- by an administrator until the grace after which the retention pass purges it), with the state it
-- came from and when; a tenant's default rule and a channel's own (keep the newest N versions per
-- DuckDB version, purge after a grace), and the pins that keep a version whatever the rule. An older
-- replica does not know the retired state (it would refuse or misread it), and its tenant reaper does
-- not know retention_pins, which references channels, so it no longer runs here. SQLite rebuilds
-- releases to widen its state's CHECK, with foreign keys off (release_signatures references it) and
-- checked before the commit, keeping its indexes.
CREATE TABLE releases_new (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL,
	channel_id       TEXT NOT NULL,
	build_id         TEXT NOT NULL,
	source           TEXT NOT NULL DEFAULT 'tenant',
	name             TEXT NOT NULL,
	package_version  TEXT NOT NULL,
	platform         TEXT NOT NULL,
	slot             TEXT NOT NULL,
	state            TEXT NOT NULL CHECK (state IN ('active', 'deprecated', 'yanked', 'retired')),
	visibility       TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
	seq              INTEGER,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	state_changed_at TEXT NOT NULL,
	state_changed_by TEXT NOT NULL,
	version          INTEGER NOT NULL,
	origin           TEXT NOT NULL DEFAULT 'admin',
	provenance       TEXT,
	origin_signature BLOB,
	publisher_statement TEXT,
	publisher_signature TEXT,
	retired_from     TEXT NULL,
	retired_at       TEXT NULL,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (build_id, tenant_id) REFERENCES builds (id, tenant_id) ON DELETE NO ACTION,
	CONSTRAINT ux_releases_slot UNIQUE (channel_id, source, name, package_version, platform, slot),
	UNIQUE (id, channel_id)
)
-- +statement
INSERT INTO releases_new (id, tenant_id, channel_id, build_id, source, name, package_version, platform, slot, state, visibility, seq,
	created_at, created_by, state_changed_at, state_changed_by, version, origin, provenance, origin_signature, publisher_statement,
	publisher_signature)
SELECT id, tenant_id, channel_id, build_id, source, name, package_version, platform, slot, state, visibility, seq, created_at, created_by,
	state_changed_at, state_changed_by, version, origin, provenance, origin_signature, publisher_statement, publisher_signature
FROM releases
-- +statement
DROP TABLE releases
-- +statement
ALTER TABLE releases_new RENAME TO releases
-- +statement
CREATE UNIQUE INDEX ux_releases_seq ON releases (channel_id, seq) WHERE seq IS NOT NULL
-- +statement
CREATE INDEX ix_releases_resolve ON releases (channel_id, name, platform, state, seq)
-- +statement
CREATE INDEX ix_releases_build ON releases (build_id)
-- +statement
-- a rule's fields: NULL is not set; a channel's keep 0 is none there, whatever the tenant's
ALTER TABLE tenants ADD COLUMN retention_keep INTEGER NULL
-- +statement
ALTER TABLE tenants ADD COLUMN retention_grace_seconds INTEGER NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_keep INTEGER NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_grace_seconds INTEGER NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_checked_at TEXT NULL
-- +statement
-- a pin keeps (name, version) in one DuckDB version of the channel, or in every one ('')
CREATE TABLE retention_pins (
	id              TEXT PRIMARY KEY,
	channel_id      TEXT NOT NULL REFERENCES channels (id),
	name            TEXT NOT NULL,
	package_version TEXT NOT NULL,
	duckdb_version  TEXT NOT NULL,
	reason          TEXT NOT NULL,
	pinned_by       TEXT NOT NULL,
	pinned_at       TEXT NOT NULL,
	CONSTRAINT ux_retention_pins UNIQUE (channel_id, name, package_version, duckdb_version)
)
