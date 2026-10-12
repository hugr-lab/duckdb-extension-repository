-- +min_reader 19
-- spec 0019: deleting a tenant; an older replica would let a deleting tenant be resumed and keep
-- running its upstreams and feeds
ALTER TABLE tenants ADD COLUMN suspended_at timestamptz
-- +statement
ALTER TABLE tenants ADD COLUMN deleting_at timestamptz
-- +statement
ALTER TABLE tenants ADD COLUMN deleted_at timestamptz
-- +statement
UPDATE tenants SET suspended_at = now() WHERE state = 'suspended'
-- +statement
CREATE INDEX ix_tenants_deleting ON tenants (deleting_at)
-- +statement
CREATE TABLE tenant_deletions (
	tenant_id       varchar(36) PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	started_at      timestamptz NOT NULL,
	started_by      varchar(400) NOT NULL,
	keys            text NOT NULL,
	channel_ids     text NOT NULL,
	run_leases      text NOT NULL,
	passes          bigint NOT NULL,
	failures        bigint NOT NULL,
	current_table   varchar(64) NOT NULL,
	deleted_rows    bigint NOT NULL,
	last_error      varchar(32) NOT NULL,
	next_attempt_at timestamptz NOT NULL,
	finished_at     timestamptz,
	last_checked_at timestamptz
)
