package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
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
	Alerted             bool   // posted to Discord
}

// SaveFindings records one kind's complete result: new findings open, gone
// ones resolve, and dismissed ones stay dismissed.
func (db *DB) SaveFindings(ctx context.Context, kind string, fs []Finding) error {
	now := time.Now().Unix()
	subjects := make([]string, 0, len(fs))
	for _, f := range fs {
		subjects = append(subjects, f.Subject)
	}
	still, err := json.Marshal(subjects)
	if err != nil {
		return err
	}
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET state = 'resolved'
			WHERE kind = ? AND state = 'open' AND subject NOT IN (SELECT value FROM json_each(?))`, kind, string(still)); err != nil {
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
					last_seen = excluded.last_seen, alerted = CASE state WHEN 'resolved' THEN 0 ELSE alerted END,
					first_seen = CASE state WHEN 'resolved' THEN excluded.first_seen ELSE first_seen END,
					state = CASE state WHEN 'resolved' THEN 'open' ELSE state END`,
				kind, f.Subject, f.Title, f.Detail, f.Severity, now, now, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// Findings lists findings in a state, worst first within each kind.
func (db *DB) Findings(ctx context.Context, state string) ([]Finding, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind, subject, title, detail, severity, state, first_seen, last_seen, data, alerted FROM findings
		WHERE state = ? ORDER BY kind, title COLLATE NOCASE`, state)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.Kind, &f.Subject, &f.Title, &f.Detail, &f.Severity, &f.State, &f.FirstSeen, &f.LastSeen, &f.Data, &f.Alerted); err != nil {
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

// SetFindingsState dismisses or reopens several findings in one transaction.
func (db *DB) SetFindingsState(ctx context.Context, ids []int64, state string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET state = ? WHERE id = ? AND state <> 'resolved'`, state, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LibraryPath is one path a service uses, as last checked.
type LibraryPath struct {
	Path, UsedBy                string
	Media                       bool // holds a video library, so it can be managed
	Visible, Writable, Verified bool
	Managed                     bool
	// Local is where the app sees Path, if it's mounted somewhere else here.
	Local string
}

// Local turns a path as the services see it into where the app sees it,
// through the longest mapped library path holding it.
func Local(paths []LibraryPath, p string) string {
	best := -1
	for i, lp := range paths {
		if lp.Local != "" && (p == lp.Path || strings.HasPrefix(p, lp.Path+"/")) && (best < 0 || len(lp.Path) > len(paths[best].Path)) {
			best = i
		}
	}
	if best < 0 {
		return p
	}
	return paths[best].Local + strings.TrimPrefix(p, paths[best].Path)
}

// ValidLocal says what's wrong with a mapping, if anything: it must be an
// absolute, clean path. Empty means no mapping.
func ValidLocal(p string) error {
	if p != "" && (!path.IsAbs(p) || path.Clean(p) != p || p == "/") {
		return fmt.Errorf("%q must be a full path, like /media/movies", p)
	}
	return nil
}

// SetLocalPaths saves where the app sees each path; empty clears a mapping.
// Paths not named keep theirs.
func (db *DB) SetLocalPaths(ctx context.Context, local map[string]string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		for p, l := range local {
			if err := ValidLocal(l); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE library_paths SET local = ? WHERE path = ?`, l, p); err != nil {
				return err
			}
		}
		return nil
	})
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
	rows, err := db.QueryContext(ctx, `SELECT path, used_by, media, visible, writable, verified, managed, local FROM library_paths ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LibraryPath
	for rows.Next() {
		var p LibraryPath
		if err := rows.Scan(&p.Path, &p.UsedBy, &p.Media, &p.Visible, &p.Writable, &p.Verified, &p.Managed, &p.Local); err != nil {
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
	Requesters      string // comma-separated names
	RequestedAt     int64  // the first request, unix seconds
}

// UnmatchedRequests lists requests no library item matched, one per Seerr media.
func (db *DB) UnmatchedRequests(ctx context.Context) ([]UnmatchedRequest, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.seerr_media_id, r.tmdb_id, r.tvdb_id, r.media_type, max(r.status),
			coalesce(group_concat(DISTINCT u.display_name), ''), min(r.created_at)
		FROM requests r LEFT JOIN users u ON u.seerr_user_id = r.seerr_user_id
		WHERE r.rating_key IS NULL GROUP BY r.seerr_media_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UnmatchedRequest
	for rows.Next() {
		var r UnmatchedRequest
		if err := rows.Scan(&r.MediaID, &r.TmdbID, &r.TvdbID, &r.Type, &r.Status, &r.Requesters, &r.RequestedAt); err != nil {
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
	ctx = context.WithoutCancel(ctx)
	at := time.Now().Unix()
	var err error
	// A busy database must not cost an entry: the audit log has to be complete.
	for try := 0; try < auditTries; try++ {
		if _, err = db.ExecContext(ctx, `INSERT INTO audit_log (at, who, action, item, detail)
			VALUES (?, ?, ?, coalesce((SELECT title FROM media WHERE rating_key = ?), ?), ?)`,
			at, who, action, item, item, detail); err == nil {
			return
		}
	}
	log.Printf("audit: lost %s %s: %v", action, item, err)
}

const auditTries = 4

// QueuedFix is one fix to queue.
type QueuedFix struct {
	FindingID     int64
	Title, Choice string
}

// QueueFixes queues a batch and logs each one, in one transaction: a few
// hundred separate writes would hold up everything else on a slow disk.
func (db *DB) QueueFixes(ctx context.Context, who string, fixes []QueuedFix) error {
	now := time.Now().Unix()
	return db.tx(ctx, func(tx *sql.Tx) error {
		for _, f := range fixes {
			queued, err := changed(tx.ExecContext(ctx, `INSERT INTO fix_jobs (finding_id, choice, who, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				f.FindingID, f.Choice, who, now))
			if err != nil {
				return err
			}
			if !queued {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, who, action, item, detail) VALUES (?, ?, 'doctor fix approved', ?, ?)`,
				now, who, f.Title, f.Choice); err != nil {
				return err
			}
		}
		return nil
	})
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
	err = db.QueryRowContext(ctx, `SELECT id, kind, subject, title, detail, severity, state, first_seen, last_seen, data, alerted FROM findings WHERE id = ?`, id).
		Scan(&f.ID, &f.Kind, &f.Subject, &f.Title, &f.Detail, &f.Severity, &f.State, &f.FirstSeen, &f.LastSeen, &f.Data, &f.Alerted)
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

// HasActiveArrDeletion is HasActiveDeletion for a Radarr or Sonarr ID.
func (db *DB) HasActiveArrDeletion(ctx context.Context, sonarr bool, arrID int64) (bool, error) {
	mediaType := "movie"
	if sonarr {
		mediaType = "show"
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM deletions d JOIN media m ON m.rating_key IN (d.rating_key, d.parent_key)
		WHERE m.type = ? AND m.arr_id = ? AND d.state IN (?, ?, ?)`,
		mediaType, arrID, DeletionScheduled, DeletionNotified, DeletionExecuting).Scan(&n)
	return n > 0, err
}

// HasActiveDeletion reports whether an item, or an episode of it, is on its way out.
func (db *DB) HasActiveDeletion(ctx context.Context, ratingKey string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM deletions WHERE (rating_key = ? OR parent_key = ?) AND state IN (?, ?, ?)`,
		ratingKey, ratingKey, DeletionScheduled, DeletionNotified, DeletionExecuting).Scan(&n)
	return n > 0, err
}

// ForgetSeerrMedia drops a title's requests once it's gone from Seerr, so the
// doctor sees that before the next sync.
func (db *DB) ForgetSeerrMedia(ctx context.Context, mediaID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM requests WHERE seerr_media_id = ?`, mediaID)
	return err
}

// MarkFindingAlerted records that a finding was posted to Discord. It's
// cleared if the finding resolves and comes back, so a new episode posts again.
func (db *DB) MarkFindingAlerted(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE findings SET alerted = 1 WHERE id = ?`, id)
	return err
}

// DiscordAdmin returns the Seerr user a Discord user is linked to, and
// whether they're an admin. It records the Discord user, so they show on the
// users page to be linked. name is "" if they aren't linked.
func (db *DB) DiscordAdmin(ctx context.Context, discordID, discordName string) (name string, admin bool, err error) {
	if _, err := db.ExecContext(ctx, `INSERT INTO discord_users (discord_id, name, first_seen) VALUES (?, ?, ?)
		ON CONFLICT (discord_id) DO UPDATE SET name = excluded.name`, discordID, discordName, time.Now().Unix()); err != nil {
		return "", false, err
	}
	err = db.QueryRowContext(ctx, `SELECT u.display_name, u.is_admin FROM discord_users d JOIN users u ON u.seerr_user_id = d.seerr_user_id
		WHERE d.discord_id = ?`, discordID).Scan(&name, &admin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return name, admin, err
}
