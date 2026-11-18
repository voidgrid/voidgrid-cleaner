package storage

import (
	"context"
	"database/sql"
	"errors"
)

// NoRequesterItem is a movie or show in the library that no Seerr request matched.
type NoRequesterItem struct {
	RatingKey string
	Type      string // movie or show
	Title     string
	Year      int
	TmdbID    sql.NullInt64
}

// ErrNoRequests means no request has been synced yet, so "no requester" would
// be true of every show and means nothing.
var ErrNoRequests = errors.New("no requests synced yet")

// ItemsWithoutRequester lists movies and shows nobody requested in Seerr. It fails with
// ErrNoRequests while the requests table is empty.
func (db *DB) ItemsWithoutRequester(ctx context.Context) ([]NoRequesterItem, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNoRequests
	}
	rows, err := db.QueryContext(ctx, `SELECT m.rating_key, m.type, m.title, m.year, m.tmdb_id FROM media m
		JOIN sections s ON s.section_key = m.section_key
		WHERE s.ignored = 0
		AND NOT EXISTS (SELECT 1 FROM requests r WHERE r.rating_key = m.rating_key)
		ORDER BY m.type, m.title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NoRequesterItem
	for rows.Next() {
		var s NoRequesterItem
		if err := rows.Scan(&s.RatingKey, &s.Type, &s.Title, &s.Year, &s.TmdbID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MediaIDs returns an item's type and its TMDB and TVDB IDs (0 where it has
// none); found is false if the library has no such item.
func (db *DB) MediaIDs(ctx context.Context, ratingKey string) (typ string, tmdb, tvdb int64, found bool, err error) {
	var a, b sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT type, tmdb_id, tvdb_id FROM media WHERE rating_key = ?`, ratingKey).Scan(&typ, &a, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, false, nil
	}
	return typ, a.Int64, b.Int64, err == nil, err
}

// AddRequest stores one request the app just made, so the screens show it
// before the next sync reads it back from Seerr.
func (db *DB) AddRequest(ctx context.Context, r Request) error {
	_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO requests
		(seerr_request_id, seerr_media_id, seerr_user_id, media_type, tmdb_id, tvdb_id, status, created_at, rating_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.SeerrRequestID, r.SeerrMediaID, r.SeerrUserID, r.MediaType, r.TmdbID, r.TvdbID, r.Status, r.CreatedAt, r.RatingKey)
	return err
}
