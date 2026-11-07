package storage

import (
	"context"
	"errors"
	"time"
)

// DefaultDigestTime is when the daily digest posts, local time: just after
// the default delete window, so it reports that night.
const DefaultDigestTime = "05:30"

const (
	settingDigestTime = "digest_time"
	settingDigestSent = "digest_sent"
)

// ParseDigestTime reads the time from the settings form; empty means the default.
func ParseDigestTime(s string) (string, error) {
	if s == "" {
		return DefaultDigestTime, nil
	}
	if _, err := time.Parse("15:04", s); err != nil {
		return "", errors.New("the digest time must be like 05:30")
	}
	return s, nil
}

// DigestTime returns the saved time.
func (db *DB) DigestTime(ctx context.Context) (string, error) {
	s, err := db.Setting(ctx, settingDigestTime, func() string { return DefaultDigestTime })
	if err != nil {
		return "", err
	}
	return ParseDigestTime(s)
}

// SetDigestTime saves the time.
func (db *DB) SetDigestTime(ctx context.Context, t string) error {
	return db.setSetting(ctx, settingDigestTime, t)
}

// DigestSent returns the local date, "2006-01-02", the digest last went out.
func (db *DB) DigestSent(ctx context.Context) (string, error) {
	return db.Setting(ctx, settingDigestSent, func() string { return "" })
}

// SetDigestSent records that today's digest went out, so a restart doesn't
// send it twice.
func (db *DB) SetDigestSent(ctx context.Context, date string) error {
	return db.setSetting(ctx, settingDigestSent, date)
}

func (db *DB) setSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Digest is what the daily posts report on.
type Digest struct {
	Upcoming     []Deletion // due within a day, ephemeral episodes folded by show
	Failed, Held int
	NewFindings  int
	OpenFindings int
	SyncErrors   []SyncStatus
	PendingAsks  int
	UnlinkedDisc int
	UnlinkedPlex int
	SpaceQueue   int
}

// EpisodeWait is an ephemeral show with episodes someone on it hasn't watched.
type EpisodeWait struct {
	Title    string
	Episodes int
	Who      string // comma-separated names
}

// DigestData gathers the digest for the day ending at now.
func (db *DB) DigestData(ctx context.Context, since, now time.Time, dryRun bool) (Digest, error) {
	var d Digest
	up, err := db.deletions(ctx, `SELECT `+deletionCols+` FROM deletions WHERE state = ? AND delete_after <= ? ORDER BY delete_after`,
		DeletionNotified, now.Add(24*time.Hour).Unix())
	if err != nil {
		return d, err
	}
	d.Upcoming = ByShow(up)
	for _, q := range []struct {
		dst  *int
		sql  string
		args []any
	}{
		{&d.Failed, `SELECT count(*) FROM deletions WHERE state = ? AND finished_at >= ?`, []any{DeletionFailed, since.Unix()}},
		{&d.Held, `SELECT count(*) FROM deletions WHERE state = ? AND detail <> ''`, []any{DeletionNotified}},
		{&d.NewFindings, `SELECT count(*) FROM findings WHERE state = 'open' AND first_seen >= ?`, []any{since.Unix()}},
		{&d.OpenFindings, `SELECT count(*) FROM findings WHERE state = 'open'`, nil},
		{&d.UnlinkedDisc, `SELECT count(*) FROM discord_users WHERE seerr_user_id IS NULL`, nil},
		{&d.PendingAsks, `SELECT count(*) FROM change_requests WHERE state = 'pending'`, nil},
	} {
		if err := db.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			return d, err
		}
	}
	accounts, err := db.PlexAccounts(ctx)
	if err != nil {
		return d, err
	}
	for _, a := range accounts {
		if a.SeerrID == 0 {
			d.UnlinkedPlex++
		}
	}
	statuses, err := db.SyncStatuses(ctx)
	if err != nil {
		return d, err
	}
	for _, s := range statuses {
		if s.LastError != "" {
			d.SyncErrors = append(d.SyncErrors, s)
		}
	}
	space, err := db.SpaceItems(ctx, dryRun)
	if err != nil {
		return d, err
	}
	for _, s := range space {
		if !s.Active {
			d.SpaceQueue++
		}
	}
	return d, nil
}

// EpisodeWaits finds ephemeral shows where someone has watched an episode and
// someone else on the show hasn't yet.
func (db *DB) EpisodeWaits(ctx context.Context) ([]EpisodeWait, error) {
	rows, err := db.QueryContext(ctx, `
		WITH w AS `+showWatchers+`,
		seen AS (SELECT DISTINCT m.rating_key, m.title, p.season, p.episode
			FROM item_status st JOIN media m ON m.rating_key = st.rating_key
			JOIN plays p ON p.show_rating_key = m.rating_key AND p.media_type = 'episode' AND p.watched = 1 AND p.stopped >= m.added_at
			WHERE st.status = ?),
		behind AS (SELECT s.rating_key, s.title, s.season, s.episode, w.seerr_user_id FROM seen s JOIN w ON w.rating_key = s.rating_key
			WHERE NOT EXISTS (SELECT 1 FROM plays q JOIN `+plexLinks+` l ON l.plex_user_id = q.plex_user_id
				JOIN media m ON m.rating_key = s.rating_key
				WHERE l.seerr_user_id = w.seerr_user_id AND q.show_rating_key = s.rating_key AND q.season = s.season
					AND q.episode = s.episode AND q.watched = 1 AND q.stopped >= m.added_at))
		SELECT b.title, count(DISTINCT b.season || 'x' || b.episode), group_concat(DISTINCT u.display_name)
		FROM behind b JOIN users u ON u.seerr_user_id = b.seerr_user_id
		GROUP BY b.rating_key ORDER BY b.title COLLATE NOCASE`, StatusEphemeral)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EpisodeWait
	for rows.Next() {
		var e EpisodeWait
		if err := rows.Scan(&e.Title, &e.Episodes, &e.Who); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
