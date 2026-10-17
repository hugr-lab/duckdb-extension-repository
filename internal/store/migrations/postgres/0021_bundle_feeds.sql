-- +min_reader 21
-- spec 0014 phase 1b: bundle-mode feeds, and what an export feed keeps for a site's plan; an older
-- replica would run a bundle-mode feed online, and would not keep what an export feed keeps
ALTER TABLE feeds ADD COLUMN mode varchar(16) NOT NULL DEFAULT 'online' CHECK (mode IN ('online', 'bundle'))
-- +statement
ALTER TABLE feeds ADD COLUMN export boolean NOT NULL DEFAULT false
-- +statement
-- each change of export: a run started before it writes no cursor and keeps nothing
ALTER TABLE feeds ADD COLUMN export_epoch bigint NOT NULL DEFAULT 0
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_id varchar(36)
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_created_at timestamptz
-- +statement
ALTER TABLE feeds ADD COLUMN bundle_manifest_sha256 varchar(64)
-- +statement
ALTER TABLE feeds ADD COLUMN attestation_keys text
-- +statement
ALTER TABLE feeds ADD COLUMN attestation_keys_at timestamptz
-- +statement
CREATE TABLE feed_refusals (
	feed_id    varchar(36) NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash  varchar(64) NOT NULL,
	seq        bigint NOT NULL,
	reason     varchar(32) NOT NULL,
	statement  text NOT NULL,
	signature  varchar(512) NOT NULL,
	refused_at timestamptz NOT NULL,
	PRIMARY KEY (feed_id, body_hash, seq)
)
-- +statement
CREATE TABLE feed_kept (
	tenant_id varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	issuer    varchar(400) NOT NULL,
	issuer_id varchar(64) NOT NULL,
	body_hash varchar(64) NOT NULL,
	seq       bigint NOT NULL,
	statement text NOT NULL,
	signature varchar(512) NOT NULL,
	kept_at   timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_feed_kept_body ON feed_kept (tenant_id, issuer, body_hash, seq)
