-- +min_reader 52
-- spec 0043: the channels' order, for display only: a signed channel may name its next channel (a
-- strict chain: a channel is the next of at most one, the unique key). A table rather than a column on
-- channels, so the reaper deletes it by channel before channels. An older replica's tenant reaper
-- does not know the table, which references channels, so it no longer runs here.
CREATE TABLE channel_next (
	channel_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES channels (id),
	next_channel_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channels (id),
	set_by          nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	set_at          datetime2(6) NOT NULL
)
-- +statement
CREATE UNIQUE INDEX ux_channel_next_next ON channel_next (next_channel_id)
