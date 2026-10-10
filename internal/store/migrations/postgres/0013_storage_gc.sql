-- +min_reader 13
-- spec 0016 phase 1: what a commit claims and what the collector marks; a Build's last use
ALTER TABLE builds ADD COLUMN used_at timestamptz
-- +statement
CREATE INDEX ix_builds_body ON builds (body_hash)
-- +statement
UPDATE builds SET used_at = created_at
-- +statement
CREATE INDEX ix_builds_used ON builds (used_at)
-- +statement
CREATE TABLE blob_claims (
	domain      varchar(16) NOT NULL,
	stream_hash varchar(64) NOT NULL,
	claim_id    varchar(36) NOT NULL,
	expires_at  timestamptz NOT NULL,
	PRIMARY KEY (domain, stream_hash, claim_id)
)
-- +statement
CREATE TABLE blob_tombstones (
	domain      varchar(16) NOT NULL,
	stream_hash varchar(64) NOT NULL,
	since       timestamptz NOT NULL,
	state       varchar(16) NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at timestamptz,
	PRIMARY KEY (domain, stream_hash)
)
