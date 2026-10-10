-- +min_reader 13
-- spec 0016 phase 1: what a commit claims and what the collector marks; a Build's last use
ALTER TABLE builds ADD COLUMN used_at TEXT
-- +statement
CREATE INDEX ix_builds_body ON builds (body_hash)
-- +statement
UPDATE builds SET used_at = created_at
-- +statement
CREATE INDEX ix_builds_used ON builds (used_at)
-- +statement
CREATE TABLE blob_claims (
	domain      TEXT NOT NULL,
	stream_hash TEXT NOT NULL,
	claim_id    TEXT NOT NULL,
	expires_at  TEXT NOT NULL,
	PRIMARY KEY (domain, stream_hash, claim_id)
)
-- +statement
CREATE TABLE blob_tombstones (
	domain      TEXT NOT NULL,
	stream_hash TEXT NOT NULL,
	since       TEXT NOT NULL,
	state       TEXT NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at TEXT,
	PRIMARY KEY (domain, stream_hash)
)
