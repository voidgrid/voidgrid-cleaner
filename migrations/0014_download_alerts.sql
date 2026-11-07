-- Whether a finding has been posted to Discord, for stuck downloads.
ALTER TABLE findings ADD COLUMN alerted INTEGER NOT NULL DEFAULT 0;
