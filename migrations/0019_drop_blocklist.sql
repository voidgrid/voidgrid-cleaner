-- The blocklist check is gone: blocks come from blocklist and search.
DELETE FROM findings WHERE kind = 'blocklist' AND id NOT IN (SELECT finding_id FROM fix_jobs);
