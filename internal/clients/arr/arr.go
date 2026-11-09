// Package arr talks to Radarr and Sonarr (API v3; paths and fields checked
// against each project's openapi.json).
//
// It only offers the calls the deletion order needs, and each delete call
// does exactly one thing; the caller runs them in the safe order:
// unmonitor, then clear the queue, then delete files.
package arr

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// Client is the part Radarr and Sonarr share.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	name    string // for errors
}

// Radarr is a Radarr server.
type Radarr struct{ Client }

// Sonarr is a Sonarr server.
type Sonarr struct{ Client }

// NewRadarr builds a Radarr client.
func NewRadarr(baseURL, apiKey string, hc *http.Client) *Radarr {
	return &Radarr{Client{BaseURL: baseURL, APIKey: apiKey, HTTP: hc, name: "radarr"}}
}

// NewSonarr builds a Sonarr client.
func NewSonarr(baseURL, apiKey string, hc *http.Client) *Sonarr {
	return &Sonarr{Client{BaseURL: baseURL, APIKey: apiKey, HTTP: hc, name: "sonarr"}}
}

// DiskSpace is one disk as Radarr/Sonarr see it.
type DiskSpace struct {
	Path       string `json:"path"`
	Label      string `json:"label"`
	FreeSpace  int64  `json:"freeSpace"`
	TotalSpace int64  `json:"totalSpace"`
}

// QueueItem is a download in progress.
type QueueItem struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	EpisodeID int64  `json:"episodeId"` // Sonarr only
}

