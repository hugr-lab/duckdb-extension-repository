-- +min_reader 40
-- spec 0033 phase A2: holds. A publisher, an eager mirror and a channel may have a hold (seconds, 0
-- for none): an entry arriving from a source with a hold is held until its arrival plus the hold
-- (available_at; NULL: available on arrival), and an automatic placement into a channel waits for
-- the channel's hold too; the key a publication was signed with, so revoking it purges the entries it
-- published that are still held, and marks those it keeps (placed early) revoked: placed automatically
-- nowhere, nor promoted (revoked_at); when the placement pass looks at a rule again for a hold ending
-- (due_at). An older replica would place held and revoked entries, and take a held publisher's
-- publications on a channel's route, so it no longer runs here.
ALTER TABLE upstreams ADD hold_seconds int NOT NULL CONSTRAINT df_upstreams_hold_seconds DEFAULT 0
-- +statement
ALTER TABLE publishers ADD hold_seconds int NOT NULL CONSTRAINT df_publishers_hold_seconds DEFAULT 0
-- +statement
ALTER TABLE channels ADD hold_seconds int NOT NULL CONSTRAINT df_channels_hold_seconds DEFAULT 0
-- +statement
ALTER TABLE builds ADD available_at datetime2(6) NULL
-- +statement
ALTER TABLE builds ADD publisher_key_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE builds ADD revoked_at datetime2(6) NULL
-- +statement
ALTER TABLE channel_sources ADD due_at datetime2(6) NULL
