-- The delete window now has a default. Reading it used to store empty values,
-- so nobody chose those; dropping them lets the default apply.
DELETE FROM settings WHERE key IN ('delete_window_start', 'delete_window_end') AND value = '';
