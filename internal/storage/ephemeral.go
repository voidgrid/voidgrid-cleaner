package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// showWatchers is who must watch an ephemeral show: its requesters and
// whoever added themselves.
const showWatchers = `(SELECT rating_key, seerr_user_id FROM requests WHERE rating_key IS NOT NULL
	UNION SELECT rating_key, seerr_user_id FROM ephemeral_watchers)`

// watcherNames selects the names of m's watchers.
const watcherNames = `coalesce((SELECT group_concat(name, ', ') FROM (
		SELECT DISTINCT u.display_name AS name FROM ` + showWatchers + ` w JOIN users u ON u.seerr_user_id = w.seerr_user_id
		WHERE w.rating_key = m.rating_key ORDER BY u.display_name)), '')`

// EphemeralShow is a show in the ephemeral list. Joined means the user
// viewing it is a watcher.
type EphemeralShow struct {
	RatingKey, Title, Watchers string
	Joined                     bool
	// Added means they added themselves, so they can leave; a requester stays on.
	Added bool
}

// EphemeralShows lists every ephemeral show, for anyone signed in: joining
// one you didn't request means being able to see it.
func (db *DB) EphemeralShows(ctx context.Context, u User) ([]EphemeralShow, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.rating_key, m.title, `+watcherNames+`,
			EXISTS (SELECT 1 FROM `+showWatchers+` w WHERE w.rating_key = m.rating_key AND w.seerr_user_id = ?),
			EXISTS (SELECT 1 FROM ephemeral_watchers e WHERE e.rating_key = m.rating_key AND e.seerr_user_id = ?)
				AND NOT EXISTS (SELECT 1 FROM requests r WHERE r.rating_key = m.rating_key AND r.seerr_user_id = ?)
		FROM media m JOIN item_status st ON st.rating_key = m.rating_key AND st.status = ?
		JOIN sections s ON s.section_key = m.section_key AND s.ignored = 0
		ORDER BY m.title COLLATE NOCASE`, u.SeerrID, u.SeerrID, u.SeerrID, StatusEphemeral)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EphemeralShow
	for rows.Next() {
		var e EphemeralShow
		if err := rows.Scan(&e.RatingKey, &e.Title, &e.Watchers, &e.Joined, &e.Added); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func ephemeralTitle(ctx context.Context, tx *sql.Tx, ratingKey string) (string, bool, error) {
	var title string
	err := tx.QueryRowContext(ctx, `SELECT m.title FROM media m JOIN item_status st ON st.rating_key = m.rating_key
		WHERE m.rating_key = ? AND st.status = ?`, ratingKey, StatusEphemeral).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return title, err == nil, err
}

func addWatcher(ctx context.Context, tx *sql.Tx, ratingKey string, seerrID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ephemeral_watchers (rating_key, seerr_user_id, added_at) VALUES (?, ?, ?)`,
		ratingKey, seerrID, time.Now().Unix())
	return err
}

// AddWatcher puts a user on an ephemeral show; false if it isn't one.
func (db *DB) AddWatcher(ctx context.Context, ratingKey string, seerrID int64) (ok bool, err error) {
	err = db.tx(ctx, func(tx *sql.Tx) error {
		if _, ok, err = ephemeralTitle(ctx, tx, ratingKey); err != nil || !ok {
			return err
		}
		return addWatcher(ctx, tx, ratingKey, seerrID)
	})
	return ok && err == nil, err
}

// RemoveWatcher takes a user off a show they added themselves to. Episodes
// stop waiting for them. Requesters can't leave this way.
func (db *DB) RemoveWatcher(ctx context.Context, ratingKey string, seerrID int64) (bool, error) {
	return changed(db.ExecContext(ctx, `DELETE FROM ephemeral_watchers WHERE rating_key = ? AND seerr_user_id = ?`, ratingKey, seerrID))
}

// What became of an Add me press.
const (
	JoinGone    = iota // the show isn't ephemeral any more
	JoinAdded          // they're a watcher now
	JoinPending        // they aren't linked; it applies once an admin links them
)

