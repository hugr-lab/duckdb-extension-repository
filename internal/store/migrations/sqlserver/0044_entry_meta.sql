-- spec 0036 phase 1: a catalogue entry's metadata (tenant, type, source, name): a summary and an
-- icon, a PNG in the tenant's storage domain (icons/<sha256>), with who wrote it last and the last
-- accepted signed write's signed-at and body hash (replay protection). The icons' claims and
-- tombstones are spec 0016's, in tables of their own. Additive: an older replica ignores the tables.
-- The summary's 160 characters are code points: nvarchar counts UTF-16 units, two for a character
-- beyond the Basic Multilingual Plane, so the column takes 320.
CREATE TABLE entry_meta (
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	source       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	summary      nvarchar(320) NULL,
	icon_sha256  nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	icon_width   int NULL,
	icon_height  int NULL,
	icon_bytes   int NULL,
	set_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	set_at       datetime2(6) NOT NULL,
	signed_at    datetime2(6) NULL,
	signed_body  nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	PRIMARY KEY (tenant_id, package_type, source, name)
)
-- +statement
CREATE INDEX ix_entry_meta_icon ON entry_meta (icon_sha256)
-- +statement
CREATE TABLE icon_claims (
	domain     nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	sha256     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	claim_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	expires_at datetime2(6) NOT NULL,
	PRIMARY KEY (domain, sha256, claim_id)
)
-- +statement
CREATE TABLE icon_tombstones (
	domain      nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	sha256      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	since       datetime2(6) NOT NULL,
	state       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at datetime2(6) NULL,
	PRIMARY KEY (domain, sha256)
)
