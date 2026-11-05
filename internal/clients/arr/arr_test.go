package arr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeArr answers GETs from canned bodies (keyed by path) and records every
// call as "METHOD /path?query body".
func fakeArr(t *testing.T, gets map[string]string) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(b)))
		mu.Unlock()
		if r.Method == http.MethodGet {
			body, ok := gets[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRadarr(t *testing.T) {
	u, calls := fakeArr(t, map[string]string{
		"/api/v3/movie": `[{"id":3,"title":"Zoolander","year":2001,"tmdbId":9398,"monitored":true,"hasFile":true,
			"sizeOnDisk":4208219125,"path":"/movies/Zoolander (2001)","rootFolderPath":"/movies","added":"2024-01-02T03:04:05Z"}]`,
		"/api/v3/queue/details": `[{"id":77,"movieId":3,"title":"Zoolander.2001.1080p"}]`,
		"/api/v3/diskspace":     `[{"id":1,"path":"/movies","label":"","freeSpace":100,"totalSpace":1000}]`,
	})
	r := NewRadarr(u, "key", nil)
	ctx := context.Background()

	ms, err := r.Movies(ctx)
	if err != nil || len(ms) != 1 || ms[0].TmdbID != 9398 || ms[0].SizeOnDisk != 4208219125 || ms[0].Added.Year() != 2024 {
		t.Fatalf("movies %+v %v", ms, err)
	}
	q, err := r.MovieQueue(ctx, 3)
	if err != nil || len(q) != 1 || q[0].ID != 77 {
		t.Fatalf("queue %+v %v", q, err)
	}
	d, err := r.DiskSpace(ctx)
	if err != nil || d[0].FreeSpace != 100 || d[0].TotalSpace != 1000 {
		t.Fatalf("disk %+v %v", d, err)
	}
	for _, err := range []error{r.UnmonitorMovie(ctx, 3), r.RemoveFromQueue(ctx, 77), r.DeleteMovie(ctx, 3)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	wantCalls(t, calls(),
		"GET /api/v3/movie?excludeLocalCovers=true",
		"GET /api/v3/queue/details?movieId=3",
		"GET /api/v3/diskspace",
		`PUT /api/v3/movie/editor {"monitored":false,"movieIds":[3]}`,
		"DELETE /api/v3/queue/77?removeFromClient=true&blocklist=false&skipRedownload=true",
		"DELETE /api/v3/movie/3?deleteFiles=true&addImportExclusion=true",
	)
}

func TestSonarr(t *testing.T) {
	u, calls := fakeArr(t, map[string]string{
		"/api/v3/series": `[{"id":17,"title":"Game of Thrones","tvdbId":121361,"tmdbId":1399,"monitored":true,
			"path":"/tv/Game of Thrones","rootFolderPath":"/tv","added":"2023-05-06T00:00:00Z","statistics":{"sizeOnDisk":900}}]`,
		"/api/v3/episode":       `[{"id":501,"seriesId":17,"seasonNumber":6,"episodeNumber":1,"episodeFileId":9001,"hasFile":true,"monitored":true}]`,
		"/api/v3/queue/details": `[{"id":88,"seriesId":17,"episodeId":501,"title":"GoT.S06E01"}]`,
	})
	s := NewSonarr(u, "key", nil)
	ctx := context.Background()

	ss, err := s.Series(ctx)
	if err != nil || len(ss) != 1 || ss[0].TvdbID != 121361 || ss[0].Statistics.SizeOnDisk != 900 {
		t.Fatalf("series %+v %v", ss, err)
	}
	eps, err := s.Episodes(ctx, 17)
	if err != nil || eps[0].EpisodeFileID != 9001 || eps[0].SeasonNumber != 6 {
		t.Fatalf("episodes %+v %v", eps, err)
	}
	q, err := s.SeriesQueue(ctx, 17, []int64{501, 502})
	if err != nil || q[0].EpisodeID != 501 {
		t.Fatalf("queue %+v %v", q, err)
	}
	for _, err := range []error{
		s.UnmonitorSeries(ctx, 17), s.UnmonitorEpisodes(ctx, []int64{501}),
		s.DeleteEpisodeFile(ctx, 9001), s.DeleteSeries(ctx, 17),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	wantCalls(t, calls(),
		"GET /api/v3/series",
		"GET /api/v3/episode?seriesId=17",
		"GET /api/v3/queue/details?episodeIds=501&episodeIds=502&seriesId=17",
		`PUT /api/v3/series/editor {"monitored":false,"seriesIds":[17]}`,
		`PUT /api/v3/episode/monitor {"episodeIds":[501],"monitored":false}`,
		"DELETE /api/v3/episodefile/9001",
		"DELETE /api/v3/series/17?deleteFiles=true&addImportListExclusion=true",
	)
}

func TestErrorHidesQuery(t *testing.T) {
	u, _ := fakeArr(t, nil)
	r := NewRadarr(u, "wrong", nil)
	err := r.DeleteMovie(context.Background(), 3)
	if err == nil || strings.Contains(err.Error(), "deleteFiles") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("got %v", err)
	}
}
