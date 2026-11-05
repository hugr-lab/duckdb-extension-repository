-- +min_reader 51
-- spec 0041: a rule keeps the version its last source.waiting named (told once per version), and
-- the administrators' decisions on the decision queue (a skip of a waiting version, a dismissal keyed
-- to its situation) are kept, one per (scope, kind, source, name): a channel's, or the tenant's. An
-- older replica's tenant reaper does not know the table, which references channels, so it no longer
-- runs here.
ALTER TABLE channel_sources ADD told_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
CREATE TABLE decisions (
	id              nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type    nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL REFERENCES channels (id),
	kind            nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (kind IN ('skip', 'dismiss')),
	source          nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	package_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	situation       nvarchar(max) COLLATE Latin1_General_100_BIN2 NULL,
	version         bigint NOT NULL,
	decided_by      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	decided_at      datetime2(6) NOT NULL
)
-- +statement
CREATE UNIQUE INDEX ux_decisions_channel ON decisions (channel_id, kind, source, name) WHERE channel_id IS NOT NULL
-- +statement
CREATE UNIQUE INDEX ux_decisions_tenant ON decisions (tenant_id, package_type, kind, source, name) WHERE channel_id IS NULL
-- +statement
-- a source's delete (WHERE source = ?), and the deletes of a (source, name)'s once its last build goes
CREATE INDEX ix_decisions_entry ON decisions (source, tenant_id, package_type, name)
-- +statement
-- a tenant's list by id, the reaper's batched deletes, and the tenant's foreign key
CREATE INDEX ix_decisions_tenant ON decisions (tenant_id, id)
