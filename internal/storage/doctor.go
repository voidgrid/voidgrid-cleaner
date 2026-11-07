package storage

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"time"
)

// Finding is one thing the doctor found.
type Finding struct {
	ID                  int64
	Kind, Subject       string
	Title, Detail       string
	Severity            string // "problem", "check" or "note"
	State               string
	FirstSeen, LastSeen int64
	Data                string // JSON for the fix
}

// SaveFindings records one kind's complete result: new findings open, gone
// ones resolve, and dismissed ones stay dismissed.
func (db *DB) SaveFindings(ctx context.Context, kind string, fs []Finding) error {
	now := time.Now().Unix()
	return db.tx(ctx, func(tx *sql.Tx) error {
		// Resolve first; the upsert reopens whatever is still there.
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET state = 'resolved' WHERE kind = ? AND state = 'open'`, kind); err != nil {
			return err
		}
		for _, f := range fs {
			data := f.Data
			if data == "" {
				data = "{}"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO findings (kind, subject, title, detail, severity, first_seen, last_seen, data)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (kind, subject) DO UPDATE SET title = excluded.title, detail = excluded.detail, severity = excluded.severity, data = excluded.data,
					last_seen = excluded.last_seen, state = CASE state WHEN 'resolved' THEN 'open' ELSE state END`,
				kind, f.Subject, f.Title, f.Detail, f.Severity, now, now, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// Findings lists findings in a state, worst first within each kind.
func (db *DB) Findings(ctx context.Context, state string) ([]Finding, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind, subject, title, detail, severity, state, first_seen, last_seen, data FROM findings
		WHERE state = ? ORDER BY kind, title COLLATE NOCASE`, state)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.Kind, &f.Subject, &f.Title, &f.Detail, &f.Severity, &f.State, &f.FirstSeen, &f.LastSeen, &f.Data); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFindingState dismisses or reopens a finding.
func (db *DB) SetFindingState(ctx context.Context, id int64, state string) (bool, error) {
	return changed(db.ExecContext(ctx, `UPDATE findings SET state = ? WHERE id = ? AND state <> 'resolved'`, state, id))
}

// LibraryPath is one path a service uses, as last checked.
type LibraryPath struct {
	Path, UsedBy                string
	Media                       bool // holds a video library, so it can be managed
	Visible, Writable, Verified bool
	Managed                     bool
}

// SaveLibraryPaths replaces the inferred paths, keeping each one's managed
// choice. A path that's no longer a video library loses it.
func (db *DB) SaveLibraryPaths(ctx context.Context, ps []LibraryPath) error {
	now := time.Now().Unix()
	return db.tx(ctx, func(tx *sql.Tx) error {
		for _, p := range ps {
			if _, err := tx.ExecContext(ctx, `INSERT INTO library_paths (path, used_by, media, visible, writable, verified, checked_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (path) DO UPDATE SET used_by = excluded.used_by, media = excluded.media, visible = excluded.visible,
					writable = excluded.writable, verified = excluded.verified, checked_at = excluded.checked_at,
					managed = managed AND excluded.media`,
				p.Path, p.UsedBy, p.Media, p.Visible, p.Writable, p.Verified, now); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM library_paths WHERE checked_at < ?`, now)
		return err
	})
}

// LibraryPaths lists the known paths.
func (db *DB) LibraryPaths(ctx context.Context) ([]LibraryPath, error) {
	rows, err := db.QueryContext(ctx, `SELECT path, used_by, media, visible, writable, verified, managed FROM library_paths ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LibraryPath
	for rows.Next() {
		var p LibraryPath
		if err := rows.Scan(&p.Path, &p.UsedBy, &p.Media, &p.Visible, &p.Writable, &p.Verified, &p.Managed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetManagedPaths makes exactly these paths managed. Only video libraries can
// be, so music and photos stay out of reach whatever a form says.
func (db *DB) SetManagedPaths(ctx context.Context, paths []string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE library_paths SET managed = 0 WHERE managed = 1`); err != nil {
			return err
		}
		for _, p := range paths {
			if _, err := tx.ExecContext(ctx, `UPDATE library_paths SET managed = 1 WHERE path = ? AND media = 1`, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnmatchedRequest is a Seerr request with nothing in the library.
type UnmatchedRequest struct {
	MediaID, TmdbID int64
	TvdbID          sql.NullInt64
	Type            string
	Status          int
}

// UnmatchedRequests lists requests no library item matched, one per Seerr media.
func (db *DB) UnmatchedRequests(ctx context.Context) ([]UnmatchedRequest, error) {
	rows, err := db.QueryContext(ctx, `SELECT seerr_media_id, tmdb_id, tvdb_id, media_type, max(status) FROM requests
		WHERE rating_key IS NULL GROUP BY seerr_media_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UnmatchedRequest
	for rows.Next() {
		var r UnmatchedRequest
		if err := rows.Scan(&r.MediaID, &r.TmdbID, &r.TvdbID, &r.Type, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WatchCounts compares Plex's finished views with Tautulli's watched plays
// over the time both have covered.
func (db *DB) WatchCounts(ctx context.Context) (plexViews, tautulliWatched int, err error) {
	err = db.QueryRowContext(ctx, `WITH start AS (SELECT min(stopped) AS t FROM plays WHERE reference_id > 0)
		SELECT coalesce(sum(reference_id < 0), 0), coalesce(sum(reference_id > 0 AND watched = 1), 0)
		FROM plays, start WHERE stopped >= start.t`).Scan(&plexViews, &tautulliWatched)
	return plexViews, tautulliWatched, err
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	At                        int64
	Who, Action, Item, Detail string
}

// Audit records a decision. item may be a rating key, which is stored as the
// title so the entry still reads after the item is gone. A failure is only
// logged: the audit must never stop a deletion already under way.
func (db *DB) Audit(ctx context.Context, who, action, item, detail string) {
	if _, err := db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO audit_log (at, who, action, item, detail)
		VALUES (?, ?, ?, coalesce((SELECT title FROM media WHERE rating_key = ?), ?), ?)`,
		time.Now().Unix(), who, action, item, item, detail); err != nil {
		log.Printf("audit: %s %s: %v", action, item, err)
	}
}

// AuditLog lists entries newest first, matching search if it's set.
func (db *DB) AuditLog(ctx context.Context, search string, limit, offset int) ([]AuditEntry, error) {
	like := "%" + likeEscaper.Replace(search) + "%"
	rows, err := db.QueryContext(ctx, `SELECT at, who, action, item, detail FROM audit_log
		WHERE ? = '' OR who LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR item LIKE ? ESCAPE '\'
		ORDER BY at DESC, id DESC LIMIT ? OFFSET ?`, search, like, like, like, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Who, &e.Action, &e.Item, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DefaultLogExpiry is how many days the audit log keeps by default.
const DefaultLogExpiry = 90

const settingLogExpiry = "log_expiry_days"

// ParseLogExpiry reads the days from the settings form; empty means the default.
func ParseLogExpiry(s string) (int, error) {
	if s == "" {
		return DefaultLogExpiry, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 3650 {
		return 0, errors.New("log expiry must be from 1 to 3650 days")
	}
	return n, nil
}

// LogExpiry returns the saved days.
func (db *DB) LogExpiry(ctx context.Context) (int, error) {
	s, err := db.Setting(ctx, settingLogExpiry, func() string { return strconv.Itoa(DefaultLogExpiry) })
	if err != nil {
		return 0, err
	}
	return ParseLogExpiry(s)
}

// SetLogExpiry saves the days.
func (db *DB) SetLogExpiry(ctx context.Context, days int) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingLogExpiry, strconv.Itoa(days))
	return err
}

// Log levels. Audit adds every call the worker makes to what normal records.
const (
	LogNormal = "normal"
	LogAudit  = "audit"
)

// DefaultLogLevel is what the log records by default.
const DefaultLogLevel = LogAudit

const settingLogLevel = "log_level"

// ParseLogLevel reads the level from the settings form; empty means the default.
func ParseLogLevel(s string) (string, error) {
	switch s {
	case "":
		return DefaultLogLevel, nil
	case LogNormal, LogAudit:
		return s, nil
	}
	return "", errors.New("log level must be normal or audit")
}

// LogLevel returns the saved level.
func (db *DB) LogLevel(ctx context.Context) (string, error) {
	s, err := db.Setting(ctx, settingLogLevel, func() string { return DefaultLogLevel })
	if err != nil {
		return "", err
	}
	return ParseLogLevel(s)
}

// SetLogLevel saves the level.
func (db *DB) SetLogLevel(ctx context.Context, level string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingLogLevel, level)
	return err
}

// PruneAudit deletes entries older than the expiry, a batch at a time so it
// never holds the write lock for long. It touches only the audit log:
// deletions rows are what stop cancelled items being scheduled again.
func (db *DB) PruneAudit(ctx context.Context, now time.Time) error {
	days, err := db.LogExpiry(ctx)
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -days).Unix()
	for {
		more, err := changed(db.ExecContext(ctx, `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log WHERE at < ? LIMIT 1000)`, cutoff))
		if err != nil || !more {
			return err
		}
	}
}

// Finding returns one finding; found is false if there's no such ID.
func (db *DB) Finding(ctx context.Context, id int64) (f Finding, found bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT id, kind, subject, title, detail, severity, state, first_seen, last_seen, data FROM findings WHERE id = ?`, id).
		Scan(&f.ID, &f.Kind, &f.Subject, &f.Title, &f.Detail, &f.Severity, &f.State, &f.FirstSeen, &f.LastSeen, &f.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// FixJob is a fix an admin approved.
type FixJob struct {
	ID, FindingID                     int64
	Title, Choice, Who, State, Detail string
	CreatedAt, FinishedAt             int64
}

const fixCols = `j.id, j.finding_id, f.title, j.choice, j.who, j.state, j.detail, j.created_at, j.finished_at`

func scanFix(sc interface{ Scan(...any) error }) (FixJob, error) {
	var j FixJob
	err := sc.Scan(&j.ID, &j.FindingID, &j.Title, &j.Choice, &j.Who, &j.State, &j.Detail, &j.CreatedAt, &j.FinishedAt)
	return j, err
}

// QueueFix adds a fix; false means that finding already has one waiting.
func (db *DB) QueueFix(ctx context.Context, findingID int64, choice, who string) (bool, error) {
	return changed(db.ExecContext(ctx, `INSERT INTO fix_jobs (finding_id, choice, who, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		findingID, choice, who, time.Now().Unix()))
}

// ClaimFix moves the oldest queued fix to running.
func (db *DB) ClaimFix(ctx context.Context) (j FixJob, found bool, err error) {
	var id int64
	err = db.QueryRowContext(ctx, `UPDATE fix_jobs SET state = 'running'
		WHERE id = (SELECT id FROM fix_jobs WHERE state = 'queued' ORDER BY id LIMIT 1) RETURNING id`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	j, err = scanFix(db.QueryRowContext(ctx, `SELECT `+fixCols+` FROM fix_jobs j JOIN findings f ON f.id = j.finding_id WHERE j.id = ?`, id))
	return j, err == nil, err
}

// FinishFix records how a fix ended.
func (db *DB) FinishFix(ctx context.Context, id int64, state, detail string) error {
	_, err := db.ExecContext(ctx, `UPDATE fix_jobs SET state = ?, detail = ?, finished_at = ? WHERE id = ?`, state, detail, time.Now().Unix(), id)
	return err
}

// FailInterruptedFixes ends fixes a restart cut off. They don't resume: a
// half-applied fix needs a person to look at the finding again.
func (db *DB) FailInterruptedFixes(ctx context.Context) error {
	_, err := db.ExecContext(ctx, `UPDATE fix_jobs SET state = 'failed', detail = 'interrupted by a restart', finished_at = ? WHERE state = 'running'`, time.Now().Unix())
	return err
}

// RecentFixes lists the latest fixes, newest first.
func (db *DB) RecentFixes(ctx context.Context, limit int) ([]FixJob, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+fixCols+` FROM fix_jobs j JOIN findings f ON f.id = j.finding_id ORDER BY j.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FixJob
	for rows.Next() {
		j, err := scanFix(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// HasActiveDeletion reports whether an item, or an episode of it, is on its way out.
func (db *DB) HasActiveDeletion(ctx context.Context, ratingKey string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM deletions WHERE (rating_key = ? OR parent_key = ?) AND state IN (?, ?, ?)`,
		ratingKey, ratingKey, DeletionScheduled, DeletionNotified, DeletionExecuting).Scan(&n)
	return n > 0, err
}