// RootFolder is a library folder, with the folders in it that nothing tracks.
type RootFolder struct {
	Path            string `json:"path"`
	Accessible      bool   `json:"accessible"`
	UnmappedFolders []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"unmappedFolders"`
}

// RootFolders lists the library folders.
func (c *Client) RootFolders(ctx context.Context) ([]RootFolder, error) {
	var out []RootFolder
	err := c.call(ctx, http.MethodGet, "/api/v3/rootfolder", nil, &out)
	return out, err
}

// Health is one entry on the System > Status page.
type Health struct {
	Source  string `json:"source"`
	Type    string `json:"type"` // "ok", "notice", "warning" or "error"
	Message string `json:"message"`
	WikiURL string `json:"wikiUrl"`
}

// Health lists what the app itself reports as wrong.
func (c *Client) Health(ctx context.Context) ([]Health, error) {
	var out []Health
	err := c.call(ctx, http.MethodGet, "/api/v3/health", nil, &out)
	return out, err
}

// MediaManagement is the part of those settings the doctor checks.
type MediaManagement struct {
	RecycleBin              string `json:"recycleBin"`
	DeleteEmptyFolders      bool   `json:"deleteEmptyFolders"`
	CreateEmptyMovieFolders bool   `json:"createEmptyMovieFolders"`
}

// MediaManagement reads those settings.
func (c *Client) MediaManagement(ctx context.Context) (MediaManagement, error) {
	var out MediaManagement
	err := c.call(ctx, http.MethodGet, "/api/v3/config/mediamanagement", nil, &out)
	return out, err
}

// Rename is a file that doesn't match the naming format.
type Rename struct {
	ExistingPath  string `json:"existingPath"`
	NewPath       string `json:"newPath"`
	MovieID       int64  `json:"movieId"`
	MovieFileID   int64  `json:"movieFileId"`
	SeriesID      int64  `json:"seriesId"`
	EpisodeFileID int64  `json:"episodeFileId"`
}

// DiskSpace lists free and total space per disk.
func (c *Client) DiskSpace(ctx context.Context) ([]DiskSpace, error) {
	var d []DiskSpace
	err := c.call(ctx, http.MethodGet, "/api/v3/diskspace", nil, &d)
	return d, err
}

// RemoveFromQueue cancels a download and removes it from the download
// client, without blocklisting the release (a blocklist could trigger a
// fresh search).
func (c *Client) RemoveFromQueue(ctx context.Context, queueID int64) error {
	return c.call(ctx, http.MethodDelete,
		"/api/v3/queue/"+id(queueID)+"?removeFromClient=true&blocklist=false&skipRedownload=true", nil, nil)
}

// Movie is a Radarr movie.
type Movie struct {
	ID               int64      `json:"id"`
	Title            string     `json:"title"`
	Year             int        `json:"year"`
	TmdbID           int64      `json:"tmdbId"`
	Monitored        bool       `json:"monitored"`
	HasFile          bool       `json:"hasFile"`
	SizeOnDisk       int64      `json:"sizeOnDisk"`
	Path             string     `json:"path"`
	RootFolderPath   string     `json:"rootFolderPath"`
	Added            time.Time  `json:"added"`
	OriginalTitle    string     `json:"originalTitle"`
	ImdbID           string     `json:"imdbId"`
	QualityProfileID int64      `json:"qualityProfileId"`
	IsAvailable      bool       `json:"isAvailable"`
	AlternateTitles  []AltTitle `json:"alternateTitles"`
	MovieFile        *struct {
		RelativePath string `json:"relativePath"`
	} `json:"movieFile"`
}

// AltTitle is another name for a movie or series.
type AltTitle struct {
	Title string `json:"title"`
}

// RenamePreview lists what Radarr would rename among these movies.
func (r *Radarr) RenamePreview(ctx context.Context, movieIDs []int64) ([]Rename, error) {
	q := url.Values{}
	for _, m := range movieIDs {
		q.Add("movieId", id(m))
	}
	var out []Rename
	err := r.call(ctx, http.MethodGet, "/api/v3/rename?"+q.Encode(), nil, &out)
	return out, err
}

// Movies lists every movie.
func (r *Radarr) Movies(ctx context.Context) ([]Movie, error) {
	var m []Movie
	err := r.call(ctx, http.MethodGet, "/api/v3/movie?excludeLocalCovers=true", nil, &m)
	return m, err
}

// UnmonitorMovie stops Radarr searching for or grabbing the movie.
func (r *Radarr) UnmonitorMovie(ctx context.Context, movieID int64) error {
	return r.call(ctx, http.MethodPut, "/api/v3/movie/editor",
		map[string]any{"movieIds": []int64{movieID}, "monitored": false}, nil)
}

// MovieQueue lists the movie's downloads in progress.
func (r *Radarr) MovieQueue(ctx context.Context, movieID int64) ([]QueueItem, error) {
	var q []QueueItem
	err := r.call(ctx, http.MethodGet, "/api/v3/queue/details?movieId="+id(movieID), nil, &q)
	return q, err
}

// DeleteMovie deletes the movie and its files, and adds an import
// exclusion so no import list adds it back.
func (r *Radarr) DeleteMovie(ctx context.Context, movieID int64) error {
	return r.call(ctx, http.MethodDelete, "/api/v3/movie/"+id(movieID)+"?deleteFiles=true&addImportExclusion=true", nil, nil)
}

// Series is a Sonarr series.
type Series struct {
	ID               int64      `json:"id"`
	Title            string     `json:"title"`
	TvdbID           int64      `json:"tvdbId"`
	TmdbID           int64      `json:"tmdbId"`
	Monitored        bool       `json:"monitored"`
	Path             string     `json:"path"`
	RootFolderPath   string     `json:"rootFolderPath"`
	Added            time.Time  `json:"added"`
	Year             int        `json:"year"`
	ImdbID           string     `json:"imdbId"`
	QualityProfileID int64      `json:"qualityProfileId"`
	AlternateTitles  []AltTitle `json:"alternateTitles"`
	Statistics       struct {
		SizeOnDisk       int64 `json:"sizeOnDisk"`
		EpisodeFileCount int   `json:"episodeFileCount"`
	} `json:"statistics"`
}

// EpisodeFile is a file Sonarr tracks.
type EpisodeFile struct {
	Path string `json:"path"`
}

// EpisodeFiles lists a series' tracked files.
func (s *Sonarr) EpisodeFiles(ctx context.Context, seriesID int64) ([]EpisodeFile, error) {
	var out []EpisodeFile
	err := s.call(ctx, http.MethodGet, "/api/v3/episodefile?seriesId="+id(seriesID), nil, &out)
	return out, err
}

// RenamePreview lists what Sonarr would rename in a series.
func (s *Sonarr) RenamePreview(ctx context.Context, seriesID int64) ([]Rename, error) {
	var out []Rename
	err := s.call(ctx, http.MethodGet, "/api/v3/rename?seriesId="+id(seriesID), nil, &out)
	return out, err
}

// Episode is a Sonarr episode.
type Episode struct {
	ID            int64 `json:"id"`
	SeriesID      int64 `json:"seriesId"`
	SeasonNumber  int   `json:"seasonNumber"`
	EpisodeNumber int   `json:"episodeNumber"`
	EpisodeFileID int64 `json:"episodeFileId"`
	HasFile       bool  `json:"hasFile"`
	Monitored     bool  `json:"monitored"`
}

// Series lists every series.
func (s *Sonarr) Series(ctx context.Context) ([]Series, error) {
	var out []Series
	err := s.call(ctx, http.MethodGet, "/api/v3/series", nil, &out)
	return out, err
}

// Episodes lists a series' episodes.
func (s *Sonarr) Episodes(ctx context.Context, seriesID int64) ([]Episode, error) {
	var out []Episode
	err := s.call(ctx, http.MethodGet, "/api/v3/episode?seriesId="+id(seriesID), nil, &out)
	return out, err
}

// UnmonitorSeries stops Sonarr searching for or grabbing anything in it.
func (s *Sonarr) UnmonitorSeries(ctx context.Context, seriesID int64) error {
	return s.call(ctx, http.MethodPut, "/api/v3/series/editor",
		map[string]any{"seriesIds": []int64{seriesID}, "monitored": false}, nil)
}

// UnmonitorEpisodes stops Sonarr grabbing these episodes again.
func (s *Sonarr) UnmonitorEpisodes(ctx context.Context, episodeIDs []int64) error {
	return s.call(ctx, http.MethodPut, "/api/v3/episode/monitor",
		map[string]any{"episodeIds": episodeIDs, "monitored": false}, nil)
}

// SeriesQueue lists downloads in progress for the series, or only for the
// given episodes if any are given.
func (s *Sonarr) SeriesQueue(ctx context.Context, seriesID int64, episodeIDs []int64) ([]QueueItem, error) {
	q := url.Values{"seriesId": {id(seriesID)}}
	for _, e := range episodeIDs {
		q.Add("episodeIds", id(e))
	}
	var out []QueueItem
	err := s.call(ctx, http.MethodGet, "/api/v3/queue/details?"+q.Encode(), nil, &out)
	return out, err
}

// DeleteSeries deletes the series and its files, and adds an import list
// exclusion so no import list adds it back.
func (s *Sonarr) DeleteSeries(ctx context.Context, seriesID int64) error {
	return s.call(ctx, http.MethodDelete, "/api/v3/series/"+id(seriesID)+"?deleteFiles=true&addImportListExclusion=true", nil, nil)
}

// DeleteEpisodeFile deletes one episode file; the series stays.
func (s *Sonarr) DeleteEpisodeFile(ctx context.Context, episodeFileID int64) error {
	return s.call(ctx, http.MethodDelete, "/api/v3/episodefile/"+id(episodeFileID), nil, nil)
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	hdr := http.Header{}
	hdr.Set("X-Api-Key", c.APIKey)
	return httpjson.Do(ctx, c.HTTP, c.name, method, strings.TrimRight(c.BaseURL, "/")+path, hdr, body, out)
}
