package seerr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // the driver, registered as "sqlite"
)

// Seerr's request API runs everything a request sets off: Sonarr or Radarr
// gets the request and re-monitors what it covers, a search starts, and
// notifications go out. Recording a request for something that is already
// there needs none of that, so DB writes the request into Seerr's own
// database, where Seerr reads it back like any other. The two tables are
// media_request and season_request (server/entity/MediaRequest.ts,
// SeasonRequest.ts); the app's own database is never involved.

var (
	// ErrNoMedia means Seerr has no record of the title, which its own scans create.
	ErrNoMedia = errors.New("seerr has no record of this title")
	// ErrHasRequests means Seerr already holds a request on the title.
	ErrHasRequests = errors.New("seerr already holds a request on this title")
)

// Season is one season a recorded request covers.
type Season struct {
	Number    int
	Available bool // its files are there, so it is recorded completed, not approved
}

// DB is Seerr's SQLite database, opened to record requests.
type DB struct{ db *sql.DB }

// OpenDB opens Seerr's database file for writing. It leaves Seerr's journal mode
// alone and waits on its locks, since Seerr is using the file too.
func OpenDB(path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(15000)")
	q.Add("_txlock", "immediate")
	d, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, fmt.Errorf("seerr: open database: %w", err)
	}
	d.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("seerr: open database %s: %w", path, err)
	}
	return &DB{d}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// RecordRequest writes a request for a movie ("movie", no seasons) or a show
// ("tv") on behalf of a user, and returns it. It is recorded completed when
// everything it covers is there and approved otherwise, and it doesn't count
// toward the user's quota. It refuses a title Seerr has no record of, or one
// that already holds a request, and a user Seerr doesn't have.
func (d *DB) RecordRequest(ctx context.Context, mediaType string, tmdbID, userID int64, seasons []Season) (Request, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var mediaID int64
	var tvdb sql.NullInt64
	switch err := tx.QueryRowContext(ctx, `SELECT id, tvdbId FROM media WHERE mediaType = ? AND tmdbId = ?`, mediaType, tmdbID).Scan(&mediaID, &tvdb); {
	case errors.Is(err, sql.ErrNoRows):
		return Request{}, ErrNoMedia
	case err != nil:
		return Request{}, err
	}
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM media_request WHERE mediaId = ?`, mediaID).Scan(&held); err != nil {
		return Request{}, err
	}
	if held > 0 {
		return Request{}, ErrHasRequests
	}
	status := StatusCompleted
	for _, s := range seasons {
		if !s.Available {
			status = StatusApproved
		}
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	res, err := tx.ExecContext(ctx, `INSERT INTO media_request
		(status, createdAt, updatedAt, type, mediaId, requestedById, modifiedById, is4k, isAutoRequest, ignoreQuota)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1)`, status, now, now, mediaType, mediaID, userID, userID)
	if err != nil {
		return Request{}, fmt.Errorf("recording the request (is the user a Seerr user?): %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Request{}, err
	}
	for _, s := range seasons {
		st := StatusApproved
		if s.Available {
			st = StatusCompleted
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO season_request (seasonNumber, status, createdAt, updatedAt, requestId)
			VALUES (?, ?, ?, ?, ?)`, s.Number, st, now, now, id); err != nil {
			return Request{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Request{}, err
	}
	r := Request{ID: id, Status: status}
	r.Media = Media{ID: mediaID, MediaType: mediaType, TmdbID: tmdbID}
	if tvdb.Valid {
		r.Media.TvdbID = &tvdb.Int64
	}
	return r, nil
}
