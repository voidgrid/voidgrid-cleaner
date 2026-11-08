-- Where the app sees a path, when it's mounted somewhere other than where
-- Radarr, Sonarr and Plex see it. Empty means the same place.
ALTER TABLE library_paths ADD COLUMN local TEXT NOT NULL DEFAULT '';
