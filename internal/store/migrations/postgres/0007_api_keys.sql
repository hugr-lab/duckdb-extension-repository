CREATE TABLE api_keys (
	id           varchar(36) PRIMARY KEY,
	publisher_id varchar(36) NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	prefix       varchar(16) NOT NULL,
	hash         varchar(64) NOT NULL UNIQUE,
	expires_at   timestamptz NOT NULL,
	created_at   timestamptz NOT NULL,
	created_by   varchar(400) NOT NULL,
	last_used_at timestamptz
)
-- +statement
CREATE INDEX ix_api_keys_publisher ON api_keys (publisher_id)
