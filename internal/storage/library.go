package storage

import (
	"context"
	"database/sql"
	"time"
)

// Media is a movie or show in Plex.
type Media struct {
	RatingKey  string
	SectionKey string
	Type       string // "movie" or "show"
	Title      string
	Year       int
	Thumb      string
	AddedAt    int64
	TmdbID     sql.NullInt64
	TvdbID     sql.NullInt64
	ArrID      sql.NullInt64 // Radarr movie or Sonarr series
	SizeBytes  int64
}

// Request is a Seerr request. RatingKey is empty when it matched nothing in Plex.
type Request struct {
	SeerrRequestID int64
	SeerrMediaID   int64
	SeerrUserID    int64
	MediaType      string
	TmdbID         int64
	TvdbID         sql.NullInt64
	Status         int
	CreatedAt      int64
	RatingKey      sql.NullString
}

// Play is one Tautulli play.
type Play struct {
	ReferenceID   int64
	PlexUserID    int64
	UserName      string
	MediaType     string
	RatingKey     string
	ShowRatingKey string
	Season        int
	Episode       int
	Percent       int
	Watched       bool
	Stopped       int64
}

// SyncStatus is how the last sync of one source went.
type SyncStatus struct {
	Source    string
	LastRun   int64
	LastOK    sql.NullInt64
	LastError string
	Items     int
}

// ReplaceMedia makes the media table exactly ms.
func (db *DB) ReplaceMedia(ctx context.Context, ms []Media) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media`); err != nil {
			return err
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO media
			(rating_key, section_key, type, title, year, thumb, added_at, tmdb_id, tvdb_id, arr_id, size_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		for _, m := range ms {
			if _, err := st.ExecContext(ctx, m.RatingKey, m.SectionKey, m.Type, m.Title, m.Year, m.Thumb, m.AddedAt,
				m.TmdbID, m.TvdbID, m.ArrID, m.SizeBytes); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceRequests makes the requests table exactly rs.
func (db *DB) ReplaceRequests(ctx context.Context, rs []Request) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM requests`); err != nil {
			return err
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO requests
			(seerr_request_id, seerr_media_id, seerr_user_id, media_type, tmdb_id, tvdb_id, status, created_at, rating_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		for _, r := range rs {
			if _, err := st.ExecContext(ctx, r.SeerrRequestID, r.SeerrMediaID, r.SeerrUserID, r.MediaType, r.TmdbID,
				r.TvdbID, r.Status, r.CreatedAt, r.RatingKey); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertPlays adds plays, replacing any with the same reference ID (a
// grouped play grows when it's resumed).
func (db *DB) UpsertPlays(ctx context.Context, ps []Play) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO plays
			(reference_id, plex_user_id, user_name, media_type, rating_key, show_rating_key, season, episode, percent, watched, stopped)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		for _, p := range ps {
			if _, err := st.ExecContext(ctx, p.ReferenceID, p.PlexUserID, p.UserName, p.MediaType, p.RatingKey,
				p.ShowRatingKey, p.Season, p.Episode, p.Percent, p.Watched, p.Stopped); err != nil {
				return err
			}
		}
		return nil
	})
}

// LastPlayStopped is when the newest stored play ended, or zero if none.
func (db *DB) LastPlayStopped(ctx context.Context) (time.Time, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT max(stopped) FROM plays`).Scan(&n)
	if err != nil || !n.Valid {
		return time.Time{}, err
	}
	return time.Unix(n.Int64, 0), nil
}

// RecordSync stores the outcome of syncing source; err nil means success.
func (db *DB) RecordSync(ctx context.Context, source string, items int, syncErr error) error {
	now := time.Now().Unix()
	if syncErr == nil {
		_, err := db.ExecContext(ctx, `INSERT INTO sync_status (source, last_run, last_ok, last_error, items) VALUES (?, ?, ?, '', ?)
			ON CONFLICT (source) DO UPDATE SET last_run = excluded.last_run, last_ok = excluded.last_ok, last_error = '', items = excluded.items`,
			source, now, now, items)
		return err
	}
	// A failure keeps the last good time and count: the old data is still there.
	_, err := db.ExecContext(ctx, `INSERT INTO sync_status (source, last_run, last_error) VALUES (?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET last_run = excluded.last_run, last_error = excluded.last_error`,
		source, now, syncErr.Error())
	return err
}

// SyncStatuses returns every source's last sync, by name.
func (db *DB) SyncStatuses(ctx context.Context) ([]SyncStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT source, last_run, last_ok, last_error, items FROM sync_status ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SyncStatus
	for rows.Next() {
		var s SyncStatus
		if err := rows.Scan(&s.Source, &s.LastRun, &s.LastOK, &s.LastError, &s.Items); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Unmatched is one thing the sync couldn't link, for the sync page.
type Unmatched struct {
	Kind string // e.g. "plex item without Radarr/Sonarr"
	Name string
}

// SyncReport lists what didn't match, at most limit of each kind.
func (db *DB) SyncReport(ctx context.Context, limit int) (counts map[string]int, items []Unmatched, err error) {
	queries := []struct{ kind, count, list string }{
		{
			"Plex item not in Radarr/Sonarr",
			`SELECT count(*) FROM media WHERE arr_id IS NULL`,
			`SELECT type || ': ' || title || ' (' || year || ')' FROM media WHERE arr_id IS NULL ORDER BY title LIMIT ?`,
		},
		{
			"Seerr request not in Plex",
			`SELECT count(*) FROM requests WHERE rating_key IS NULL`,
			`SELECT media_type || ' tmdb ' || tmdb_id || ', request ' || seerr_request_id FROM requests WHERE rating_key IS NULL ORDER BY seerr_request_id LIMIT ?`,
		},
		{
			"Tautulli user not linked to a Seerr user",
			`SELECT count(DISTINCT plex_user_id) FROM plays WHERE plex_user_id NOT IN (SELECT plex_id FROM users WHERE plex_id IS NOT NULL)`,
			`SELECT DISTINCT user_name FROM plays WHERE plex_user_id NOT IN (SELECT plex_id FROM users WHERE plex_id IS NOT NULL) ORDER BY user_name LIMIT ?`,
		},
	}
	counts = map[string]int{}
	for _, q := range queries {
		var n int
		if err := db.QueryRowContext(ctx, q.count).Scan(&n); err != nil {
			return nil, nil, err
		}
		counts[q.kind] = n
		rows, err := db.QueryContext(ctx, q.list, limit)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			items = append(items, Unmatched{Kind: q.kind, Name: name})
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
		}
	}
	return counts, items, nil
}
