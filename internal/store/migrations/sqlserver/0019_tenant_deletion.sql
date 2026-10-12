-- +min_reader 19
-- spec 0019: deleting a tenant; an older replica would let a deleting tenant be resumed and keep
-- running its upstreams and feeds
ALTER TABLE tenants ADD suspended_at datetime2(6) NULL
-- +statement
ALTER TABLE tenants ADD deleting_at datetime2(6) NULL
-- +statement
ALTER TABLE tenants ADD deleted_at datetime2(6) NULL
-- +statement
UPDATE tenants SET suspended_at = SYSUTCDATETIME() WHERE state = 'suspended'
-- +statement
CREATE INDEX ix_tenants_deleting ON tenants (deleting_at)
-- +statement
CREATE TABLE tenant_deletions (
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	started_at      datetime2(6) NOT NULL,
	started_by      nvarchar(400) NOT NULL,
	keys            nvarchar(max) NOT NULL,
	channel_ids     nvarchar(max) NOT NULL,
	run_leases      nvarchar(max) NOT NULL,
	passes          bigint NOT NULL,
	failures        bigint NOT NULL,
	current_table   nvarchar(64) NOT NULL,
	deleted_rows    bigint NOT NULL,
	last_error      nvarchar(32) NOT NULL,
	next_attempt_at datetime2(6) NOT NULL,
	finished_at     datetime2(6) NULL,
	last_checked_at datetime2(6) NULL
)
