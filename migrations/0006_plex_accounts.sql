-- Plex accounts an admin linked to a Seerr user here, for people Seerr has no
-- Plex ID for. Many accounts can point at one user. Syncs never touch this.
CREATE TABLE plex_accounts (
    plex_user_id  INTEGER PRIMARY KEY,
    seerr_user_id INTEGER NOT NULL REFERENCES users (seerr_user_id) ON DELETE CASCADE,
    set_by        INTEGER NOT NULL,
    set_at        INTEGER NOT NULL
);
