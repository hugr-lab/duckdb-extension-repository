-- +min_reader 5
ALTER TABLE builds ADD checked bit NOT NULL CONSTRAINT df_builds_checked DEFAULT 0
-- +statement
ALTER TABLE releases ADD origin nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_releases_origin DEFAULT 'admin'
-- +statement
ALTER TABLE releases ADD provenance nvarchar(4000) NULL
-- +statement
CREATE TABLE blocks (
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	reason     nvarchar(400) NOT NULL,
	created_at datetime2(6) NOT NULL,
	created_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (tenant_id, body_hash)
)
