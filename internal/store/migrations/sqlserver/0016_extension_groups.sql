-- spec 0017: extension groups, their members, and grants on them (apart from grants, so a binary
-- that does not know groups never reads a group grant as a tenant- or channel-wide one)
CREATE TABLE extension_groups (
	id          nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name        nvarchar(63) COLLATE Latin1_General_100_BIN2 NOT NULL,
	description nvarchar(256) COLLATE Latin1_General_100_BIN2 NOT NULL,
	version     bigint NOT NULL,
	created_at  datetime2(6) NOT NULL,
	created_by  nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	CONSTRAINT uq_extension_groups_name UNIQUE (tenant_id, name),
	CONSTRAINT uq_extension_groups_tenant UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE extension_group_members (
	group_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	extension  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	added_at   datetime2(6) NOT NULL,
	added_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	CONSTRAINT pk_extension_group_members PRIMARY KEY (group_id, extension),
	CONSTRAINT fk_extension_group_members_group FOREIGN KEY (group_id, tenant_id) REFERENCES extension_groups (id, tenant_id) ON DELETE CASCADE
)
-- +statement
CREATE INDEX ix_extension_group_members_tenant ON extension_group_members (tenant_id, extension)
-- +statement
CREATE TABLE extension_group_grants (
	id           nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	group_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issuer_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL,
	publisher_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL,
	kind         nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	value        nvarchar(256) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL,
	verbs        nvarchar(100) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at   datetime2(6) NOT NULL,
	created_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	CONSTRAINT ck_extension_group_grants_kind CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher')),
	CONSTRAINT ck_extension_group_grants_principal CHECK ((issuer_id IS NULL AND publisher_id IS NOT NULL AND kind = 'publisher')
		OR (issuer_id IS NOT NULL AND publisher_id IS NULL AND kind <> 'publisher')),
	CONSTRAINT fk_extension_group_grants_group FOREIGN KEY (group_id, tenant_id) REFERENCES extension_groups (id, tenant_id) ON DELETE NO ACTION,
	CONSTRAINT fk_extension_group_grants_issuer FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE CASCADE,
	CONSTRAINT fk_extension_group_grants_publisher FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE,
	CONSTRAINT fk_extension_group_grants_channel FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_extension_group_grants_tenant ON extension_group_grants (tenant_id)
-- +statement
CREATE INDEX ix_extension_group_grants_group ON extension_group_grants (group_id)
-- +statement
CREATE INDEX ix_extension_group_grants_issuer ON extension_group_grants (issuer_id)
-- +statement
CREATE INDEX ix_extension_group_grants_publisher ON extension_group_grants (publisher_id)
