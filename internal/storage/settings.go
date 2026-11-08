package storage

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"time"
)

// DefaultWindow applies until an admin saves another; saving both empty means any time.
var DefaultWindow = DeleteWindow{Start: "03:00", End: "05:00"}

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
	settingWindowStart  = "delete_window_start"
	settingWindowEnd    = "delete_window_end"
	settingAdminDiscord = "admin_discord_id"
)

var discordID = regexp.MustCompile(`^[0-9]{5,25}$`)

// ValidAdminDiscord reports what's wrong with a Discord user ID, if anything.
// Empty is allowed: notices then tag nobody.
func ValidAdminDiscord(id string) error {
	if id != "" && !discordID.MatchString(id) {
		return errors.New("the discord user must be a numeric user ID")
	}
	return nil
}

// AdminDiscord returns who admin notices tag.
func (db *DB) AdminDiscord(ctx context.Context) (string, error) {
	return db.Setting(ctx, settingAdminDiscord, func() string { return "" })
}

// SetAdminDiscord saves who admin notices tag.
func (db *DB) SetAdminDiscord(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingAdminDiscord, id)
	return err
}

// DefaultAdminDiscord fills in the default when nobody has set one.
func (db *DB) DefaultAdminDiscord(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE value = ''`, settingAdminDiscord, id)
	return err
}

// SpaceRange is when space-based deletions start, as percent of a disk used,
// and how far down they clear it.
type SpaceRange struct{ Start, Floor int }

// DefaultSpace starts at 75%, where filesystems start to slow down.
var DefaultSpace = SpaceRange{Start: 75, Floor: 70}

// ParseSpaceRange reads one disk's fields from the settings form; an empty
// field means its default.
func ParseSpaceRange(start, floor string) (SpaceRange, error) {
	r := DefaultSpace
	for _, f := range []struct {
		in  string
		dst *int
	}{{start, &r.Start}, {floor, &r.Floor}} {
		if f.in == "" {
			continue
		}
		n, err := strconv.Atoi(f.in)
		if err != nil || n < 1 || n > 99 {
			return SpaceRange{}, errors.New("space used must be a percent from 1 to 99")
		}
		*f.dst = n
	}
	if r.Floor >= r.Start {
		return SpaceRange{}, errors.New("space-based deletions must clear down to below where they start")
	}
	return r, nil
}

// SpaceRanges are the saved ranges by disk. A disk without one uses DefaultSpace.
type SpaceRanges map[string]SpaceRange

// For returns a disk's range.
func (rs SpaceRanges) For(disk string) SpaceRange {
	if r, ok := rs[disk]; ok {
		return r
	}
	return DefaultSpace
}

// SpaceRanges returns the saved ranges.
func (db *DB) SpaceRanges(ctx context.Context) (SpaceRanges, error) {
	rows, err := db.QueryContext(ctx, `SELECT path, start, floor FROM disk_ranges`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := SpaceRanges{}
	for rows.Next() {
		var p string
		var r SpaceRange
		if err := rows.Scan(&p, &r.Start, &r.Floor); err != nil {
			return nil, err
		}
		out[p] = r
	}
	return out, rows.Err()
}

// SetSpaceRanges saves ranges by disk in one go; nil puts a disk back on the
// defaults. Disks not named keep what they have.
func (db *DB) SetSpaceRanges(ctx context.Context, rs map[string]*SpaceRange) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		for p, r := range rs {
			var err error
			if r == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM disk_ranges WHERE path = ?`, p)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO disk_ranges (path, start, floor) VALUES (?, ?, ?)
					ON CONFLICT (path) DO UPDATE SET start = excluded.start, floor = excluded.floor`, p, r.Start, r.Floor)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteWindow returns the saved window.
func (db *DB) DeleteWindow(ctx context.Context) (DeleteWindow, error) {
	start, err := db.Setting(ctx, settingWindowStart, func() string { return DefaultWindow.Start })
	if err != nil {
		return DeleteWindow{}, err
	}
	end, err := db.Setting(ctx, settingWindowEnd, func() string { return DefaultWindow.End })
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
