-- +min_reader 21
-- spec 0014 phase 1b: bundle-mode feeds, and what an export feed keeps for a site's plan; an older
-- replica would run a bundle-mode feed online, and would not keep what an export feed keeps
ALTER TABLE feeds ADD mode nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_feeds_mode DEFAULT 'online'
	CONSTRAINT ck_feeds_mode CHECK (mode IN ('online', 'bundle'))
-- +statement
ALTER TABLE feeds ADD export bit NOT NULL CONSTRAINT df_feeds_export DEFAULT 0
-- +statement
-- each change of export: a run started before it writes no cursor and keeps nothing
ALTER TABLE feeds ADD export_epoch bigint NOT NULL CONSTRAINT df_feeds_export_epoch DEFAULT 0
-- +statement
ALTER TABLE feeds ADD bundle_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE feeds ADD bundle_created_at datetime2(6) NULL
-- +statement
ALTER TABLE feeds ADD bundle_manifest_sha256 nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE feeds ADD attestation_keys nvarchar(max) NULL
-- +statement
ALTER TABLE feeds ADD attestation_keys_at datetime2(6) NULL
-- +statement
CREATE TABLE feed_refusals (
	feed_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	seq        bigint NOT NULL,
	reason     nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	statement  nvarchar(max) NOT NULL,
	signature  nvarchar(512) COLLATE Latin1_General_100_BIN2 NOT NULL,
	refused_at datetime2(6) NOT NULL,
	PRIMARY KEY (feed_id, body_hash, seq)
)
-- +statement
CREATE TABLE feed_kept (
	tenant_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	issuer    nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issuer_id nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_hash nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	seq       bigint NOT NULL,
	statement nvarchar(max) NOT NULL,
	signature nvarchar(512) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kept_at   datetime2(6) NOT NULL,
	-- nonclustered: over the 900 bytes a clustered key takes, as attachments' issuer key
	PRIMARY KEY NONCLUSTERED (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_feed_kept_body ON feed_kept (tenant_id, issuer, body_hash, seq)
