package arr

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// QueueRecord is one item in the download queue. A season pack shows as one
// record per episode, sharing a DownloadID.
type QueueRecord struct {
	ID                    int64   `json:"id"`
	DownloadID            string  `json:"downloadId"`
	Title                 string  `json:"title"` // the release name
	Status                string  `json:"status"`
	TrackedDownloadStatus string  `json:"trackedDownloadStatus"` // "ok", "warning" or "error"
	TrackedDownloadState  string  `json:"trackedDownloadState"`  // "downloading", "importBlocked", "failedPending" and so on
	ErrorMessage          string  `json:"errorMessage"`
	Size                  float64 `json:"size"`
	Sizeleft              float64 `json:"sizeleft"`
	DownloadClient        string  `json:"downloadClient"`
	StatusMessages        []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
	Movie *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"movie"`
	Series *struct {
		Title string `json:"title"`
	} `json:"series"`
	Episode *struct {
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episode"`
}

// Reason says why a download is stuck, in the app's own words.
func (q QueueRecord) Reason() string {
	var parts []string
	if q.ErrorMessage != "" {
		parts = append(parts, q.ErrorMessage)
	}
	for _, m := range q.StatusMessages {
		parts = append(parts, m.Messages...)
		if len(m.Messages) == 0 && m.Title != "" {
			parts = append(parts, m.Title)
		}
	}
	return strings.Join(parts, " ")
}

// Queue lists the download queue with what each item is for.
func (c *Client) Queue(ctx context.Context) ([]QueueRecord, error) {
	var out struct {
		Records []QueueRecord `json:"records"`
	}
	err := c.call(ctx, http.MethodGet, "/api/v3/queue?pageSize=1000&includeMovie=true&includeSeries=true&includeEpisode=true", nil, &out)
	return out.Records, err
}

// RemoveQueue removes queue items and their downloads. blocklist stops that
// release being grabbed again; skipRedownload stops a new search after it.
func (c *Client) RemoveQueue(ctx context.Context, ids []int64, blocklist, skipRedownload bool) error {
	q := url.Values{"removeFromClient": {"true"}, "blocklist": {strconv.FormatBool(blocklist)}, "skipRedownload": {strconv.FormatBool(skipRedownload)}}
	return c.call(ctx, http.MethodDelete, "/api/v3/queue/bulk?"+q.Encode(), map[string]any{"ids": ids}, nil)
}

// DownloadFile is a file in a download that could be imported, and what it
// was identified as.
type DownloadFile struct {
	Path         string         `json:"path"`
	FolderName   string         `json:"folderName"`
	ReleaseGroup string         `json:"releaseGroup"`
	Quality      map[string]any `json:"quality"`
	Languages    []any          `json:"languages"`
	Movie        *struct {
		ID int64 `json:"id"`
	} `json:"movie"`
	Series *struct {
		ID int64 `json:"id"`
	} `json:"series"`
	Episodes []struct {
		ID int64 `json:"id"`
	} `json:"episodes"`
	Rejections []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

// DownloadImports lists a download's files for a manual import.
func (c *Client) DownloadImports(ctx context.Context, downloadID string) ([]DownloadFile, error) {
	q := url.Values{"downloadId": {downloadID}, "filterExistingFiles": {"false"}}
	var out []DownloadFile
	err := c.call(ctx, http.MethodGet, "/api/v3/manualimport?"+q.Encode(), nil, &out)
	return out, err
}
