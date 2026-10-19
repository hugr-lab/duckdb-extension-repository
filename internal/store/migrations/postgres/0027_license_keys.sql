-- +min_reader 27
-- spec 0021: the licence keys a build declares in its key block, each recorded with the trusted
-- source whose intake carried the body; and the publishers whose publications are such a source
-- (an older replica would let a tenant administrator change a marked publisher's credentials)
CREATE TABLE build_license_keys (
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id),
	build_id    varchar(36) NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
	kid         varchar(71) NOT NULL,
	pem         text NOT NULL,
	source_kind varchar(16) NOT NULL CHECK (source_kind IN ('publisher', 'upstream')),
	source_id   varchar(36) NOT NULL,
	recorded_at timestamptz NOT NULL,
	PRIMARY KEY (build_id, kid, source_kind, source_id)
)
-- +statement
CREATE INDEX ix_build_license_keys_source ON build_license_keys (source_kind, source_id)
-- +statement
CREATE INDEX ix_build_license_keys_tenant ON build_license_keys (tenant_id)
-- +statement
ALTER TABLE publishers ADD COLUMN license_keys boolean NOT NULL DEFAULT false
