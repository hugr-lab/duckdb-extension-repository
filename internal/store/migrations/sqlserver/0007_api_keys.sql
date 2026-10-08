CREATE TABLE api_keys (
	id           nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	publisher_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	prefix       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	hash         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL UNIQUE,
	expires_at   datetime2(6) NOT NULL,
	created_at   datetime2(6) NOT NULL,
	created_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	last_used_at datetime2(6) NULL
)
-- +statement
CREATE INDEX ix_api_keys_publisher ON api_keys (publisher_id)
