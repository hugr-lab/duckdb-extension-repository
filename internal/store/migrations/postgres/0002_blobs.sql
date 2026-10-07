-- +min_reader 1
CREATE TABLE deployment (
	one        int PRIMARY KEY CHECK (one = 1),
	id         varchar(36) NOT NULL,
	created_at timestamptz NOT NULL
)
-- +statement
CREATE TABLE storage_domains (
	name       varchar(16) PRIMARY KEY,
	kind       varchar(16) NOT NULL,
	store_id   varchar(64) NOT NULL,
	created_at timestamptz NOT NULL
)
-- +statement
CREATE TABLE blobs (
	domain        varchar(16) NOT NULL,
	body_hash     varchar(64) NOT NULL,
	stream_hash   varchar(64) NOT NULL,
	body_len      bigint NOT NULL,
	body_crc32    bigint NOT NULL,
	stream_len    bigint NOT NULL,
	stream_chunks bytea NOT NULL,
	corrupt_at    timestamptz,
	created_at    timestamptz NOT NULL,
	committed_at  timestamptz NOT NULL,
	PRIMARY KEY (domain, body_hash)
)
-- +statement
CREATE INDEX ix_blobs_stream ON blobs (domain, stream_hash)
-- +statement
ALTER TABLE tenants ADD COLUMN storage_domain varchar(16) NOT NULL DEFAULT 'default'
