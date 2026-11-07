package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DeletionDelay is how long after a confirmed notification a deletion runs.
const DeletionDelay = 24 * time.Hour

// Why a deletion exists.
const (
	KindManual    = "manual"
	KindSpace     = "space"
	KindEphemeral = "ephemeral"
)

// Deletion states.
const (
	DeletionScheduled = "scheduled"
	DeletionNotified  = "notified"
	DeletionExecuting = "executing"
	DeletionDone      = "done"
	DeletionFailed    = "failed"
	DeletionCancelled = "cancelled"
)

// Deletion is one item's way through the pipeline.
type Deletion struct {
	ID          int64
	RatingKey   string
	Title       string
	Type        string
	State       string
	DryRun      bool
	DeleteAfter int64 // unix seconds, 0 until the notification is confirmed
	FinishedAt  int64
	Detail      string
	Kind        string
	ParentKey   string // the show, on an episode's deletion
	Season      int
	Episode     int
	Episodes    int // how many a row from ByShow stands for
}

// ByShow folds a show's ephemeral episodes into one row each for a screen:
// people think of these as the show. The row carries the show's key, so
// actions on it reach them all. Anything else passes through.
func ByShow(ds []Deletion) []Deletion {
	var out []Deletion
	rows := map[[2]string]int{}
	for _, d := range ds {
		if d.Kind != KindEphemeral {
			out = append(out, d)
			continue
		}
		k := [2]string{d.ParentKey, d.State}
		if i, seen := rows[k]; seen {
			out[i].Episodes++
			continue
		}
		rows[k] = len(out)
		d.RatingKey, d.Episodes = d.ParentKey, 1
		out = append(out, d)
	}
	return out
}

const deletionCols = `id, rating_key, title, media_type, state, dry_run, delete_after, finished_at, detail, kind, parent_key, season, episode`

func scanDeletion(sc interface{ Scan(...any) error }) (Deletion, error) {
	var d Deletion
	err := sc.Scan(&d.ID, &d.RatingKey, &d.Title, &d.Type, &d.State, &d.DryRun, &d.DeleteAfter, &d.FinishedAt, &d.Detail, &d.Kind, &d.ParentKey, &d.Season, &d.Episode)
	return d, err
}

