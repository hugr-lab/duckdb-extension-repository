-- +min_reader 1
CREATE TABLE deployment (
	one        INTEGER PRIMARY KEY CHECK (one = 1),
	id         TEXT NOT NULL,
	created_at TEXT NOT NULL
)
-- +statement
CREATE TABLE storage_domains (
	name       TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	store_id   TEXT NOT NULL,
	created_at TEXT NOT NULL
)
-- +statement
CREATE TABLE blobs (
	domain        TEXT NOT NULL,
	body_hash     TEXT NOT NULL,
	stream_hash   TEXT NOT NULL,
	body_len      INTEGER NOT NULL,
	body_crc32    INTEGER NOT NULL,
	stream_len    INTEGER NOT NULL,
	stream_chunks BLOB NOT NULL,
	corrupt_at    TEXT,
	created_at    TEXT NOT NULL,
	committed_at  TEXT NOT NULL,
	PRIMARY KEY (domain, body_hash)
)
-- +statement
CREATE INDEX ix_blobs_stream ON blobs (domain, stream_hash)
-- +statement
ALTER TABLE tenants ADD COLUMN storage_domain TEXT NOT NULL DEFAULT 'default'
