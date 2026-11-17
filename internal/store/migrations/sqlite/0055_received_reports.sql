-- +min_reader 55
-- spec 0045: received reports. A feed may be taken through a relay: via is the relaying tenant's
-- URL, set at creation and never changed; the feed's url stays its issuer's identity and is then never
-- contacted. An older replica does not know the column and would run such a feed against the
-- issuer's URL, so it no longer runs here. What an export feed keeps of a refusal gains the
-- document's id at its issuer: a kept refusal replaces the earlier kept row of the same document (a
-- row kept before has none and is replaced by none).
ALTER TABLE feeds ADD COLUMN via TEXT NULL
-- +statement
ALTER TABLE feed_refusals ADD COLUMN issuer_id TEXT NULL
-- +statement
CREATE INDEX ix_feed_refusals_issuer ON feed_refusals (feed_id, issuer_id)
