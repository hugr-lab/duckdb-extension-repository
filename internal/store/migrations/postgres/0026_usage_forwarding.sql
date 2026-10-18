-- spec 0012 phase 1c: usage forwarded upstream, and daily aggregates received from downstream
ALTER TABLE upstreams ADD COLUMN usage varchar(8) NOT NULL DEFAULT 'forward'
-- +statement
CREATE TABLE usage_received (
	tenant_id   varchar(36) NOT NULL,
	channel_id  varchar(36) NOT NULL,
	sender      varchar(64) NOT NULL,
	day         varchar(10) NOT NULL,
	name        varchar(64) NOT NULL,
	ext_version varchar(64) NOT NULL,
	event       varchar(64) NOT NULL,
	counter     varchar(64) NOT NULL,
	users       bigint NOT NULL,
	value       bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, sender, day, name, ext_version, event, counter)
)
-- +statement
CREATE INDEX ix_usage_received_day ON usage_received (tenant_id, day)
-- +statement
CREATE TABLE usage_forwards (
	upstream_id varchar(36) PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id),
	last_run_at timestamptz NOT NULL,
	last_result text NOT NULL
)
