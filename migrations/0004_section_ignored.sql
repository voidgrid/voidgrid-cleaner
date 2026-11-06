-- Set on the libraries page. An ignored library keeps its row but has no tab
-- and no media rows, so nothing in it can be scheduled or deleted.
ALTER TABLE sections ADD COLUMN ignored INTEGER NOT NULL DEFAULT 0;
