-- What users ask for on items they requested, waiting for an admin.
CREATE TABLE change_requests (
    id            INTEGER PRIMARY KEY,
    rating_key    TEXT NOT NULL,
    seerr_user_id INTEGER NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('keep', 'delete')),
    state         TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'denied')),
    created_at    INTEGER NOT NULL,
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER NOT NULL DEFAULT 0,
    note          TEXT NOT NULL DEFAULT ''
);
-- One open ask per person per item: asking again replaces it.
CREATE UNIQUE INDEX change_requests_open ON change_requests (rating_key, seerr_user_id) WHERE state = 'pending';

CREATE TRIGGER change_requests_insert_changed AFTER INSERT ON change_requests BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER change_requests_update_changed AFTER UPDATE ON change_requests BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER users_auto_approve_changed AFTER UPDATE OF auto_approve ON users BEGIN
    UPDATE changes SET version = version + 1;
END;
