package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNoSession means the session cookie is unknown or expired.
var ErrNoSession = errors.New("storage: no such session")

// User is a signed-in user, keyed by their Seerr user ID.
type User struct {
	SeerrID     int64
	Name        string
	PlexID      sql.NullInt64
	IsAdmin     bool
	AutoApprove bool
	// UnlinkedDiscord counts Discord users waiting for a link; admins only.
	UnlinkedDiscord int
	// OpenFindings counts what the doctor has open; admins only.
	OpenFindings int
	// PendingAsks counts requests waiting for an admin; admins only.
	PendingAsks int
}

// Setting returns the value stored under key, creating it with fallback()
// first if it doesn't exist yet.
func (db *DB) Setting(ctx context.Context, key string, fallback func() string) (string, error) {
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, key, fallback()); err != nil {
		return "", err
	}
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// UpsertUser records u as seen in Seerr now. AutoApprove is this app's own
// flag, so an existing row keeps its value.
func (db *DB) UpsertUser(ctx context.Context, u User) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (seerr_user_id, display_name, plex_id, is_admin, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (seerr_user_id) DO UPDATE SET
			display_name = excluded.display_name, plex_id = excluded.plex_id,
			is_admin = excluded.is_admin, updated_at = excluded.updated_at`,
		u.SeerrID, u.Name, u.PlexID, u.IsAdmin, time.Now().Unix())
	return err
}

// CreateSession starts a session for the user and returns the cookie value.
func (db *DB) CreateSession(ctx context.Context, seerrID int64, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (token_hash, seerr_user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), seerrID, time.Now().Add(ttl).Unix())
	return token, err
}

// SessionUser returns the user a live session belongs to.
func (db *DB) SessionUser(ctx context.Context, token string) (User, error) {
	var u User
	err := db.QueryRowContext(ctx, `
		SELECT u.seerr_user_id, u.display_name, u.plex_id, u.is_admin, u.auto_approve,
			CASE WHEN u.is_admin THEN (SELECT count(*) FROM discord_users WHERE seerr_user_id IS NULL) ELSE 0 END,
			CASE WHEN u.is_admin THEN (SELECT count(*) FROM findings WHERE state = 'open') ELSE 0 END,
			CASE WHEN u.is_admin THEN (SELECT count(*) FROM change_requests WHERE state = 'pending') ELSE 0 END
		FROM sessions s JOIN users u USING (seerr_user_id)
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashToken(token), time.Now().Unix()).
		Scan(&u.SeerrID, &u.Name, &u.PlexID, &u.IsAdmin, &u.AutoApprove, &u.UnlinkedDiscord, &u.OpenFindings, &u.PendingAsks)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	return u, err
}

