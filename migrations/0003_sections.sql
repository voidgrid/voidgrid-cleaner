-- Plex movie and show libraries as of the last sync, in Plex's order. The
-- library screen has one tab per row.
CREATE TABLE sections (
    section_key TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    type        TEXT NOT NULL CHECK (type IN ('movie', 'show')),
    position    INTEGER NOT NULL
);
