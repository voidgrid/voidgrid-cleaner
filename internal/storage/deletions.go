package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DeletionDelay is how long after a confirmed notification a deletion runs.
const DeletionDelay = 24 * time.Hour

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
}

const deletionCols = `id, rating_key, title, media_type, state, dry_run, delete_after, finished_at, detail`

func scanDeletion(sc interface{ Scan(...any) error }) (Deletion, error) {
	var d Deletion
	err := sc.Scan(&d.ID, &d.RatingKey, &d.Title, &d.Type, &d.State, &d.DryRun, &d.DeleteAfter, &d.FinishedAt, &d.Detail)
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
func (db *DB) ScheduleDeletion(ctx context.Context, ratingKey string, by int64, dryRun bool) (bool, error) {
	return changed(db.ExecContext(ctx, `
		INSERT INTO deletions (rating_key, title, media_type, dry_run, scheduled_by, created_at)
		SELECT rating_key, title, type, ?, ?, ? FROM media m
		WHERE m.rating_key = ? AND NOT EXISTS (
			SELECT 1 FROM item_status st WHERE st.rating_key = m.rating_key AND st.status = ?)
		ON CONFLICT DO NOTHING`, dryRun, by, time.Now().Unix(), ratingKey, StatusKeep))
}

// CancelDeletion stops a deletion. Once executing it can't be stopped.
func (db *DB) CancelDeletion(ctx context.Context, ratingKey string) (bool, error) {
	return changed(db.ExecContext(ctx, `
		UPDATE deletions SET state = ?, finished_at = ? WHERE rating_key = ? AND state IN (?, ?)`,
		DeletionCancelled, time.Now().Unix(), ratingKey, DeletionScheduled, DeletionNotified))
}

// PostponeDeletion adds DeletionDelay to a running clock.
func (db *DB) PostponeDeletion(ctx context.Context, ratingKey string) (bool, error) {
	return changed(db.ExecContext(ctx, `UPDATE deletions SET delete_after = delete_after + ? WHERE rating_key = ? AND state = ?`,
		int64(DeletionDelay/time.Second), ratingKey, DeletionNotified))
}

// UnnotifiedDeletions returns deletions still waiting for their notification.
func (db *DB) UnnotifiedDeletions(ctx context.Context, limit int) ([]Deletion, error) {
	return db.deletions(ctx, `SELECT `+deletionCols+` FROM deletions WHERE state = ? ORDER BY id LIMIT ?`, DeletionScheduled, limit)
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
		q += ` AND EXISTS (SELECT 1 FROM requests r WHERE r.rating_key = d.rating_key AND r.seerr_user_id = ?)`
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
// transaction so the worker can't claim it in between. kept is false if the
// deletion is no longer waiting, whatever happened to it.
func (db *DB) KeepFromDiscord(ctx context.Context, deletionID int64, discordID, name string) (title string, kept bool, err error) {
	err = db.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		var key string
		err := tx.QueryRowContext(ctx, `
			UPDATE deletions SET state = ?, finished_at = ? WHERE id = ? AND state IN (?, ?) RETURNING rating_key, title`,
			DeletionCancelled, now, deletionID, DeletionScheduled, DeletionNotified).Scan(&key, &title)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO item_status (rating_key, status, set_by, set_at, set_by_discord) VALUES (?, ?, 0, ?, ?)
			ON CONFLICT (rating_key) DO UPDATE SET status = excluded.status, set_by = 0, set_at = excluded.set_at,
				set_by_discord = excluded.set_by_discord`, key, StatusKeep, now, discordID)
		return err
	})
	return title, kept && err == nil, err
}
