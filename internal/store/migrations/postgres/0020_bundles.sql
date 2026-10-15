-- +min_reader 20
-- spec 0014: bundle-mode upstreams and bundle imports; an older replica would fetch a bundle-mode
-- upstream online, or give it a credential
ALTER TABLE upstreams DROP CONSTRAINT upstreams_mode_check
-- +statement
ALTER TABLE upstreams ADD CONSTRAINT upstreams_mode_check CHECK (mode IN ('mirror', 'pull-through', 'bundle'))
-- +statement
ALTER TABLE upstreams ADD COLUMN bundle_id varchar(36)
-- +statement
ALTER TABLE upstreams ADD COLUMN bundle_created_at timestamptz
-- +statement
ALTER TABLE upstreams ADD COLUMN bundle_manifest_sha256 varchar(64)
-- +statement
CREATE TABLE bundle_imports (
	id                  varchar(36) PRIMARY KEY,
	tenant_id           varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	manifest_id         varchar(36) NOT NULL,
	manifest_created_at timestamptz NOT NULL,
	manifest_sha256     varchar(64) NOT NULL,
	exporter            varchar(1000) NOT NULL,
	bundle_key          varchar(80) NOT NULL,
	input_sha256        varchar(64),
	dry_run             boolean NOT NULL,
	state               varchar(16) NOT NULL CHECK (state IN ('running', 'done', 'failed')),
	counts              text NOT NULL,
	started_at          timestamptz NOT NULL,
	finished_at         timestamptz,
	actor               varchar(400) NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_imports_tenant ON bundle_imports (tenant_id, started_at)
