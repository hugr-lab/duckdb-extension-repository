-- +min_reader 28
-- spec 0023: publishers authenticate by signed requests with their developers' SSH keys; GitHub
-- trusted publishing and API keys are removed (an older replica would still read them)
DROP TABLE publisher_github
-- +statement
DROP TABLE api_keys
-- +statement
CREATE TABLE publisher_keys (
	id           varchar(36) PRIMARY KEY,
	tenant_id    varchar(36) NOT NULL REFERENCES tenants (id),
	publisher_id varchar(36) NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	type         varchar(32) NOT NULL,
	public_key   varchar(400) NOT NULL,
	fingerprint  varchar(64) NOT NULL,
	title        varchar(100) NOT NULL,
	expires_at   timestamptz,
	created_at   timestamptz NOT NULL,
	created_by   varchar(400) NOT NULL,
	last_used_at timestamptz,
	UNIQUE (tenant_id, fingerprint)
)
-- +statement
CREATE INDEX ix_publisher_keys_publisher ON publisher_keys (publisher_id)
-- +statement
ALTER TABLE releases ADD COLUMN publisher_statement varchar(4096)
-- +statement
ALTER TABLE releases ADD COLUMN publisher_signature varchar(1400)
