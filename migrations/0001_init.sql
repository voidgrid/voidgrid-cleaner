-- Key/value settings the app generates or the admin sets (e.g. the Plex
-- client identifier used for sign-in).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Everyone who has signed in, keyed by their Seerr user. is_admin mirrors
-- Seerr's admin permission and is refreshed at each sign-in; auto_approve
-- is this app's own flag.
CREATE TABLE users (
    seerr_user_id INTEGER PRIMARY KEY,
    display_name  TEXT NOT NULL,
    plex_id       INTEGER,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    auto_approve  INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL
);

-- Web sessions. Only a SHA-256 of the cookie value is stored.
CREATE TABLE sessions (
    token_hash    TEXT PRIMARY KEY,
    seerr_user_id INTEGER NOT NULL REFERENCES users (seerr_user_id) ON DELETE CASCADE,
    expires_at    INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions (expires_at);
