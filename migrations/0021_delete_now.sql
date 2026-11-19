-- An admin's "now" skips the notice, the wait and the delete window for that row.
ALTER TABLE deletions ADD COLUMN immediate INTEGER NOT NULL DEFAULT 0;
