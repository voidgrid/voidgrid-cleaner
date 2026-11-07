package arr

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Command starts a background task and returns its ID.
func (c *Client) Command(ctx context.Context, body map[string]any) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.call(ctx, http.MethodPost, "/api/v3/command", body, &out)
	return out.ID, err
}

// CommandStatus reads a task's state: "queued", "started", "completed" or
// "failed", and any message with it.
func (c *Client) CommandStatus(ctx context.Context, commandID int64) (status, message string, err error) {
	var out struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	err = c.call(ctx, http.MethodGet, "/api/v3/command/"+id(commandID), nil, &out)
	return out.Status, out.Message, err
}

// MediaManagementRaw reads those settings whole, so a change can keep the rest.
func (c *Client) MediaManagementRaw(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.call(ctx, http.MethodGet, "/api/v3/config/mediamanagement", nil, &out)
	return out, err
}

// SetMediaManagement saves settings read with MediaManagementRaw.
func (c *Client) SetMediaManagement(ctx context.Context, settings map[string]any) error {
	return c.call(ctx, http.MethodPut, "/api/v3/config/mediamanagement", settings, nil)
}

// Movie reads one movie.
func (r *Radarr) Movie(ctx context.Context, movieID int64) (Movie, error) {
	var out Movie
	err := r.call(ctx, http.MethodGet, "/api/v3/movie/"+id(movieID), nil, &out)
	return out, err
}

// LookupMovies searches for movies. Results keep every field, because adding
// one sends the whole thing back.
func (r *Radarr) LookupMovies(ctx context.Context, term string) ([]map[string]any, error) {
	var out []map[string]any
	err := r.call(ctx, http.MethodGet, "/api/v3/movie/lookup?term="+url.QueryEscape(term), nil, &out)
	return out, err
}

// AddMovie adds a looked-up movie and returns its ID.
func (r *Radarr) AddMovie(ctx context.Context, movie map[string]any) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := r.call(ctx, http.MethodPost, "/api/v3/movie", movie, &out)
	return out.ID, err
}

// QualityDefinitions lists the qualities a file can be imported as.
func (r *Radarr) QualityDefinitions(ctx context.Context) ([]map[string]any, error) {
	var defs []struct {
		Quality map[string]any `json:"quality"`
	}
	if err := r.call(ctx, http.MethodGet, "/api/v3/qualitydefinition", nil, &defs); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Quality)
	}
	return out, nil
}

// ImportFile is a file Radarr could import, and why it might not.
type ImportFile struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	FolderName   string `json:"folderName"`
	ReleaseGroup string `json:"releaseGroup"`
	Languages    []any  `json:"languages"`
	Rejections   []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

// ImportFiles lists what's in a folder for a manual import of a movie.
func (r *Radarr) ImportFiles(ctx context.Context, movieID int64, folder string) ([]ImportFile, error) {
	q := url.Values{"movieId": {id(movieID)}, "folder": {folder}, "filterExistingFiles": {"false"}}
	var out []ImportFile
	err := r.call(ctx, http.MethodGet, "/api/v3/manualimport?"+q.Encode(), nil, &out)
	return out, err
}

// OneSeries reads one series.
func (s *Sonarr) OneSeries(ctx context.Context, seriesID int64) (Series, error) {
	var out Series
	err := s.call(ctx, http.MethodGet, "/api/v3/series/"+id(seriesID), nil, &out)
	return out, err
}

// LookupSeries searches for series, keeping every field like LookupMovies.
func (s *Sonarr) LookupSeries(ctx context.Context, term string) ([]map[string]any, error) {
	var out []map[string]any
	err := s.call(ctx, http.MethodGet, "/api/v3/series/lookup?term="+url.QueryEscape(term), nil, &out)
	return out, err
}

// AddSeries adds a looked-up series and returns its ID.
func (s *Sonarr) AddSeries(ctx context.Context, series map[string]any) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := s.call(ctx, http.MethodPost, "/api/v3/series", series, &out)
	return out.ID, err
}

// MissingEpisode is a monitored, aired episode with no file.
type MissingEpisode struct {
	ID            int64  `json:"id"`
	SeriesID      int64  `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	Series        struct {
		Title string `json:"title"`
	} `json:"series"`
}

// MissingEpisodes lists every missing episode with its series.
func (s *Sonarr) MissingEpisodes(ctx context.Context) ([]MissingEpisode, error) {
	const take = 500
	var all []MissingEpisode
	for page := 1; ; page++ {
		var out struct {
			Records []MissingEpisode `json:"records"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(take)}, "monitored": {"true"}, "includeSeries": {"true"}}
		if err := s.call(ctx, http.MethodGet, "/api/v3/wanted/missing?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Records...)
		if len(out.Records) < take {
			return all, nil
		}
	}
}

// MonitorMovie starts monitoring a movie.
func (r *Radarr) MonitorMovie(ctx context.Context, movieID int64) error {
	return r.call(ctx, http.MethodPut, "/api/v3/movie/editor", map[string]any{"movieIds": []int64{movieID}, "monitored": true}, nil)
}

// RemoveMovie takes a movie out of Radarr and leaves any file where it is. It
// adds no exclusion, so it can be added again.
func (r *Radarr) RemoveMovie(ctx context.Context, movieID int64) error {
	return r.call(ctx, http.MethodDelete, "/api/v3/movie/"+id(movieID)+"?deleteFiles=false&addImportExclusion=false", nil, nil)
}

// RemoveSeries takes a series out of Sonarr and leaves any files where they are.
func (s *Sonarr) RemoveSeries(ctx context.Context, seriesID int64) error {
	return s.call(ctx, http.MethodDelete, "/api/v3/series/"+id(seriesID)+"?deleteFiles=false&addImportListExclusion=false", nil, nil)
}
