-- Deletions: scheduled -> notified -> executing -> done | failed, or
-- cancelled before executing. The title is a copy: the media row goes away
-- with the item. delete_after is 0 until the notification is confirmed, so
-- the 24 hours can't start without it. A row made in dry-run stays dry-run.
CREATE TABLE deletions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    rating_key   TEXT NOT NULL,
    title        TEXT NOT NULL,
    media_type   TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'scheduled'
                 CHECK (state IN ('scheduled', 'notified', 'executing', 'done', 'failed', 'cancelled')),
    dry_run      INTEGER NOT NULL,
    scheduled_by INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    delete_after INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    detail       TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX deletions_active ON deletions (rating_key) WHERE state IN ('scheduled', 'notified', 'executing');
CREATE INDEX deletions_state ON deletions (state, delete_after);
