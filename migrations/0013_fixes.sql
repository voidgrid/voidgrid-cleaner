-- What a fix needs to know about a finding, as JSON.
ALTER TABLE findings ADD COLUMN data TEXT NOT NULL DEFAULT '{}';

-- Fixes approved on the doctor page, run one at a time in the background.
CREATE TABLE fix_jobs (
    id          INTEGER PRIMARY KEY,
    finding_id  INTEGER NOT NULL REFERENCES findings (id),
    choice      TEXT NOT NULL DEFAULT '',
    who         TEXT NOT NULL,
    state       TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed', 'skipped')),
    detail      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX fix_jobs_active ON fix_jobs (finding_id) WHERE state IN ('queued', 'running');

CREATE TRIGGER fix_jobs_insert_changed AFTER INSERT ON fix_jobs BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER fix_jobs_state_changed AFTER UPDATE OF state ON fix_jobs BEGIN
    UPDATE changes SET version = version + 1;
END;
