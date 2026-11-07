-- A counter open pages poll: anything that changes what they show bumps it,
-- whoever made the change (the web app, the worker, Discord, a sync).
CREATE TABLE changes (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);
INSERT INTO changes (id, version) VALUES (1, 0);

CREATE TRIGGER item_status_insert_changed AFTER INSERT ON item_status BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER item_status_update_changed AFTER UPDATE ON item_status BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER item_status_delete_changed AFTER DELETE ON item_status BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER deletions_insert_changed AFTER INSERT ON deletions BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER deletions_update_changed AFTER UPDATE ON deletions BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER deletions_delete_changed AFTER DELETE ON deletions BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER discord_users_insert_changed AFTER INSERT ON discord_users BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER discord_users_update_changed AFTER UPDATE ON discord_users BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER discord_users_delete_changed AFTER DELETE ON discord_users BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER sync_status_insert_changed AFTER INSERT ON sync_status BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER sync_status_update_changed AFTER UPDATE ON sync_status BEGIN
    UPDATE changes SET version = version + 1;
END;
