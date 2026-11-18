-- Plex labels and collections, copied at sync so the library can show and filter by them.
-- A label's name matches without regard to case, as Plex does.
CREATE TABLE plex_labels (
    section_key TEXT NOT NULL,
    title       TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (section_key, title)
);

CREATE TABLE item_labels (
    rating_key TEXT NOT NULL,
    label      TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (rating_key, label)
);
CREATE INDEX item_labels_label ON item_labels (label);

CREATE TABLE plex_collections (
    key         TEXT PRIMARY KEY,
    section_key TEXT NOT NULL,
    title       TEXT NOT NULL,
    subtype     TEXT NOT NULL DEFAULT '',
    smart       INTEGER NOT NULL DEFAULT 0,
    item_count  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE item_collections (
    rating_key     TEXT NOT NULL,
    collection_key TEXT NOT NULL,
    PRIMARY KEY (rating_key, collection_key)
);
CREATE INDEX item_collections_collection ON item_collections (collection_key);

-- Open pages refresh when any of this changes.
CREATE TRIGGER plex_labels_insert_changed AFTER INSERT ON plex_labels BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER plex_labels_delete_changed AFTER DELETE ON plex_labels BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER item_labels_insert_changed AFTER INSERT ON item_labels BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER item_labels_delete_changed AFTER DELETE ON item_labels BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER plex_collections_insert_changed AFTER INSERT ON plex_collections BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER plex_collections_update_changed AFTER UPDATE ON plex_collections BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER plex_collections_delete_changed AFTER DELETE ON plex_collections BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER item_collections_insert_changed AFTER INSERT ON item_collections BEGIN UPDATE changes SET version = version + 1; END;
CREATE TRIGGER item_collections_delete_changed AFTER DELETE ON item_collections BEGIN UPDATE changes SET version = version + 1; END;
