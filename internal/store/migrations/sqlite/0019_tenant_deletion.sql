-- +min_reader 19
-- spec 0019: deleting a tenant; an older replica would let a deleting tenant be resumed and keep
-- running its upstreams and feeds
ALTER TABLE tenants ADD COLUMN suspended_at TEXT
-- +statement
ALTER TABLE tenants ADD COLUMN deleting_at TEXT
-- +statement
ALTER TABLE tenants ADD COLUMN deleted_at TEXT
-- +statement
UPDATE tenants SET suspended_at = strftime('%Y-%m-%dT%H:%M:%S.000000Z', 'now') WHERE state = 'suspended'
-- +statement
CREATE INDEX ix_tenants_deleting ON tenants (deleting_at)
-- +statement
CREATE TABLE tenant_deletions (
	tenant_id       TEXT PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	started_at      TEXT NOT NULL,
	started_by      TEXT NOT NULL,
	keys            TEXT NOT NULL,
	channel_ids     TEXT NOT NULL,
	run_leases      TEXT NOT NULL,
	passes          INTEGER NOT NULL,
	failures        INTEGER NOT NULL,
	current_table   TEXT NOT NULL,
	deleted_rows    INTEGER NOT NULL,
	last_error      TEXT NOT NULL,
	next_attempt_at TEXT NOT NULL,
	finished_at     TEXT,
	last_checked_at TEXT
)
