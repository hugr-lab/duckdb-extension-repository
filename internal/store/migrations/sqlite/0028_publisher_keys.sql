-- +min_reader 28
-- spec 0023: publishers authenticate by signed requests with their developers' SSH keys; GitHub
-- trusted publishing and API keys are removed (an older replica would still read them)
DROP TABLE publisher_github
-- +statement
DROP TABLE api_keys
-- +statement
CREATE TABLE publisher_keys (
	id           TEXT PRIMARY KEY,
	tenant_id    TEXT NOT NULL REFERENCES tenants (id),
	publisher_id TEXT NOT NULL,
	type         TEXT NOT NULL,
	public_key   TEXT NOT NULL,
	fingerprint  TEXT NOT NULL,
	title        TEXT NOT NULL,
	expires_at   TEXT,
	created_at   TEXT NOT NULL,
	created_by   TEXT NOT NULL,
	last_used_at TEXT,
	UNIQUE (tenant_id, fingerprint),
	-- the key's tenant is its publisher's
	FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE
)
-- +statement
CREATE INDEX ix_publisher_keys_publisher ON publisher_keys (publisher_id)
-- +statement
ALTER TABLE releases ADD COLUMN publisher_statement TEXT
-- +statement
ALTER TABLE releases ADD COLUMN publisher_signature TEXT
