-- +min_reader 1
CREATE TABLE deployment (
	one        int PRIMARY KEY CHECK (one = 1),
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at datetime2(6) NOT NULL
)
-- +statement
CREATE TABLE storage_domains (
	name       nvarchar(16) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	kind       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	store_id   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at datetime2(6) NOT NULL
)
-- +statement
CREATE TABLE blobs (
	domain        nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_hash     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	stream_hash   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_len      bigint NOT NULL,
	body_crc32    bigint NOT NULL,
	stream_len    bigint NOT NULL,
	stream_chunks varbinary(max) NOT NULL,
	corrupt_at    datetime2(6),
	created_at    datetime2(6) NOT NULL,
	committed_at  datetime2(6) NOT NULL,
	PRIMARY KEY (domain, body_hash)
)
-- +statement
CREATE INDEX ix_blobs_stream ON blobs (domain, stream_hash)
-- +statement
ALTER TABLE tenants ADD storage_domain nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_tenants_storage_domain DEFAULT N'default'
