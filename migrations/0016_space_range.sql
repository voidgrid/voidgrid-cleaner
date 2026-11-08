-- Space-based deletions get a range per disk: they start at a level (75% by
-- default, where filesystems slow down) and clear down to a floor (70%). A
-- disk with no row uses the defaults. The old single limit goes.
DELETE FROM settings WHERE key = 'space_limit_percent';

CREATE TABLE disk_ranges (
    path  TEXT PRIMARY KEY,
    start INTEGER NOT NULL CHECK (start BETWEEN 2 AND 99),
    floor INTEGER NOT NULL CHECK (floor >= 1 AND floor < start)
);
