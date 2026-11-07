// Package seerr talks to the Seerr API: users, local sign-in, requests and
// media records.
//
// Field names follow Seerr's entities (server/entity/*.ts), not its OpenAPI
// file, which leaves out plexId, ratingKey, externalServiceId and seasons.
package seerr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// ErrBadCredentials means Seerr refused a local sign-in.
var ErrBadCredentials = errors.New("seerr: wrong email or password")

// Seerr permission bits (server/lib/permissions.ts). Manage Requests counts
// as admin here: only people who are admins in this app hold it.
const (
	permAdmin          = 2
	permManageRequests = 16
	// The auto-approve permissions: all, movies only and TV only.
	permAutoApprove      = 128
	permAutoApproveMovie = 256
	permAutoApproveTV    = 512
)

// Client calls one Seerr server. With the API key Seerr acts as its admin
// (user 1), which is also what makes it include plexId in user replies.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// User is the part of a Seerr user this app uses.
type User struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	Username     string `json:"username"`
	PlexUsername string `json:"plexUsername"`
	DisplayName  string `json:"displayName"`
	PlexID       *int64 `json:"plexId"`
	Permissions  int64  `json:"permissions"`
}

// IsAdmin reports whether the user is an admin in this app: Seerr Admin,
// Manage Requests, or any auto-approve permission. Someone Seerr trusts to
// get things without approval is trusted with the library here too.
func (u User) IsAdmin() bool {
	return u.Permissions&(permAdmin|permManageRequests|permAutoApprove|permAutoApproveMovie|permAutoApproveTV) != 0
}

// Name is the best display name Seerr has for the user.
func (u User) Name() string {
	for _, s := range []string{u.DisplayName, u.Username, u.PlexUsername, u.Email} {
		if s != "" {
			return s
		}
	}
	return "user " + strconv.FormatInt(u.ID, 10)
}

// Media is Seerr's record of one movie or show. RatingKey is the Plex item;
// ExternalServiceID is the Radarr movie or Sonarr series ID.
type Media struct {
	ID                int64   `json:"id"`
	MediaType         string  `json:"mediaType"` // "movie" or "tv"
	TmdbID            int64   `json:"tmdbId"`
	TvdbID            *int64  `json:"tvdbId"`
	Status            int     `json:"status"`
	RatingKey         *string `json:"ratingKey"`
	ExternalServiceID *int64  `json:"externalServiceId"`
}

// Request is one Seerr request.
type Request struct {
	ID          int64     `json:"id"`
	Status      int       `json:"status"`
	Is4k        bool      `json:"is4k"`
	CreatedAt   time.Time `json:"createdAt"`
	RequestedBy User      `json:"requestedBy"`
	Media       Media     `json:"media"`
	Seasons     []struct {
		SeasonNumber int `json:"seasonNumber"`
	} `json:"seasons"`
}

// User fetches one user by Seerr ID.
func (c *Client) User(ctx context.Context, id int64) (User, error) {
	var u User
	err := c.call(ctx, http.MethodGet, "/api/v1/user/"+strconv.FormatInt(id, 10), true, nil, &u)
	return u, err
}

// UserByPlexID finds the Seerr user linked to a plex.tv account ID.
// found is false when no Seerr user has that Plex account.
func (c *Client) UserByPlexID(ctx context.Context, plexID int64) (u User, found bool, err error) {
	users, err := c.Users(ctx)
	if err != nil {
		return User{}, false, err
	}
	for _, u := range users {
		if u.PlexID != nil && *u.PlexID == plexID {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

// Users returns every Seerr user.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	return pages[User](ctx, c, "/api/v1/user?")
}

// Requests returns every request, of any status.
func (c *Client) Requests(ctx context.Context) ([]Request, error) {
	return pages[Request](ctx, c, "/api/v1/request?filter=all&")
}

// DeleteMedia removes Seerr's media record and the requests on it, so the
// title shows as not requested and can be requested again later. It
// deletes no files; Radarr/Sonarr do that.
func (c *Client) DeleteMedia(ctx context.Context, mediaID int64) error {
	return c.call(ctx, http.MethodDelete, "/api/v1/media/"+strconv.FormatInt(mediaID, 10), true, nil, nil)
}

// LocalLogin checks a Seerr local account's email and password and returns
// the user's Seerr ID. It sends no API key, so Seerr does the full check.
func (c *Client) LocalLogin(ctx context.Context, email, password string) (int64, error) {
	var u User
	err := c.call(ctx, http.MethodPost, "/api/v1/auth/local", false, map[string]string{"email": email, "password": password}, &u)
	var se *httpjson.StatusError
	if errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden) {
		return 0, ErrBadCredentials
	}
	return u.ID, err
}

// pages fetches every page of a list endpoint; prefix ends in ? or &.
func pages[T any](ctx context.Context, c *Client, prefix string) ([]T, error) {
	const take = 100
	var all []T
	for skip := 0; ; skip += take {
		var page struct {
			Results []T `json:"results"`
		}
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("%stake=%d&skip=%d", prefix, take, skip), true, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if len(page.Results) < take {
			return all, nil
		}
	}
}

// Title names something by its TMDB ID, for what the library no longer has.
// mediaType is "movie" or "tv".
func (c *Client) Title(ctx context.Context, mediaType string, tmdbID int64) (string, error) {
	var out struct {
		Title string `json:"title"`
		Name  string `json:"name"`
	}
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/%s/%d", mediaType, tmdbID), true, nil, &out); err != nil {
		return "", err
	}
	if out.Title != "" {
		return out.Title, nil
	}
	return out.Name, nil
}

func (c *Client) call(ctx context.Context, method, path string, withKey bool, body, out any) error {
	hdr := http.Header{}
	if withKey {
		hdr.Set("X-Api-Key", c.APIKey)
	}
	return httpjson.Do(ctx, c.HTTP, "seerr", method, strings.TrimRight(c.BaseURL, "/")+path, hdr, body, out)
}
