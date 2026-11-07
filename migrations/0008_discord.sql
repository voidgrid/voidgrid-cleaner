-- Discord users who pressed Keep. No mapping is needed for the keep to work;
-- an admin links them to a Seerr user afterwards, for attribution.
CREATE TABLE discord_users (
    discord_id    TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    seerr_user_id INTEGER REFERENCES users (seerr_user_id) ON DELETE SET NULL,
    first_seen    INTEGER NOT NULL
);

-- Who set a status from Discord; NULL when it was set in the app.
ALTER TABLE item_status ADD COLUMN set_by_discord TEXT REFERENCES discord_users (discord_id);
