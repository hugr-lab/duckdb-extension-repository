-- +min_reader 29
-- spec 0028: the holder a tenant's licences must name (their sub); null, none. An older replica
-- would store a licence of another holder, so it no longer runs on this database.
ALTER TABLE tenants ADD holder_id nvarchar(128) COLLATE Latin1_General_100_BIN2 NULL
