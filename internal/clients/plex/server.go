package plex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// Server is one Plex Media Server, called with the admin's token.
// Endpoints follow developer.plex.tv/pms.
type Server struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Section is a library. Type is "movie", "show", "artist" (music) or "photo".
type Section struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Location []struct {
		Path string `json:"path"`
	} `json:"Location"`
}

// Item is a movie or show in a library.
type Item struct {
	RatingKey string `json:"ratingKey"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Thumb     string `json:"thumb"`
	AddedAt   int64  `json:"addedAt"`
	// OriginalTitle is often the foreign title a match should be compared with.
	OriginalTitle string `json:"originalTitle"`
	// Media holds a movie's files. Shows list only Location, and only from
	// Location below, not in a library listing.
	Media []struct {
		Part []struct {
			File string `json:"file"`
		} `json:"Part"`
	} `json:"Media"`
	// Plex also sends "guid" (a plex:// string). It must have its own field:
	// encoding/json matches names case-insensitively, so it would otherwise
	// land on Guids and fail to decode.
	PlexGUID string `json:"guid"`
	Guids    []struct {
		ID string `json:"id"` // e.g. "tmdb://105", "tvdb://299"
	} `json:"Guid"`
}

// ExternalID returns the item's ID in an agent such as "tmdb" or "tvdb".
func (it Item) ExternalID(agent string) string {
	for _, g := range it.Guids {
		if id, ok := strings.CutPrefix(g.ID, agent+"://"); ok {
			return id
		}
	}
	return ""
}

// View is one finished view in Plex's own history. Removed items have no
// keys. AccountID is the plex.tv ID, except 1, which is the server owner.
type View struct {
	HistoryKey     string `json:"historyKey"`
	RatingKey      string `json:"ratingKey"`
	GrandparentKey string `json:"grandparentKey"`
	Type           string `json:"type"`
	Index          int    `json:"index"`
	ParentIndex    int    `json:"parentIndex"`
	ViewedAt       int64  `json:"viewedAt"`
	AccountID      int64  `json:"accountID"`
}

const viewPage = 5000

// ViewHistory returns every view Plex has on record, all accounts included.
func (s *Server) ViewHistory(ctx context.Context) ([]View, error) {
	var all []View
	for start := 0; ; start += viewPage {
		var out struct {
			MediaContainer struct {
				Metadata []View `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := fmt.Sprintf("/status/sessions/history/all?sort=viewedAt:desc&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", start, viewPage)
		if err := s.do(ctx, http.MethodGet, path, &out); err != nil {
			return nil, err
		}
		all = append(all, out.MediaContainer.Metadata...)
		if len(out.MediaContainer.Metadata) < viewPage {
			return all, nil
		}
	}
}

// ServerAccount is an account the server knows.
type ServerAccount struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the server's accounts.
func (s *Server) Accounts(ctx context.Context) ([]ServerAccount, error) {
	var out struct {
		MediaContainer struct {
			Account []ServerAccount `json:"Account"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/accounts", &out)
	return out.MediaContainer.Account, err
}

// Session is something playing now.
type Session struct {
	RatingKey            string `json:"ratingKey"`
	GrandparentRatingKey string `json:"grandparentRatingKey"`
}

// Sessions lists what's playing now.
func (s *Server) Sessions(ctx context.Context) ([]Session, error) {
	var out struct {
		MediaContainer struct {
			Metadata []Session `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/status/sessions", &out)
	return out.MediaContainer.Metadata, err
}

// Files lists a movie's files.
func (it Item) Files() []string {
	var out []string
	for _, m := range it.Media {
		for _, p := range m.Part {
			out = append(out, p.File)
		}
	}
	return out
}

// ShowPath returns a show's folder. A library listing leaves it out.
func (s *Server) ShowPath(ctx context.Context, ratingKey string) (string, error) {
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := s.do(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(ratingKey), &r); err != nil {
		return "", err
	}
	if len(r.MediaContainer.Metadata) == 0 || len(r.MediaContainer.Metadata[0].Location) == 0 {
		return "", nil
	}
	return r.MediaContainer.Metadata[0].Location[0].Path, nil
}

// PreferLocalMetadata reports whether a library uses tags inside files over
// Plex's own data. Libraries without the setting report false.
func (s *Server) PreferLocalMetadata(ctx context.Context, sectionKey string) (bool, error) {
	var r struct {
		MediaContainer struct {
			Setting []struct {
				ID    string `json:"id"`
				Value any    `json:"value"`
			} `json:"Setting"`
		} `json:"MediaContainer"`
	}
	if err := s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/prefs", &r); err != nil {
		return false, err
	}
	for _, p := range r.MediaContainer.Setting {
		if p.ID == "respectTags" {
			return p.Value == true || p.Value == "true" || p.Value == "1", nil
		}
	}
	return false, nil
}

// Match is something Plex could match an item to.
type Match struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
	Year int    `json:"year"`
}

