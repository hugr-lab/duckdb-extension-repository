-- spec 0012 phase 1c: usage forwarded upstream, and daily aggregates received from downstream
ALTER TABLE upstreams ADD usage nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_upstreams_usage DEFAULT N'forward'
-- +statement
CREATE TABLE usage_received (
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	sender      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	day         nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	event       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	counter     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	users       bigint NOT NULL,
	value       bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, sender, day, name, ext_version, event, counter)
)
-- +statement
CREATE INDEX ix_usage_received_day ON usage_received (tenant_id, day)
-- +statement
CREATE TABLE usage_forwards (
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	last_run_at datetime2(6) NOT NULL,
	last_result nvarchar(max) NOT NULL
)
