-- +min_reader 21
-- spec 0014 phase 1b: bundle-mode feeds, and what an export feed keeps for a site's plan; an older
-- replica would run a bundle-mode feed online, and would not keep what an export feed keeps
ALTER TABLE feeds ADD COLUMN mode TEXT NOT NULL DEFAULT 'online' CHECK (mode IN ('online', 'bundle'))
-- +statement
ALTER TABLE feeds ADD COLUMN export INTEGER NOT NULL DEFAULT 0
-- +statement
-- each change of export: a run started before it writes no cursor and keeps nothing
ALTER TABLE feeds ADD COLUMN export_epoch INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_id TEXT
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_created_at TEXT
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_manifest_sha256 TEXT
-- +statement
ALTER TABLE feeds ADD COLUMN attestation_keys TEXT
-- +statement
ALTER TABLE feeds ADD COLUMN attestation_keys_at TEXT
-- +statement
CREATE TABLE feed_refusals (
	feed_id    TEXT NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash  TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	reason     TEXT NOT NULL,
	statement  TEXT NOT NULL,
	signature  TEXT NOT NULL,
	refused_at TEXT NOT NULL,
	PRIMARY KEY (feed_id, body_hash, seq)
)
-- +statement
CREATE TABLE feed_kept (
	tenant_id TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	issuer    TEXT NOT NULL,
	issuer_id TEXT NOT NULL,
	body_hash TEXT NOT NULL,
	seq       INTEGER NOT NULL,
	statement TEXT NOT NULL,
	signature TEXT NOT NULL,
	kept_at   TEXT NOT NULL,
	PRIMARY KEY (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_feed_kept_body ON feed_kept (tenant_id, issuer, body_hash, seq)
