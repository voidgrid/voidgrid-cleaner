-- What the sync job copies from the other services. Each table is a cache:
-- rebuilt from its source on every sync, so nothing here is the only copy.

-- Movies and shows in Plex, matched to Radarr (movies, by TMDB ID) or
-- Sonarr (shows, by TVDB ID). arr_id is NULL when there's no match.
CREATE TABLE media (
    rating_key  TEXT PRIMARY KEY,
    section_key TEXT NOT NULL,
    type        TEXT NOT NULL CHECK (type IN ('movie', 'show')),
    title       TEXT NOT NULL,
    year        INTEGER NOT NULL,
    thumb       TEXT NOT NULL,
    added_at    INTEGER NOT NULL,
    tmdb_id     INTEGER,
    tvdb_id     INTEGER,
    arr_id      INTEGER,
    size_bytes  INTEGER NOT NULL DEFAULT 0
);

-- Seerr requests. rating_key is the Plex item they matched, NULL if the
-- title isn't in Plex.
CREATE TABLE requests (
    seerr_request_id INTEGER PRIMARY KEY,
    seerr_media_id   INTEGER NOT NULL,
    seerr_user_id    INTEGER NOT NULL,
    media_type       TEXT NOT NULL,
    tmdb_id          INTEGER NOT NULL,
    tvdb_id          INTEGER,
    status           INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    rating_key       TEXT
);
CREATE INDEX requests_rating_key ON requests (rating_key);

-- Tautulli plays (resumed sessions grouped). show_rating_key is the show for
-- an episode and the movie itself for a movie. plex_user_id is the plex.tv
-- account ID, the same as users.plex_id.
CREATE TABLE plays (
    reference_id    INTEGER PRIMARY KEY,
    plex_user_id    INTEGER NOT NULL,
    user_name       TEXT NOT NULL,
    media_type      TEXT NOT NULL,
    rating_key      TEXT NOT NULL,
    show_rating_key TEXT NOT NULL,
    season          INTEGER NOT NULL,
    episode         INTEGER NOT NULL,
    percent         INTEGER NOT NULL,
    watched         INTEGER NOT NULL,
    stopped         INTEGER NOT NULL
);
CREATE INDEX plays_show ON plays (show_rating_key, plex_user_id);

-- Outcome of the last sync of each source.
CREATE TABLE sync_status (
    source     TEXT PRIMARY KEY,
    last_run   INTEGER NOT NULL,
    last_ok    INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    items      INTEGER NOT NULL DEFAULT 0
);