func (db *DB) deletions(ctx context.Context, q string, args ...any) ([]Deletion, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Deletion
	for rows.Next() {
		d, err := scanDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func changed(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ScheduleDeletion queues an item. It's false if the item is unknown, set to
// keep, or already on its way.
func (db *DB) ScheduleDeletion(ctx context.Context, ratingKey string, by int64, dryRun bool, kind string) (bool, error) {
	return changed(db.ExecContext(ctx, `
		INSERT INTO deletions (rating_key, title, media_type, kind, dry_run, scheduled_by, created_at)
		SELECT rating_key, title, type, ?, ?, ?, ? FROM media m
		WHERE m.rating_key = ? AND NOT EXISTS (
			SELECT 1 FROM item_status st WHERE st.rating_key = m.rating_key AND st.status = ?)
		ON CONFLICT DO NOTHING`, kind, dryRun, by, time.Now().Unix(), ratingKey, StatusKeep))
}

// SpaceItem is an item the free-space check can act on. Active means a
// deletion is already under way.
type SpaceItem struct {
	Media
	Active bool
}

// SpaceItems lists space-based items in the order they should go, those
// already going first. Only what Radarr or Sonarr manage counts, because only
// they say which disk an item is on. An item whose deletion finished since it
// was marked stays out until it's marked again: a cancel or failure isn't
// retried on its own, and a dry run doesn't repeat. dryRun picks which runs count.
func (db *DB) SpaceItems(ctx context.Context, dryRun bool) ([]SpaceItem, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.rating_key, m.type, m.title, m.tmdb_id, m.tvdb_id, m.size_bytes, d.id IS NOT NULL, `+lastWatched+`
		FROM media m
		JOIN item_status st ON st.rating_key = m.rating_key AND st.status = ?
		JOIN sections s ON s.section_key = m.section_key AND s.ignored = 0
		LEFT JOIN deletions d ON d.rating_key = m.rating_key AND d.state IN (?, ?, ?)
		WHERE m.arr_id IS NOT NULL AND m.size_bytes > 0 AND NOT EXISTS (
			SELECT 1 FROM deletions f WHERE f.rating_key = m.rating_key AND f.dry_run = ? AND f.finished_at > st.set_at)
		ORDER BY d.id IS NULL, `+byOldestWatched,
		StatusSpace, DeletionScheduled, DeletionNotified, DeletionExecuting, dryRun)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SpaceItem
	for rows.Next() {
		var it SpaceItem
		var watched int64
		if err := rows.Scan(&it.RatingKey, &it.Type, &it.Title, &it.TmdbID, &it.TvdbID, &it.SizeBytes, &it.Active, &watched); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CancelDeletion stops a deletion. Once executing it can't be stopped.
func (db *DB) CancelDeletion(ctx context.Context, ratingKey string) (bool, error) {
	return changed(db.ExecContext(ctx, `
		UPDATE deletions SET state = ?, finished_at = ? WHERE (rating_key = ? OR (parent_key = ? AND kind = ?)) AND state IN (?, ?)`,
		DeletionCancelled, time.Now().Unix(), ratingKey, ratingKey, KindEphemeral, DeletionScheduled, DeletionNotified))
}

// PostponeDeletion adds DeletionDelay to a running clock.
func (db *DB) PostponeDeletion(ctx context.Context, ratingKey string) (bool, error) {
	return changed(db.ExecContext(ctx, `UPDATE deletions SET delete_after = delete_after + ? WHERE (rating_key = ? OR (parent_key = ? AND kind = ?)) AND state = ?`,
		int64(DeletionDelay/time.Second), ratingKey, ratingKey, KindEphemeral, DeletionNotified))
}

// UnnotifiedDeletions returns deletions still waiting for their notification.
func (db *DB) UnnotifiedDeletions(ctx context.Context, limit int) ([]Deletion, error) {
	return db.deletions(ctx, `SELECT `+deletionCols+` FROM deletions WHERE state = ? ORDER BY id LIMIT ?`, DeletionScheduled, limit)
}

// UnnotifiedEpisodes returns a show's ephemeral deletions still waiting for
// their notification, which they share.
func (db *DB) UnnotifiedEpisodes(ctx context.Context, showKey string) ([]Deletion, error) {
	return db.deletions(ctx, `SELECT `+deletionCols+` FROM deletions WHERE state = ? AND parent_key = ? AND kind = ? ORDER BY id`,
		DeletionScheduled, showKey, KindEphemeral)
}

// MarkNotified starts the clock on a deletion whose notification was
// confirmed. One cancelled meanwhile is left alone.
func (db *DB) MarkNotified(ctx context.Context, id int64, deleteAfter time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE deletions SET state = ?, delete_after = ? WHERE id = ? AND state = ?`,
		DeletionNotified, deleteAfter.Unix(), id, DeletionScheduled)
	return err
}

// ClaimDueDeletion moves the oldest due deletion to executing, from which it
// can't be cancelled, and returns it. That makes a one-click keep and the
// worker unable to both win. found is false if nothing is due.
func (db *DB) ClaimDueDeletion(ctx context.Context, now time.Time) (d Deletion, found bool, err error) {
	d, err = scanDeletion(db.QueryRowContext(ctx, `
		UPDATE deletions SET state = ?
		WHERE id = (SELECT id FROM deletions WHERE state = ? AND delete_after <= ? ORDER BY delete_after, id LIMIT 1)
		RETURNING `+deletionCols, DeletionExecuting, DeletionNotified, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}

// PushBackDeletion returns an executing deletion to the clock.
func (db *DB) PushBackDeletion(ctx context.Context, id int64, until time.Time, reason string) error {
	_, err := db.ExecContext(ctx, `UPDATE deletions SET state = ?, delete_after = ?, detail = ? WHERE id = ? AND state = ?`,
		DeletionNotified, until.Unix(), reason, id, DeletionExecuting)
	return err
}

// FinishDeletion records the end of an executing deletion.
func (db *DB) FinishDeletion(ctx context.Context, id int64, state, detail string) error {
	_, err := db.ExecContext(ctx, `UPDATE deletions SET state = ?, finished_at = ?, detail = ? WHERE id = ? AND state = ?`,
		state, time.Now().Unix(), detail, id, DeletionExecuting)
	return err
}

// FailInterruptedDeletions marks deletions a restart cut off as failed. How
// far they got is unknown, so they're never resumed by themselves.
func (db *DB) FailInterruptedDeletions(ctx context.Context) error {
	_, err := db.ExecContext(ctx, `UPDATE deletions SET state = ?, finished_at = ?, detail = 'interrupted by a restart; check it by hand' WHERE state = ?`,
		DeletionFailed, time.Now().Unix(), DeletionExecuting)
	return err
}

// DeletionTarget returns what the worker needs: the library item and the
// Seerr media IDs requested for it. found is false if it left the library.
func (db *DB) DeletionTarget(ctx context.Context, ratingKey string) (m Media, seerrMedia []int64, found bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT rating_key, section_key, type, title, tmdb_id, tvdb_id, arr_id FROM media WHERE rating_key = ?`, ratingKey).
		Scan(&m.RatingKey, &m.SectionKey, &m.Type, &m.Title, &m.TmdbID, &m.TvdbID, &m.ArrID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil, false, nil
	}
	if err != nil {
		return m, nil, false, err
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT seerr_media_id FROM requests WHERE rating_key = ? ORDER BY 1`, ratingKey)
	if err != nil {
		return m, nil, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return m, nil, false, err
		}
		seerrMedia = append(seerrMedia, id)
	}
	return m, seerrMedia, true, rows.Err()
}

// visibleDeletions limits a query to what u may see: everything for admins,
// otherwise what they requested.
func visibleDeletions(u User, where string, args []any) (string, []any) {
	q := `SELECT ` + deletionCols + ` FROM deletions d WHERE ` + where
	if !u.IsAdmin {
		q += ` AND EXISTS (SELECT 1 FROM requests r WHERE r.rating_key IN (d.rating_key, d.parent_key) AND r.seerr_user_id = ?)`
		args = append(args, u.SeerrID)
	}
	return q, args
}

// ActiveDeletions lists what's on its way, soonest first and anything still
// waiting for its notification last.
func (db *DB) ActiveDeletions(ctx context.Context, u User) ([]Deletion, error) {
	q, args := visibleDeletions(u, `d.state IN (?, ?, ?)`, []any{DeletionScheduled, DeletionNotified, DeletionExecuting})
	return db.deletions(ctx, q+` ORDER BY d.delete_after = 0, d.delete_after, d.id`, args...)
}

// RecentDeletions lists the latest finished deletions.
func (db *DB) RecentDeletions(ctx context.Context, u User, limit int) ([]Deletion, error) {
	q, args := visibleDeletions(u, `d.state IN (?, ?)`, []any{DeletionDone, DeletionFailed})
	return db.deletions(ctx, q+` ORDER BY d.finished_at DESC, d.id DESC LIMIT ?`, append(args, limit)...)
}

// KeepFromDiscord stops a deletion and sets its item to keep, in one
// transaction so the worker can't claim it in between. An ephemeral notice
// covers a show's waiting episodes, so Keep stops them all, and the show stays
// ephemeral. kept is false if the deletion is no longer waiting, whatever
// happened to it.
func (db *DB) KeepFromDiscord(ctx context.Context, deletionID int64, discordID, name string) (title string, kept bool, err error) {
	err = db.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		var key, parent, kind string
		err := tx.QueryRowContext(ctx, `
			UPDATE deletions SET state = ?, finished_at = ? WHERE id = ? AND state IN (?, ?) RETURNING rating_key, title, parent_key, kind`,
			DeletionCancelled, now, deletionID, DeletionScheduled, DeletionNotified).Scan(&key, &title, &parent, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO discord_users (discord_id, name, first_seen) VALUES (?, ?, ?)
			ON CONFLICT (discord_id) DO UPDATE SET name = excluded.name`, discordID, name, now); err != nil {
			return err
		}
		kept = true
		if kind == KindEphemeral {
			if _, err := tx.ExecContext(ctx, `UPDATE deletions SET state = ?, finished_at = ? WHERE parent_key = ? AND kind = ? AND state IN (?, ?)`,
				DeletionCancelled, now, parent, KindEphemeral, DeletionScheduled, DeletionNotified); err != nil {
				return err
			}
			title += " (these episodes)"
			return nil
		}
		if parent != "" {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO item_status (rating_key, status, set_by, set_at, set_by_discord) VALUES (?, ?, 0, ?, ?)
			ON CONFLICT (rating_key) DO UPDATE SET status = excluded.status, set_by = 0, set_at = excluded.set_at,
				set_by_discord = excluded.set_by_discord`, key, StatusKeep, now, discordID)
		return err
	})
	return title, kept && err == nil, err
}
