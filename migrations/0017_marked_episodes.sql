-- Episodes marked by hand outlive a show leaving ephemeral; only the
-- ephemeral rows go with it.
DROP TRIGGER ephemeral_ended_update;
DROP TRIGGER ephemeral_ended_delete;

CREATE TRIGGER ephemeral_ended_update AFTER UPDATE OF status ON item_status
WHEN OLD.status = 'ephemeral' AND NEW.status <> 'ephemeral' BEGIN
    DELETE FROM ephemeral_watchers WHERE rating_key = OLD.rating_key;
    DELETE FROM discord_joins WHERE rating_key = OLD.rating_key;
    UPDATE deletions SET state = 'cancelled', finished_at = unixepoch()
        WHERE parent_key = OLD.rating_key AND kind = 'ephemeral' AND state IN ('scheduled', 'notified');
END;

CREATE TRIGGER ephemeral_ended_delete AFTER DELETE ON item_status
WHEN OLD.status = 'ephemeral' BEGIN
    DELETE FROM ephemeral_watchers WHERE rating_key = OLD.rating_key;
    DELETE FROM discord_joins WHERE rating_key = OLD.rating_key;
    UPDATE deletions SET state = 'cancelled', finished_at = unixepoch()
        WHERE parent_key = OLD.rating_key AND kind = 'ephemeral' AND state IN ('scheduled', 'notified');
END;
