-- +min_reader 10
CREATE TABLE events (
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	at         datetime2(6) NOT NULL,
	kind       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	v          int NOT NULL,
	outcome    nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	actor      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	actor_name nvarchar(256) COLLATE Latin1_General_100_BIN2 NULL,
	subject    nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	data       nvarchar(max) NOT NULL,
	client     nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	request    nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	pending    int NOT NULL
)
-- +statement
CREATE INDEX ix_events_tenant_at ON events (tenant_id, at, id)
-- +statement
CREATE INDEX ix_events_tenant_kind ON events (tenant_id, kind, at)
-- +statement
CREATE INDEX ix_events_tenant_subject ON events (tenant_id, subject, at)
-- +statement
CREATE INDEX ix_events_pending ON events (at, id) WHERE pending <> 0
-- +statement
CREATE TABLE event_sinks (
	name           nvarchar(64) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	bit            int NOT NULL UNIQUE,
	added_at       datetime2(6) NOT NULL,
	last_listed_at datetime2(6) NOT NULL
)
