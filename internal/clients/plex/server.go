package plex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	Key   string `json:"key"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

// Item is a movie or show in a library.
type Item struct {
	RatingKey string `json:"ratingKey"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Thumb     string `json:"thumb"`
	AddedAt   int64  `json:"addedAt"`
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
