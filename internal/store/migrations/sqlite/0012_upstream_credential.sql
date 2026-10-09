-- +min_reader 12
-- spec 0009 phase 3: an upstream's credential, by name (configuration holds the rest); an older
-- replica would make a credentialed upstream's releases public
ALTER TABLE upstreams ADD COLUMN credential TEXT NULL