// Matches searches for what an item could be. title may be an ID such as
// "tmdb-105"; year 0 leaves the year out.
func (s *Server) Matches(ctx context.Context, ratingKey, title string, year int, agent string) ([]Match, error) {
	q := url.Values{"manual": {"1"}, "title": {title}, "agent": {agent}}
	if year != 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var r struct {
		MediaContainer struct {
			SearchResult []Match `json:"SearchResult"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(ratingKey)+"/matches?"+q.Encode(), &r)
	return r.MediaContainer.SearchResult, err
}

// Match rematches an item. Its file and plays stay as they are.
func (s *Server) Match(ctx context.Context, ratingKey, guid, name string) error {
	q := url.Values{"guid": {guid}, "name": {name}}
	return s.do(ctx, http.MethodPut, "/library/metadata/"+url.PathEscape(ratingKey)+"/match?"+q.Encode(), nil)
}

// Item reads one item.
func (s *Server) Item(ctx context.Context, ratingKey string) (Item, error) {
	var r struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := s.do(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(ratingKey), &r); err != nil {
		return Item{}, err
	}
	if len(r.MediaContainer.Metadata) == 0 {
		return Item{}, fmt.Errorf("plex: no item %s", ratingKey)
	}
	return r.MediaContainer.Metadata[0], nil
}

// SetPreferLocalMetadata changes that library setting.
func (s *Server) SetPreferLocalMetadata(ctx context.Context, sectionKey string, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.do(ctx, http.MethodPut, "/library/sections/"+url.PathEscape(sectionKey)+"/prefs?respectTags="+v, nil)
}

// RefreshMetadata re-reads all of a library's metadata. Plex does it in the
// background; this returns once it's started.
func (s *Server) RefreshMetadata(ctx context.Context, sectionKey string) error {
	return s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/refresh?force=1", nil)
}

// Sections lists the libraries.
func (s *Server) Sections(ctx context.Context) ([]Section, error) {
	var r struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/library/sections/all", &r)
	return r.MediaContainer.Directory, err
}

// Items lists a library's movies or shows, with their external IDs.
func (s *Server) Items(ctx context.Context, sectionKey string) ([]Item, error) {
	var r struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/all?includeGuids=1", &r)
	return r.MediaContainer.Metadata, err
}

// RefreshSection makes Plex rescan a library, so deleted files drop out.
func (s *Server) RefreshSection(ctx context.Context, sectionKey string) error {
	return s.do(ctx, http.MethodPost, "/library/sections/"+url.PathEscape(sectionKey)+"/refresh", nil)
}

// EmptyTrash removes a library's items whose files are gone.
func (s *Server) EmptyTrash(ctx context.Context, sectionKey string) error {
	return s.do(ctx, http.MethodPut, "/library/sections/"+url.PathEscape(sectionKey)+"/emptyTrash", nil)
}

// DeleteItem deletes a library item and its media files through Plex. It
// needs "Allow media deletion" on in the server's settings. Only for items
// no Radarr/Sonarr manages: for those, deleting behind the arr's back would
// let it download the item again.
func (s *Server) DeleteItem(ctx context.Context, ratingKey string) error {
	return s.do(ctx, http.MethodDelete, "/library/metadata/"+url.PathEscape(ratingKey), nil)
}

// maxPoster caps a resized poster.
const maxPoster = 5 << 20

// Poster returns a thumb (an Item's Thumb path) resized to width x height
// as JPEG.
func (s *Server) Poster(ctx context.Context, thumb string, width, height int) ([]byte, error) {
	q := url.Values{}
	q.Set("url", thumb)
	q.Set("width", fmt.Sprint(width))
	q.Set("height", fmt.Sprint(height))
	q.Set("minSize", "1")
	q.Set("format", "jpg")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.BaseURL, "/")+"/photo/:/transcode?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", s.Token)
	hc := s.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: poster: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpjson.StatusError{Service: "plex", Method: http.MethodGet, Path: "/photo/:/transcode", Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPoster+1))
	if err == nil && len(b) > maxPoster {
		return nil, fmt.Errorf("plex: poster over %d bytes", maxPoster)
	}
	return b, err
}

func (s *Server) do(ctx context.Context, method, path string, out any) error {
	hdr := http.Header{}
	hdr.Set("X-Plex-Token", s.Token)
	return httpjson.Do(ctx, s.HTTP, "plex", method, strings.TrimRight(s.BaseURL, "/")+path, hdr, nil, out)
}
