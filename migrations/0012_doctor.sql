-- What the doctor found. A finding that stops appearing resolves itself;
-- dismissed ones stay dismissed.
CREATE TABLE findings (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,
    subject    TEXT NOT NULL,
    title      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    severity   TEXT NOT NULL CHECK (severity IN ('problem', 'check', 'note')),
    state      TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'dismissed', 'resolved')),
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    UNIQUE (kind, subject)
);
CREATE INDEX findings_state ON findings (state, kind);

-- Every path a service uses, inferred on each doctor run. Only managed is an
-- admin's choice.
CREATE TABLE library_paths (
    path       TEXT PRIMARY KEY,
    used_by    TEXT NOT NULL,
    media      INTEGER NOT NULL,
    visible    INTEGER NOT NULL,
    writable   INTEGER NOT NULL,
    verified   INTEGER NOT NULL,
    managed    INTEGER NOT NULL DEFAULT 0,
    checked_at INTEGER NOT NULL
);

-- Who decided what. Old rows expire; see the log expiry setting.
CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY,
    at     INTEGER NOT NULL,
    who    TEXT NOT NULL,
    action TEXT NOT NULL,
    item   TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log (at);

CREATE TRIGGER findings_insert_changed AFTER INSERT ON findings BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER findings_state_changed AFTER UPDATE OF state ON findings BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER library_paths_managed_changed AFTER UPDATE OF managed ON library_paths BEGIN
    UPDATE changes SET version = version + 1;
END;

CREATE TRIGGER audit_log_insert_changed AFTER INSERT ON audit_log BEGIN
    UPDATE changes SET version = version + 1;
END;