// DeleteSession ends a session (sign-out). Expired ones are swept too.
func (db *DB) DeleteSession(ctx context.Context, token string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ? OR expires_at <= ?`, hashToken(token), time.Now().Unix())
	return err
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// plexLinks is every Plex account tied to a Seerr user: Seerr's own link or
// one an admin set here.
const plexLinks = `(SELECT plex_id AS plex_user_id, seerr_user_id FROM users WHERE plex_id IS NOT NULL
	UNION SELECT plex_user_id, seerr_user_id FROM plex_accounts)`

// PlexAccount is a Plex account seen in watch history that Seerr doesn't link
// to anyone. SeerrID is the user an admin linked it to, 0 if none.
type PlexAccount struct {
	PlexUserID int64
	Name       string
	Plays      int
	LastPlayed int64
	SeerrID    int64
}

// PlexAccounts lists those accounts, most plays first.
func (db *DB) PlexAccounts(ctx context.Context) ([]PlexAccount, error) {
	// With max(), SQLite takes user_name from the latest play.
	rows, err := db.QueryContext(ctx, `
		SELECT p.plex_user_id, p.user_name, count(*), max(p.stopped), coalesce(a.seerr_user_id, 0)
		FROM plays p LEFT JOIN plex_accounts a ON a.plex_user_id = p.plex_user_id
		WHERE p.plex_user_id NOT IN (SELECT plex_id FROM users WHERE plex_id IS NOT NULL)
		GROUP BY p.plex_user_id ORDER BY count(*) DESC, p.plex_user_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PlexAccount
	for rows.Next() {
		var a PlexAccount
		if err := rows.Scan(&a.PlexUserID, &a.Name, &a.Plays, &a.LastPlayed, &a.SeerrID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetPlexAccount links a Plex account from watch history to a Seerr user, or
// unlinks it when seerrID is 0. found is false if either doesn't exist.
func (db *DB) SetPlexAccount(ctx context.Context, plexUserID, seerrID, by int64) (found bool, err error) {
	if seerrID == 0 {
		_, err = db.ExecContext(ctx, `DELETE FROM plex_accounts WHERE plex_user_id = ?`, plexUserID)
		return err == nil, err
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO plex_accounts (plex_user_id, seerr_user_id, set_by, set_at)
		SELECT ?, seerr_user_id, ?, ? FROM users
		WHERE seerr_user_id = ? AND EXISTS (SELECT 1 FROM plays WHERE plex_user_id = ?)
		ON CONFLICT (plex_user_id) DO UPDATE SET
			seerr_user_id = excluded.seerr_user_id, set_by = excluded.set_by, set_at = excluded.set_at`,
		plexUserID, by, time.Now().Unix(), seerrID, plexUserID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Users returns every synced user by name.
func (db *DB) Users(ctx context.Context) ([]User, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT seerr_user_id, display_name, plex_id, is_admin, auto_approve FROM users ORDER BY display_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.SeerrID, &u.Name, &u.PlexID, &u.IsAdmin, &u.AutoApprove); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DiscordUser is someone who pressed Keep. SeerrID is the user an admin
// linked them to, 0 if none.
type DiscordUser struct {
	DiscordID string
	Name      string
	SeerrID   int64
	FirstSeen int64
}

// DiscordUsers lists them, unlinked first.
func (db *DB) DiscordUsers(ctx context.Context) ([]DiscordUser, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT discord_id, name, coalesce(seerr_user_id, 0), first_seen FROM discord_users
		ORDER BY seerr_user_id IS NOT NULL, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DiscordUser
	for rows.Next() {
		var u DiscordUser
		if err := rows.Scan(&u.DiscordID, &u.Name, &u.SeerrID, &u.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetDiscordUser links a Discord user to a Seerr user, or unlinks them when
// seerrID is 0. found is false if either doesn't exist.
func (db *DB) SetDiscordUser(ctx context.Context, discordID string, seerrID int64) (found bool, err error) {
	if seerrID == 0 {
		return changed(db.ExecContext(ctx, `UPDATE discord_users SET seerr_user_id = NULL WHERE discord_id = ?`, discordID))
	}
	// Add me presses waiting on this link apply with it.
	err = db.tx(ctx, func(tx *sql.Tx) error {
		if found, err = changed(tx.ExecContext(ctx, `UPDATE discord_users SET seerr_user_id = ?
			WHERE discord_id = ? AND EXISTS (SELECT 1 FROM users WHERE seerr_user_id = ?)`, seerrID, discordID, seerrID)); err != nil || !found {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ephemeral_watchers (rating_key, seerr_user_id, added_at)
			SELECT rating_key, ?, ? FROM discord_joins WHERE discord_id = ?`, seerrID, time.Now().Unix(), discordID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM discord_joins WHERE discord_id = ?`, discordID)
		return err
	})
	return found && err == nil, err
}

// ChangeVersion goes up whenever something open pages show has changed.
func (db *DB) ChangeVersion(ctx context.Context) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, `SELECT version FROM changes`).Scan(&v)
	return v, err
}
