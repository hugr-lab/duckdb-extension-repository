-- spec 0009 phase 1b: a tenant's replacements made before its first passthrough upstream became shadows
ALTER TABLE tenants ADD shadows_backfilled bit NOT NULL CONSTRAINT df_tenants_shadows_backfilled DEFAULT 0
