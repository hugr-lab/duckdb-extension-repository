-- +min_reader 40
-- spec 0033 phase A2: holds. A publisher, an eager mirror and a channel may have a hold (seconds, 0
-- for none): an entry arriving from a source with a hold is held until its arrival plus the hold
-- (available_at; NULL: available on arrival), and an automatic placement into a channel waits for
-- the channel's hold too; the key a publication was signed with, so revoking it purges the entries it
-- published that are still held; when the placement pass looks at a rule again for a hold ending
-- (due_at). An older replica would place held entries, and take a held publisher's publications on
-- a channel's route, so it no longer runs here.
ALTER TABLE upstreams ADD COLUMN hold_seconds INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE publishers ADD COLUMN hold_seconds INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE channels ADD COLUMN hold_seconds INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE builds ADD COLUMN available_at TEXT
-- +statement
ALTER TABLE builds ADD COLUMN publisher_key_id TEXT
-- +statement
ALTER TABLE channel_sources ADD COLUMN due_at TEXT
