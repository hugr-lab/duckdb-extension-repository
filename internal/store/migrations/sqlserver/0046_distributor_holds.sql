-- +min_reader 46
-- spec 0037 PR 2: tenant reservations (spec 0030 part 2) are removed, and a tenant sets a minimum
-- hold for its distributors' publishers (one day by default), which a distribution admin cannot
-- lower: an entry from a distributor's publisher is held for the larger of the publisher's hold and
-- this one. An older replica reads the dropped table on every channel snapshot and would ignore the
-- minimum, so it no longer runs here.
DROP TABLE tenant_reserved_names
-- +statement
ALTER TABLE tenants ADD distributor_hold_seconds int NOT NULL CONSTRAINT df_tenants_distributor_hold_seconds DEFAULT 86400
