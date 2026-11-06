-- Decisions about items, the one table here that is not a cache: syncs never
-- touch it. No row means the status is normal. Keyed by Plex rating key, so an
-- item that returns to Plex under a new key starts as normal again.
CREATE TABLE item_status (
    rating_key TEXT PRIMARY KEY,
    status     TEXT NOT NULL CHECK (status IN ('keep', 'space_based', 'ephemeral')),
    set_by     INTEGER NOT NULL,
    set_at     INTEGER NOT NULL
);
