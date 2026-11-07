package storage

import (
	"context"
	"errors"
	"time"
)

// DeleteWindow limits deletions to part of the day, in local time. Start and
// End are "15:04"; both empty means any time. End may be past midnight.
type DeleteWindow struct {
	Start, End string
}

func minutes(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute(), err
}

// Validate reports what's wrong with the window, if anything.
func (w DeleteWindow) Validate() error {
	if w.Start == "" && w.End == "" {
		return nil
	}
	start, err := minutes(w.Start)
	if err != nil {
		return errors.New("set both times, or neither")
	}
	end, err := minutes(w.End)
	if err != nil {
		return errors.New("set both times, or neither")
	}
	if start == end {
		return errors.New("the window needs a different start and end")
	}
	return nil
}

// Contains reports whether t is inside the window. An invalid window
// contains nothing, so a bad value stops deletions rather than widening them.
func (w DeleteWindow) Contains(t time.Time) bool {
	if w.Start == "" && w.End == "" {
		return true
	}
	if w.Validate() != nil {
		return false
	}
	start, _ := minutes(w.Start)
	end, _ := minutes(w.End)
	now := t.Hour()*60 + t.Minute()
	if start < end {
		return start <= now && now < end
	}
	return now >= start || now < end
}

// Next returns t if it's inside the window, otherwise when it next opens.
func (w DeleteWindow) Next(t time.Time) time.Time {
	if w.Contains(t) || w.Validate() != nil {
		return t
	}
	start, _ := minutes(w.Start)
	open := time.Date(t.Year(), t.Month(), t.Day(), start/60, start%60, 0, 0, t.Location())
	if !open.After(t) {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

const (
	settingWindowStart = "delete_window_start"
	settingWindowEnd   = "delete_window_end"
)

// DeleteWindow returns the saved window.
func (db *DB) DeleteWindow(ctx context.Context) (DeleteWindow, error) {
	none := func() string { return "" }
	start, err := db.Setting(ctx, settingWindowStart, none)
	if err != nil {
		return DeleteWindow{}, err
	}
	end, err := db.Setting(ctx, settingWindowEnd, none)
	return DeleteWindow{Start: start, End: end}, err
}

// SetDeleteWindow saves a valid window.
func (db *DB) SetDeleteWindow(ctx context.Context, w DeleteWindow) error {
	if err := w.Validate(); err != nil {
		return err
	}
	for k, v := range map[string]string{settingWindowStart: w.Start, settingWindowEnd: w.End} {
		if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}

// Thumbs returns every library item's Plex thumb path.
func (db *DB) Thumbs(ctx context.Context) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT thumb FROM media WHERE thumb <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
