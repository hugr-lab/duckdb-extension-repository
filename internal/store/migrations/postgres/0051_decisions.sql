-- +min_reader 51
-- spec 0041: a rule keeps the version its last source.waiting named (told once per version), and
-- the administrators' decisions on the decision queue (a skip of a waiting version, a dismissal keyed
-- to its situation) are kept, one per (scope, kind, source, name): a channel's, or the tenant's. An
-- older replica's tenant reaper does not know the table, which references channels, so it no longer
-- runs here.
ALTER TABLE channel_sources ADD COLUMN told_version varchar(64) NULL
-- +statement
CREATE TABLE decisions (
	id              varchar(36) PRIMARY KEY,
	tenant_id       varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type    varchar(32) NOT NULL,
	channel_id      varchar(36) NULL REFERENCES channels (id),
	kind            varchar(8) NOT NULL CHECK (kind IN ('skip', 'dismiss')),
	source          varchar(64) NOT NULL,
	name            varchar(64) NOT NULL,
	package_version varchar(64) NULL,
	situation       text NULL,
	version         bigint NOT NULL,
	decided_by      varchar(400) NOT NULL,
	decided_at      timestamptz NOT NULL
)
-- +statement
CREATE UNIQUE INDEX ux_decisions_channel ON decisions (channel_id, kind, source, name) WHERE channel_id IS NOT NULL
-- +statement
CREATE UNIQUE INDEX ux_decisions_tenant ON decisions (tenant_id, package_type, kind, source, name) WHERE channel_id IS NULL
-- +statement
-- the collector's and a source's deletes
CREATE INDEX ix_decisions_entry ON decisions (tenant_id, package_type, source, name)
