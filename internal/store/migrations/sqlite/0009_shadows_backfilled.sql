-- spec 0009 phase 1b: a tenant's replacements made before its first passthrough upstream became shadows
ALTER TABLE tenants ADD COLUMN shadows_backfilled INTEGER NOT NULL DEFAULT 0
