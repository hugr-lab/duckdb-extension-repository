-- +min_reader 28
-- spec 0023: publishers authenticate by signed requests with their developers' SSH keys; GitHub
-- trusted publishing and API keys are removed (an older replica would still read them)
DROP TABLE publisher_github
-- +statement
DROP TABLE api_keys
-- +statement
CREATE TABLE publisher_keys (
	id           nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	publisher_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	type         nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	public_key   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	fingerprint  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	title        nvarchar(100) NOT NULL,
	expires_at   datetime2(6) NULL,
	created_at   datetime2(6) NOT NULL,
	created_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	last_used_at datetime2(6) NULL,
	CONSTRAINT uq_publisher_keys_fingerprint UNIQUE (tenant_id, fingerprint),
	-- the key's tenant is its publisher's
	CONSTRAINT fk_publisher_keys_publisher FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE
)
-- +statement
CREATE INDEX ix_publisher_keys_publisher ON publisher_keys (publisher_id)
-- +statement
ALTER TABLE releases ADD publisher_statement varchar(4096) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE releases ADD publisher_signature varchar(1400) COLLATE Latin1_General_100_BIN2 NULL
