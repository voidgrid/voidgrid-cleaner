// Package tautulli reads watch history and current activity from Tautulli
// (API v2, per the Tautulli-API-Reference wiki page).
package tautulli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// Client calls one Tautulli server. The key goes in the X-Api-Key header
// (Tautulli 2.18+), never the URL.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Int is a number Tautulli may send as 42, "42" or "" (meaning 0).
type Int int64

// UnmarshalJSON accepts a number, a numeric string, "" or null.
func (n *Int) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(b, `"`))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("tautulli: not an integer: %s", b)
	}
	*n = Int(v)
	return nil
}

// Play is one history row. For an episode, GrandparentRatingKey is the show,
// ParentMediaIndex the season and MediaIndex the episode number. UserID is
// the plex.tv account ID, the same as Seerr's plexId.
type Play struct {
	ReferenceID          Int     `json:"reference_id"` // first row of a grouped play; stable as the group grows
	UserID               Int     `json:"user_id"`
	User                 string  `json:"user"`
	MediaType            string  `json:"media_type"` // "movie" or "episode"
	RatingKey            Int     `json:"rating_key"`
	ParentRatingKey      Int     `json:"parent_rating_key"`
	GrandparentRatingKey Int     `json:"grandparent_rating_key"`
	MediaIndex           Int     `json:"media_index"`
	ParentMediaIndex     Int     `json:"parent_media_index"`
	PercentComplete      Int     `json:"percent_complete"`
	WatchedStatus        float64 `json:"watched_status"` // 0, 0.5 or 1
	Stopped              Int     `json:"stopped"`        // unix seconds
	// State is set ("playing", "paused") only on sessions still in progress,
	// which Tautulli appends to history with null IDs.
	State string `json:"state"`
}

// Session is something being played right now.
type Session struct {
	UserID               Int    `json:"user_id"`
	MediaType            string `json:"media_type"`
	RatingKey            Int    `json:"rating_key"`
	GrandparentRatingKey Int    `json:"grandparent_rating_key"`
	State                string `json:"state"`
}

// historyPage is how many rows History asks for per call.
const historyPage = 1000

// History returns finished movie and episode plays from the given day on (Tautulli
// filters by date, inclusive), oldest first. Resumed sessions are grouped
// into one play, whatever Tautulli's own grouping setting is.
func (c *Client) History(ctx context.Context, after time.Time) ([]Play, error) {
	var all []Play
	for start := 0; ; start += historyPage {
		var data struct {
			Data []Play `json:"data"`
		}
		q := url.Values{
			"after": {after.Format("2006-01-02")}, "grouping": {"1"}, "order_column": {"date"}, "order_dir": {"asc"},
			"start": {strconv.Itoa(start)}, "length": {strconv.Itoa(historyPage)},
		}
		if err := c.cmd(ctx, "get_history", q, &data); err != nil {
			return nil, err
		}
		for _, p := range data.Data {
			if (p.MediaType == "movie" || p.MediaType == "episode") && p.State == "" {
				all = append(all, p)
			}
		}
		if len(data.Data) < historyPage {
			return all, nil
		}
	}
}

// Activity returns what's playing now.
func (c *Client) Activity(ctx context.Context) ([]Session, error) {
	var data struct {
		Sessions []Session `json:"sessions"`
	}
	err := c.cmd(ctx, "get_activity", nil, &data)
	return data.Sessions, err
}

func (c *Client) cmd(ctx context.Context, cmd string, q url.Values, data any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("cmd", cmd)
	hdr := http.Header{}
	hdr.Set("X-Api-Key", c.APIKey)
	var env struct {
		Response struct {
			Result  string          `json:"result"`
			Message *string         `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"response"`
	}
	if err := httpjson.Do(ctx, c.HTTP, "tautulli", http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/api/v2?"+q.Encode(), hdr, nil, &env); err != nil {
		return err
	}
	if env.Response.Result != "success" {
		msg := ""
		if env.Response.Message != nil {
			msg = *env.Response.Message
		}
		return fmt.Errorf("tautulli: %s: %s %s", cmd, env.Response.Result, msg)
	}
	if err := json.Unmarshal(env.Response.Data, data); err != nil {
		return fmt.Errorf("tautulli: %s: decode: %w", cmd, err)
	}
	return nil
}