// JoinFromDiscord handles an Add me press.
func (db *DB) JoinFromDiscord(ctx context.Context, ratingKey, discordID, name string) (title string, result int, err error) {
	err = db.tx(ctx, func(tx *sql.Tx) error {
		var ok bool
		if title, ok, err = ephemeralTitle(ctx, tx, ratingKey); err != nil || !ok {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO discord_users (discord_id, name, first_seen) VALUES (?, ?, ?)
			ON CONFLICT (discord_id) DO UPDATE SET name = excluded.name`, discordID, name, now); err != nil {
			return err
		}
		var seerrID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT seerr_user_id FROM discord_users WHERE discord_id = ?`, discordID).Scan(&seerrID); err != nil {
			return err
		}
		if seerrID.Valid {
			result = JoinAdded
			return addWatcher(ctx, tx, ratingKey, seerrID.Int64)
		}
		result = JoinPending
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO discord_joins (discord_id, rating_key, requested_at) VALUES (?, ?, ?)`,
			discordID, ratingKey, now)
		return err
	})
	return title, result, err
}

// Announcement is an ephemeral show still to be posted to Discord.
type Announcement struct {
	RatingKey, Title, Watchers string
}

// UnannouncedShows lists ephemeral shows not yet posted.
func (db *DB) UnannouncedShows(ctx context.Context, limit int) ([]Announcement, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.rating_key, m.title, `+watcherNames+`
		FROM media m JOIN item_status st ON st.rating_key = m.rating_key
		WHERE st.status = ? AND st.announced = 0 ORDER BY st.set_at LIMIT ?`, StatusEphemeral, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.RatingKey, &a.Title, &a.Watchers); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkAnnounced records that Discord confirmed the post.
func (db *DB) MarkAnnounced(ctx context.Context, ratingKey string) error {
	_, err := db.ExecContext(ctx, `UPDATE item_status SET announced = 1 WHERE rating_key = ?`, ratingKey)
	return err
}

// JoinAlert is an unlinked Add me press an admin hasn't been told about.
type JoinAlert struct {
	DiscordID, Name, RatingKey, Title string
}

// UnalertedJoins lists presses waiting for the admin notice.
func (db *DB) UnalertedJoins(ctx context.Context, limit int) ([]JoinAlert, error) {
	rows, err := db.QueryContext(ctx, `SELECT j.discord_id, du.name, j.rating_key, m.title FROM discord_joins j
		JOIN discord_users du ON du.discord_id = j.discord_id JOIN media m ON m.rating_key = j.rating_key
		WHERE j.alerted = 0 ORDER BY j.requested_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []JoinAlert
	for rows.Next() {
		var a JoinAlert
		if err := rows.Scan(&a.DiscordID, &a.Name, &a.RatingKey, &a.Title); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkAlerted records that the admin notice went out.
func (db *DB) MarkAlerted(ctx context.Context, discordID, ratingKey string) error {
	_, err := db.ExecContext(ctx, `UPDATE discord_joins SET alerted = 1 WHERE discord_id = ? AND rating_key = ?`, discordID, ratingKey)
	return err
}

// WatchedEpisode is an episode of an ephemeral show everyone has watched.
type WatchedEpisode struct {
	ShowKey, RatingKey, Title string
	Season, Episode           int
}

// WatchedEpisodes lists episodes ready to be scheduled: every watcher has
// watched them, on any Plex account linked to them. A show with no watchers
// never qualifies, and neither does an episode that already has a deletion,
// so a cancel isn't retried. dryRun picks which runs count. Only what Sonarr
// manages is listed, because only Sonarr can stop an episode coming back.
func (db *DB) WatchedEpisodes(ctx context.Context, dryRun bool) ([]WatchedEpisode, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.rating_key, max(p.rating_key), m.title, p.season, p.episode
		FROM item_status st
		JOIN media m ON m.rating_key = st.rating_key AND m.arr_id IS NOT NULL
		JOIN sections s ON s.section_key = m.section_key AND s.ignored = 0
		JOIN plays p ON p.show_rating_key = m.rating_key AND p.media_type = 'episode' AND p.watched = 1 AND p.stopped >= m.added_at
		WHERE st.status = ?
			AND EXISTS (SELECT 1 FROM `+showWatchers+` w WHERE w.rating_key = m.rating_key)
			AND NOT EXISTS (SELECT 1 FROM `+showWatchers+` w WHERE w.rating_key = m.rating_key AND NOT EXISTS (
				SELECT 1 FROM plays q JOIN `+plexLinks+` l ON l.plex_user_id = q.plex_user_id
				WHERE l.seerr_user_id = w.seerr_user_id AND q.show_rating_key = m.rating_key
					AND q.season = p.season AND q.episode = p.episode AND q.watched = 1 AND q.stopped >= m.added_at))
			AND NOT EXISTS (SELECT 1 FROM deletions d WHERE d.parent_key = m.rating_key
				AND d.season = p.season AND d.episode = p.episode AND d.dry_run = ?)
		GROUP BY m.rating_key, p.season, p.episode
		ORDER BY m.title COLLATE NOCASE, p.season, p.episode`, StatusEphemeral, dryRun)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WatchedEpisode
	for rows.Next() {
		var e WatchedEpisode
		if err := rows.Scan(&e.ShowKey, &e.RatingKey, &e.Title, &e.Season, &e.Episode); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ScheduleEpisode queues one episode of an ephemeral show, titled as the show.
func (db *DB) ScheduleEpisode(ctx context.Context, e WatchedEpisode, dryRun bool) (bool, error) {
	return changed(db.ExecContext(ctx, `
		INSERT INTO deletions (rating_key, parent_key, season, episode, title, media_type, kind, dry_run, scheduled_by, created_at)
		VALUES (?, ?, ?, ?, ?, 'episode', ?, ?, 0, ?) ON CONFLICT DO NOTHING`,
		e.RatingKey, e.ShowKey, e.Season, e.Episode, e.Title,
		KindEphemeral, dryRun, time.Now().Unix()))
}
