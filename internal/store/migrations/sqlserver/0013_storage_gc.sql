-- +min_reader 13
-- spec 0016 phase 1: what a commit claims and what the collector marks; a Build's last use
ALTER TABLE builds ADD used_at datetime2(6) NULL
-- +statement
CREATE INDEX ix_builds_body ON builds (body_hash)
-- +statement
UPDATE builds SET used_at = created_at
-- +statement
CREATE INDEX ix_builds_used ON builds (used_at)
-- +statement
CREATE TABLE blob_claims (
	domain      nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	stream_hash nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	claim_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	expires_at  datetime2(6) NOT NULL,
	PRIMARY KEY (domain, stream_hash, claim_id)
)
-- +statement
CREATE TABLE blob_tombstones (
	domain      nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	stream_hash nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	since       datetime2(6) NOT NULL,
	state       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at datetime2(6) NULL,
	PRIMARY KEY (domain, stream_hash)
)
