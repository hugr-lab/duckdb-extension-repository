-- +min_reader 41
-- spec 0033 phase B part 1: distributors' publishers and withdrawals. A publisher a distribution admin
-- or a server administrator marks as a distributor's (distributor: the distributor's name, as a tenant
-- reservation names its holder) publishes into the catalogue only, without grants; its hold and its
-- keys are the distributor's. A removed distributor's publisher is remembered, so its versions can
-- still be withdrawn and read by the distribution admins. A withdrawn version (withdrawn_versions,
-- with the reason) is final in its source: its entries (withdrawn_at, withdrawn_reason) are served
-- nowhere and never placed again, and no build of the version arrives again in that source. An older
-- replica would serve withdrawn entries and take a distributor's publisher's hold and keys as the
-- tenant's, so it no longer runs here.
ALTER TABLE publishers ADD distributor nvarchar(128) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE builds ADD withdrawn_at datetime2(6) NULL
-- +statement
ALTER TABLE builds ADD withdrawn_reason nvarchar(400) NULL
-- +statement
CREATE TABLE removed_distributor_publishers (
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	publisher_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	distributor  nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL,
	removed_at   datetime2(6) NOT NULL,
	removed_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (tenant_id, publisher_id)
)
-- +statement
CREATE INDEX ix_removed_distributor_publishers_name ON removed_distributor_publishers (tenant_id, name)
-- +statement
CREATE TABLE withdrawn_versions (
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type    nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	source          nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	package_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	reason          nvarchar(400) NOT NULL,
	withdrawn_by    nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	withdrawn_at    datetime2(6) NOT NULL,
	PRIMARY KEY (tenant_id, package_type, source, name, package_version)
)
