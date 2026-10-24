-- +min_reader 41
-- spec 0033 phase B part 1: distributors' publishers and withdrawals. A publisher a distribution admin
-- or a server administrator marks as a distributor's (distributor: the distributor's name, as a tenant
-- reservation names its holder) publishes into the catalogue only, without grants; its hold and its
-- keys are the distributor's. A removed distributor's publisher is remembered, so its versions can
-- still be withdrawn and read by the distribution admins. A withdrawn entry (withdrawn_at, with the
-- reason) is served nowhere, never placed again, and its body never arrives again in its source. An
-- older replica would serve withdrawn entries and take a distributor's publisher's hold and keys as
-- the tenant's, so it no longer runs here.
ALTER TABLE publishers ADD COLUMN distributor TEXT NULL
-- +statement
ALTER TABLE builds ADD COLUMN withdrawn_at TEXT NULL
-- +statement
ALTER TABLE builds ADD COLUMN withdrawn_reason TEXT NULL
-- +statement
CREATE TABLE removed_distributor_publishers (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	publisher_id TEXT NOT NULL,
	name         TEXT NOT NULL,
	distributor  TEXT NOT NULL,
	removed_at   TEXT NOT NULL,
	removed_by   TEXT NOT NULL,
	PRIMARY KEY (tenant_id, publisher_id)
)
-- +statement
CREATE INDEX ix_removed_distributor_publishers_name ON removed_distributor_publishers (tenant_id, name)
