package storage

import (
	"context"
	"database/sql"
	"errors"
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

// Section is a Plex movie or show library.
type Section struct {
	Key   string
	Title string
	Type  string // "movie" or "show"
	// Ignored is set on the libraries page and survives syncs.
	Ignored bool
}

// ReplaceLibrary makes the sections table secs (in that order, keeping each
// library's ignored flag) and the media table ms, in one transaction. Items
// of an ignored library are dropped, even if it was ignored mid-sync.
func (db *DB) ReplaceLibrary(ctx context.Context, secs []Section, ms []Media) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sections SET position = -1`); err != nil {
			return err
		}
		for i, sec := range secs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sections (section_key, title, type, position) VALUES (?, ?, ?, ?)
				ON CONFLICT (section_key) DO UPDATE SET title = excluded.title, type = excluded.type, position = excluded.position`,
				sec.Key, sec.Title, sec.Type, i); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sections WHERE position = -1`); err != nil {
			return err
		}
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
		_, err = tx.ExecContext(ctx, `DELETE FROM media WHERE section_key IN (SELECT section_key FROM sections WHERE ignored = 1)`)
		return err
	})
}

// SetSectionIgnored sets a library's ignored flag; found is false if there's
// no such library. Ignoring drops its items at once, not at the next sync.
func (db *DB) SetSectionIgnored(ctx context.Context, key string, ignored bool) (found bool, err error) {
	err = db.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sections SET ignored = ? WHERE section_key = ?`, ignored, key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		found = n > 0
		if ignored {
			_, err = tx.ExecContext(ctx, `DELETE FROM media WHERE section_key = ?`, key)
		}
		return err
	})
	return found, err
}

// Sections returns every library, ignored ones included, in Plex's order.
func (db *DB) Sections(ctx context.Context) ([]Section, error) {
	rows, err := db.QueryContext(ctx, `SELECT section_key, title, type, ignored FROM sections ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Section
	for rows.Next() {
		var sec Section
		if err := rows.Scan(&sec.Key, &sec.Title, &sec.Type, &sec.Ignored); err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
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
	return db.tx(ctx, func(tx *sql.Tx) error { return insertPlays(ctx, tx, ps) })
}

// ReplacePlays makes the plays table exactly ps.
func (db *DB) ReplacePlays(ctx context.Context, ps []Play) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plays`); err != nil {
			return err
		}
		return insertPlays(ctx, tx, ps)
	})
}

func insertPlays(ctx context.Context, tx *sql.Tx, ps []Play) error {
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
}

// LastSyncOK is when source last synced successfully, or zero if never.
func (db *DB) LastSyncOK(ctx context.Context, source string) (time.Time, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT last_ok FROM sync_status WHERE source = ?`, source).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !n.Valid) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(n.Int64, 0), nil
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

// LibraryItem is one row of the library screen.
type LibraryItem struct {
	Media
	Requesters  string // display names, comma-separated; "" if nobody requested it
	LastWatched int64  // unix seconds of the newest play by anyone, 0 if never
}

// owned limits a query to items the user requested (any status). It is
// the server-side ownership rule: non-admins only ever see or act on these.
const owned = `m.rating_key IN (SELECT rating_key FROM requests WHERE seerr_user_id = ? AND rating_key IS NOT NULL)`

// VisibleSections lists the libraries the user may see, in Plex's order:
// every library not ignored for an admin, only those holding something they requested
// otherwise.
func (db *DB) VisibleSections(ctx context.Context, u User) ([]Section, error) {
	q := `SELECT section_key, title, type FROM sections WHERE ignored = 0 ORDER BY position`
	args := []any{}
	if !u.IsAdmin {
		q = `SELECT s.section_key, s.title, s.type FROM sections s
			WHERE s.ignored = 0 AND EXISTS (SELECT 1 FROM media m WHERE m.section_key = s.section_key AND ` + owned + `)
			ORDER BY s.position`
		args = append(args, u.SeerrID)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Section
	for rows.Next() {
		var sec Section
		if err := rows.Scan(&sec.Key, &sec.Title, &sec.Type); err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// LibraryItems lists a library's items the user may see, by title.
func (db *DB) LibraryItems(ctx context.Context, u User, sectionKey string) ([]LibraryItem, error) {
	q := `SELECT m.rating_key, m.section_key, m.type, m.title, m.year, m.thumb, m.added_at, m.tmdb_id, m.tvdb_id, m.arr_id, m.size_bytes,
			coalesce((SELECT group_concat(name, ', ') FROM (
				SELECT DISTINCT us.display_name AS name FROM requests r JOIN users us ON us.seerr_user_id = r.seerr_user_id
				WHERE r.rating_key = m.rating_key ORDER BY us.display_name)), ''),
			coalesce((SELECT max(p.stopped) FROM plays p WHERE p.show_rating_key = m.rating_key), 0)
		FROM media m WHERE m.section_key = ?`
	args := []any{sectionKey}
	if !u.IsAdmin {
		q += ` AND ` + owned
		args = append(args, u.SeerrID)
	}
	q += ` ORDER BY m.title COLLATE NOCASE, m.year`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LibraryItem
	for rows.Next() {
		var it LibraryItem
		m := &it.Media
		if err := rows.Scan(&m.RatingKey, &m.SectionKey, &m.Type, &m.Title, &m.Year, &m.Thumb, &m.AddedAt,
			&m.TmdbID, &m.TvdbID, &m.ArrID, &m.SizeBytes, &it.Requesters, &it.LastWatched); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Thumb returns the Plex thumb path of a library item, "" if unknown.
func (db *DB) Thumb(ctx context.Context, ratingKey string) (string, error) {
	var t string
	err := db.QueryRowContext(ctx, `SELECT thumb FROM media WHERE rating_key = ?`, ratingKey).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return t, err
}
