-- +min_reader 20
-- spec 0014: bundle-mode upstreams and bundle imports; an older replica would fetch a bundle-mode
-- upstream online, or give it a credential
DECLARE @n sysname = (SELECT name FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID('upstreams') AND definition LIKE '%pull-through%');
EXEC ('ALTER TABLE upstreams DROP CONSTRAINT ' + @n)
-- +statement
ALTER TABLE upstreams ADD CONSTRAINT ck_upstreams_mode CHECK (mode IN ('mirror', 'pull-through', 'bundle'))
-- +statement
ALTER TABLE upstreams ADD bundle_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE upstreams ADD bundle_created_at datetime2(6) NULL
-- +statement
ALTER TABLE upstreams ADD bundle_manifest_sha256 nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
CREATE TABLE bundle_imports (
	id                  nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id           nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	manifest_id         nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	manifest_created_at datetime2(6) NOT NULL,
	manifest_sha256     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	exporter            nvarchar(1000) NOT NULL,
	bundle_key          nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL,
	input_sha256        nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	dry_run             bit NOT NULL,
	state               nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('running', 'done', 'failed')),
	counts              nvarchar(max) NOT NULL,
	started_at          datetime2(6) NOT NULL,
	finished_at         datetime2(6) NULL,
	actor               nvarchar(400) NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_imports_tenant ON bundle_imports (tenant_id, started_at)
