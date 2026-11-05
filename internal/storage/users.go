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
		SELECT u.seerr_user_id, u.display_name, u.plex_id, u.is_admin, u.auto_approve
		FROM sessions s JOIN users u USING (seerr_user_id)
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashToken(token), time.Now().Unix()).
		Scan(&u.SeerrID, &u.Name, &u.PlexID, &u.IsAdmin, &u.AutoApprove)
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
