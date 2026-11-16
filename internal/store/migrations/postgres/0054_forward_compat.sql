-- spec 0046: forward compatibility. A feed's cursor remembers what it passed over because this
-- binary could not read it (a newer statement format, a kind, media type or document version it does
-- not know): the lowest such seq, how many, the last reason, and the reader level of the binary that
-- passed over them, so a binary of a higher level takes them again. An attachment's kinds are
-- validated in the code only (internal/attachment): a row of any kind is read as stored, so a later
-- release that adds a kind needs no migration. An older replica ignores the new columns and inserts a
-- cursor with their defaults: min_reader is unchanged.
ALTER TABLE feed_cursors ADD COLUMN waiting_from bigint NULL
-- +statement
ALTER TABLE feed_cursors ADD COLUMN waiting int NOT NULL DEFAULT 0
-- +statement
ALTER TABLE feed_cursors ADD COLUMN waiting_why varchar(32) NULL
-- +statement
ALTER TABLE feed_cursors ADD COLUMN reader_level int NOT NULL DEFAULT 0
-- +statement
ALTER TABLE attachments DROP CONSTRAINT ck_attachments_kind
