package sync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

type fakes struct {
	users    []seerr.User
	reqs     []seerr.Request
	secs     []plex.Section
	items    map[string][]plex.Item
	movies   []arr.Movie
	series   []arr.Series
	plays    []tautulli.Play
	afters   []time.Time
	failArr  bool
	failPlex bool
}

func (f *fakes) Users(context.Context) ([]seerr.User, error)       { return f.users, nil }
func (f *fakes) Requests(context.Context) ([]seerr.Request, error) { return f.reqs, nil }
func (f *fakes) Sections(context.Context) ([]plex.Section, error)  { return f.secs, nil }
func (f *fakes) Items(_ context.Context, k string) ([]plex.Item, error) {
	if f.failPlex {
		return nil, errors.New("plex down")
	}
	return f.items[k], nil
}

func (f *fakes) Movies(context.Context) ([]arr.Movie, error) {
	if f.failArr {
		return nil, errors.New("radarr down")
	}
	return f.movies, nil
}
func (f *fakes) Series(context.Context) ([]arr.Series, error) { return f.series, nil }
func (f *fakes) History(_ context.Context, after time.Time) ([]tautulli.Play, error) {
	f.afters = append(f.afters, after)
	return f.plays, nil
}

func ptr[T any](v T) *T { return &v }

func item(key, title, guid string) plex.Item {
	it := plex.Item{RatingKey: key, Title: title, Year: 2001}
	it.Guids = append(it.Guids, struct {
		ID string `json:"id"`
	}{guid})
	return it
}

func setup(t *testing.T) (*Syncer, *fakes) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &fakes{
		users: []seerr.User{{ID: 1, Username: "admin", PlexID: ptr[int64](500), Permissions: 2}, {ID: 7, Email: "local@x"}},
		secs:  []plex.Section{{Key: "1", Type: "movie"}, {Key: "2", Type: "show"}, {Key: "3", Type: "artist"}},
		items: map[string][]plex.Item{
			"1": {item("10", "Zoolander", "tmdb://9398"), item("11", "Orphan Movie", "tmdb://1")},
			"2": {item("20", "Some Show", "tvdb://299")},
			"3": {item("30", "Album", "")},
		},
		movies: []arr.Movie{{ID: 101, TmdbID: 9398, SizeOnDisk: 5 << 30}},
		series: []arr.Series{{ID: 201, TvdbID: 299}},
		reqs: []seerr.Request{
			{
				ID: 1, Status: 2, CreatedAt: time.Unix(100, 0), RequestedBy: seerr.User{ID: 7},
				Media: seerr.Media{ID: 50, MediaType: "movie", TmdbID: 9398},
			}, // matched by TMDB
			{
				ID: 2, Status: 2, RequestedBy: seerr.User{ID: 7},
				Media: seerr.Media{ID: 51, MediaType: "tv", TmdbID: 1399, TvdbID: ptr[int64](299)},
			}, // by TVDB
			{
				ID: 3, Status: 2, RequestedBy: seerr.User{ID: 1},
				Media: seerr.Media{ID: 52, MediaType: "movie", TmdbID: 777, RatingKey: ptr("11")},
			}, // by rating key
			{
				ID: 4, Status: 1, RequestedBy: seerr.User{ID: 1},
				Media: seerr.Media{ID: 53, MediaType: "movie", TmdbID: 888},
			}, // not in Plex
		},
		plays: []tautulli.Play{
			{
				ReferenceID: 1, UserID: 500, User: "admin", MediaType: "episode", RatingKey: 2001, GrandparentRatingKey: 20,
				ParentMediaIndex: 1, MediaIndex: 3, PercentComplete: 95, WatchedStatus: 1, Stopped: 1_800_000_000,
			},
			{ReferenceID: 2, UserID: 999, User: "stranger", MediaType: "movie", RatingKey: 10, PercentComplete: 30, Stopped: 1_700_000_000},
		},
	}
	return &Syncer{DB: db, Seerr: f, Plex: f, Radarr: f, Sonarr: f, Tautulli: f}, f
}

func TestRunMatchesEverything(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	if !s.Run(ctx) {
		t.Fatal("run refused")
	}
	var arrID, size int64
	if err := s.DB.QueryRow(`SELECT arr_id, size_bytes FROM media WHERE rating_key='10'`).Scan(&arrID, &size); err != nil || arrID != 101 || size != 5<<30 {
		t.Fatalf("movie match %d %d %v", arrID, size, err)
	}
	var n int
	_ = s.DB.QueryRow(`SELECT count(*) FROM media`).Scan(&n)
	if n != 3 {
		t.Fatalf("media rows %d, want 3 (music skipped)", n)
	}
	got := map[int64]string{}
	rows, _ := s.DB.Query(`SELECT seerr_request_id, coalesce(rating_key, '-') FROM requests`)
	for rows.Next() {
		var id int64
		var k string
		_ = rows.Scan(&id, &k)
		got[id] = k
	}
	_ = rows.Close()
	if got[1] != "10" || got[2] != "20" || got[3] != "11" || got[4] != "-" {
		t.Fatalf("request matches %v", got)
	}
	counts, items, err := s.DB.SyncReport(ctx, 10)
	if err != nil || counts["Plex item not in Radarr/Sonarr"] != 1 || counts["Seerr request not in Plex"] != 1 ||
		counts["Tautulli user not linked to a Seerr user"] != 1 || len(items) != 3 {
		t.Fatalf("report %v %v %v", counts, items, err)
	}
	st, _ := s.DB.SyncStatuses(ctx)
	for _, x := range st {
		if x.LastError != "" || !x.LastOK.Valid {
			t.Fatalf("status %+v", x)
		}
	}
	if !f.afters[0].Equal(time.Unix(0, 0)) {
		t.Fatalf("first history read from %v, want everything", f.afters[0])
	}
	// The second run re-reads from a day before the newest play.
	s.Run(ctx)
	if want := time.Unix(1_800_000_000, 0).AddDate(0, 0, -1); !f.afters[1].Equal(want) {
		t.Fatalf("second history read from %v, want %v", f.afters[1], want)
	}
}

func TestFailedSourceKeepsLastGoodCopy(t *testing.T) {
	for name, fail := range map[string]func(*fakes){
		"radarr": func(f *fakes) { f.failArr = true },
		"plex":   func(f *fakes) { f.failPlex = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, f := setup(t)
			ctx := context.Background()
			s.Run(ctx)
			fail(f)
			f.items = nil // a successful sync would now empty the table
			s.Run(ctx)
			var n int
			_ = s.DB.QueryRow(`SELECT count(*) FROM media`).Scan(&n)
			if n != 3 {
				t.Fatalf("media rows %d after a failed sync, want the old 3", n)
			}
			st, _ := s.DB.SyncStatuses(ctx)
			for _, x := range st {
				if x.Source == SrcLibrary && (x.LastError == "" || x.Items != 3) {
					t.Fatalf("library status %+v", x)
				}
			}
		})
	}
}

func TestRunIsExclusive(t *testing.T) {
	s, _ := setup(t)
	s.mu.Lock()
	if s.Run(context.Background()) {
		t.Fatal("second concurrent run should be refused")
	}
	s.mu.Unlock()
}
