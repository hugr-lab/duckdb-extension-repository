CREATE TABLE api_keys (
	id           TEXT PRIMARY KEY,
	publisher_id TEXT NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	prefix       TEXT NOT NULL,
	hash         TEXT NOT NULL UNIQUE,
	expires_at   TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	created_by   TEXT NOT NULL,
	last_used_at TEXT
)
-- +statement
CREATE INDEX ix_api_keys_publisher ON api_keys (publisher_id)
