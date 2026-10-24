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
ALTER TABLE publishers ADD COLUMN distributor varchar(128) NULL
-- +statement
ALTER TABLE builds ADD COLUMN withdrawn_at timestamptz NULL
-- +statement
ALTER TABLE builds ADD COLUMN withdrawn_reason varchar(400) NULL
-- +statement
CREATE TABLE removed_distributor_publishers (
	tenant_id    varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	publisher_id varchar(36) NOT NULL,
	name         varchar(64) NOT NULL,
	distributor  varchar(128) NOT NULL,
	removed_at   timestamptz NOT NULL,
	removed_by   varchar(400) NOT NULL,
	PRIMARY KEY (tenant_id, publisher_id)
)
-- +statement
CREATE INDEX ix_removed_distributor_publishers_name ON removed_distributor_publishers (tenant_id, name)
-- +statement
CREATE TABLE withdrawn_versions (
	tenant_id       varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type    varchar(32) NOT NULL,
	source          varchar(64) NOT NULL,
	name            varchar(64) NOT NULL,
	package_version varchar(64) NOT NULL,
	reason          varchar(400) NOT NULL,
	withdrawn_by    varchar(400) NOT NULL,
	withdrawn_at    timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, package_type, source, name, package_version)
)
