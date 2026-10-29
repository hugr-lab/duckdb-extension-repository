-- +min_reader 47
-- spec 0038: tenant limits. Each tenant may hold its own value of four limits (NULL: the server's
-- default; -1: unlimited, the ceiling for a count; 0 or more: the limit), and a build keeps its body's
-- length, filled here from blobs and set by every insert, so a tenant's own catalogue bytes are one
-- pass over an index. A row an older replica inserts keeps NULL until the collector fills it
-- (FillBodyLen). An older replica would insert builds without body_len and check no limit, so it no
-- longer runs here.
ALTER TABLE tenants ADD COLUMN limit_bytes INTEGER NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_channels INTEGER NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_upstreams INTEGER NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_publishers INTEGER NULL
-- +statement
ALTER TABLE builds ADD COLUMN body_len INTEGER NULL
-- +statement
UPDATE builds SET body_len = (SELECT bl.body_len FROM blobs bl JOIN tenants t ON bl.domain = t.storage_domain
	WHERE t.id = builds.tenant_id AND bl.body_hash = builds.body_hash)
-- +statement
CREATE INDEX ix_builds_tenant_body ON builds (tenant_id, body_hash, body_len, source, landed, withdrawn_at, id)
