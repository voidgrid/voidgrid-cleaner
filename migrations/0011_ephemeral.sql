-- Why a deletion exists, and which episode it is if it's one. An episode row's
-- rating_key is the episode's and parent_key the show's.
ALTER TABLE deletions ADD COLUMN kind TEXT NOT NULL DEFAULT 'manual' CHECK (kind IN ('manual', 'space', 'ephemeral'));
ALTER TABLE deletions ADD COLUMN parent_key TEXT NOT NULL DEFAULT '';
ALTER TABLE deletions ADD COLUMN season INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deletions ADD COLUMN episode INTEGER NOT NULL DEFAULT 0;
CREATE INDEX deletions_episode ON deletions (parent_key, season, episode) WHERE parent_key <> '';

ALTER TABLE item_status ADD COLUMN announced INTEGER NOT NULL DEFAULT 0;

-- People who must watch an ephemeral show's episodes, on top of its requesters.
CREATE TABLE ephemeral_watchers (
    rating_key    TEXT NOT NULL,
    seerr_user_id INTEGER NOT NULL,
    added_at      INTEGER NOT NULL,
    PRIMARY KEY (rating_key, seerr_user_id)
);

-- Add me presses from Discord users nobody has linked yet.
CREATE TABLE discord_joins (
    discord_id   TEXT NOT NULL REFERENCES discord_users (discord_id),
    rating_key   TEXT NOT NULL,
    requested_at INTEGER NOT NULL,
    alerted      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (discord_id, rating_key)
);

CREATE TRIGGER ephemeral_watchers_insert_changed AFTER INSERT ON ephemeral_watchers BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER ephemeral_watchers_delete_changed AFTER DELETE ON ephemeral_watchers BEGIN
    UPDATE changes SET version = version + 1;
END;

-- A show stops being ephemeral however that happens: the web, a Discord Keep or a sync.
CREATE TRIGGER ephemeral_ended_update AFTER UPDATE OF status ON item_status
WHEN OLD.status = 'ephemeral' AND NEW.status <> 'ephemeral' BEGIN
    DELETE FROM ephemeral_watchers WHERE rating_key = OLD.rating_key;
    DELETE FROM discord_joins WHERE rating_key = OLD.rating_key;
    UPDATE deletions SET state = 'cancelled', finished_at = unixepoch()
        WHERE parent_key = OLD.rating_key AND state IN ('scheduled', 'notified');
END;

CREATE TRIGGER ephemeral_ended_delete AFTER DELETE ON item_status
WHEN OLD.status = 'ephemeral' BEGIN
    DELETE FROM ephemeral_watchers WHERE rating_key = OLD.rating_key;
    DELETE FROM discord_joins WHERE rating_key = OLD.rating_key;
    UPDATE deletions SET state = 'cancelled', finished_at = unixepoch()
        WHERE parent_key = OLD.rating_key AND state IN ('scheduled', 'notified');
END;
