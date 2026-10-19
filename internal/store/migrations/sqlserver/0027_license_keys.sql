-- +min_reader 27
-- spec 0021: the licence keys a build declares in its key block, each recorded with the trusted
-- source whose intake carried the body; and the publishers whose publications are such a source
-- (an older replica would let a tenant administrator change a marked publisher's credentials)
CREATE TABLE build_license_keys (
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	build_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
	kid         nvarchar(71) COLLATE Latin1_General_100_BIN2 NOT NULL,
	pem         nvarchar(max) NOT NULL,
	source_kind nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (source_kind IN ('publisher', 'upstream')),
	source_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	recorded_at datetime2(6) NOT NULL,
	PRIMARY KEY (build_id, kid, source_kind, source_id)
)
-- +statement
CREATE INDEX ix_build_license_keys_source ON build_license_keys (source_kind, source_id)
-- +statement
CREATE INDEX ix_build_license_keys_tenant ON build_license_keys (tenant_id)
-- +statement
ALTER TABLE publishers ADD license_keys bit NOT NULL CONSTRAINT df_publishers_license_keys DEFAULT 0
