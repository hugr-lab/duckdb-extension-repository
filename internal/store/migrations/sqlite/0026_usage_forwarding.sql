-- spec 0012 phase 1c: usage forwarded upstream, and daily aggregates received from downstream
ALTER TABLE upstreams ADD COLUMN usage TEXT NOT NULL DEFAULT 'forward'
-- +statement
CREATE TABLE usage_received (
	tenant_id   TEXT NOT NULL,
	channel_id  TEXT NOT NULL,
	sender      TEXT NOT NULL,
	day         TEXT NOT NULL,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	event       TEXT NOT NULL,
	counter     TEXT NOT NULL,
	users       INTEGER NOT NULL,
	value       INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, sender, day, name, ext_version, event, counter)
)
-- +statement
CREATE INDEX ix_usage_received_day ON usage_received (tenant_id, day)
-- +statement
CREATE TABLE usage_forwards (
	upstream_id TEXT PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id   TEXT NOT NULL REFERENCES tenants (id),
	last_run_at TEXT NOT NULL,
	last_result TEXT NOT NULL
)
