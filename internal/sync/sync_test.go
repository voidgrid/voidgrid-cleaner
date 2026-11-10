package sync

import (
	"context"
	"encoding/json"
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
	users     []seerr.User
	reqs      []seerr.Request
	secs      []plex.Section
	items     map[string][]plex.Item
	movies    []arr.Movie
	series    []arr.Series
	plays     []tautulli.Play
	afters    []time.Time
	failArr   bool
	failPlex  bool
	views     []plex.View
	failViews bool
	showPaths map[string]string
}

func (f *fakes) ShowPath(_ context.Context, key string) (string, error) { return f.showPaths[key], nil }

func (f *fakes) ViewHistory(context.Context) ([]plex.View, error) {
	if f.failViews {
		return nil, errors.New("plex history down")
	}
	return f.views, nil
}

func (f *fakes) Accounts(context.Context) ([]plex.ServerAccount, error) {
	return []plex.ServerAccount{{ID: 1, Name: "owner"}, {ID: 77, Name: "kid"}}, nil
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
		secs:  []plex.Section{{Key: "2", Type: "show", Title: "TV"}, {Key: "1", Type: "movie", Title: "Films"}, {Key: "3", Type: "artist", Title: "Music"}},
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
	owner := func(context.Context) (int64, error) { return 500, nil }
	return &Syncer{DB: db, Seerr: f, Plex: f, Radarr: f, Sonarr: f, Tautulli: f, Owner: owner}, f
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
	secs, err := s.DB.Sections(ctx)
	if err != nil || len(secs) != 2 || secs[0].Title != "TV" || secs[1].Key != "1" || secs[1].Type != "movie" {
		t.Fatalf("sections %+v %v (want TV then Films, in Plex's order, no music)", secs, err)
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
			if secs, _ := s.DB.Sections(ctx); len(secs) != 2 {
				t.Fatalf("sections %+v after a failed sync, want the old 2", secs)
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

func TestIgnoredLibrarySkipped(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	s.Run(ctx)
	if found, err := s.DB.SetSectionIgnored(ctx, "1", true); err != nil || !found {
		t.Fatalf("ignore: %v %v", found, err)
	}
	media := func() int {
		var n int
		_ = s.DB.QueryRow(`SELECT count(*) FROM media`).Scan(&n)
		return n
	}
	if n := media(); n != 1 {
		t.Fatalf("media rows %d right after ignoring, want only the show", n)
	}
	s.Run(ctx)
	if n := media(); n != 1 {
		t.Fatalf("media rows %d after a sync, want only the show", n)
	}
	secs, _ := s.DB.Sections(ctx)
	if len(secs) != 2 || secs[1].Key != "1" || !secs[1].Ignored {
		t.Fatalf("sections %+v, want the ignored library kept and flagged", secs)
	}
	if _, err := s.DB.SetSectionIgnored(ctx, "1", false); err != nil {
		t.Fatal(err)
	}
	f.items["1"] = []plex.Item{item("10", "Zoolander", "tmdb://9398")}
	s.Run(ctx)
	if n := media(); n != 2 {
		t.Fatalf("media rows %d after unignoring, want the library back", n)
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

func TestPlexHistory(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	f.secs = []plex.Section{{Key: "1", Type: "movie", Title: "Movies"}, {Key: "2", Type: "show", Title: "TV"}}
	f.items = map[string][]plex.Item{"1": {{RatingKey: "10", Type: "movie", Title: "Film"}}, "2": {{RatingKey: "20", Type: "show", Title: "Show"}}}
	f.views = []plex.View{
		{HistoryKey: "/status/sessions/history/1", RatingKey: "10", Type: "movie", ViewedAt: 100, AccountID: 1},
		{HistoryKey: "/status/sessions/history/2", RatingKey: "21", GrandparentKey: "/library/metadata/20", Type: "episode", ParentIndex: 1, Index: 2, ViewedAt: 200, AccountID: 77},
		{HistoryKey: "/status/sessions/history/3", Type: "episode", ViewedAt: 300, AccountID: 77}, // removed since
		{HistoryKey: "/status/sessions/history/4", RatingKey: "30", Type: "track", ViewedAt: 400, AccountID: 77},
	}
	f.plays = []tautulli.Play{{ReferenceID: 9, RatingKey: 10, MediaType: "movie", Stopped: 50, UserID: 500}}
	s.Run(ctx)
	var n, owner int
	if err := s.DB.QueryRow(`SELECT count(*), sum(plex_user_id = 500) FROM plays WHERE reference_id < 0`).Scan(&n, &owner); err != nil {
		t.Fatal(err)
	}
	if n != 2 || owner != 1 {
		t.Fatalf("plex rows %d, owner rows %d", n, owner)
	}
	var season, episode int
	var show, name string
	if err := s.DB.QueryRow(`SELECT season, episode, show_rating_key, user_name FROM plays WHERE reference_id = -2`).Scan(&season, &episode, &show, &name); err != nil {
		t.Fatal(err)
	}
	if season != 1 || episode != 2 || show != "20" || name != "kid" {
		t.Fatal(season, episode, show, name)
	}
	items, err := s.DB.LibraryItems(ctx, storage.User{IsAdmin: true}, "1", storage.LibraryFilter{})
	if err != nil || len(items) != 1 || items[0].LastWatched != 100 {
		t.Fatalf("%+v %v", items, err)
	}
	// Tautulli's full history doesn't touch Plex's rows, and the reverse.
	if err := s.DB.ReplacePlays(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT count(*) FROM plays`).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if last, err := s.DB.LastPlayStopped(ctx); err != nil || !last.IsZero() {
		t.Fatal("plex rows moved tautulli's sync point", last, err)
	}
}

func TestPlexHistoryFailureKeepsOldRows(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	f.views = []plex.View{{HistoryKey: "/status/sessions/history/1", RatingKey: "10", Type: "movie", ViewedAt: 100, AccountID: 77}}
	s.Run(ctx)
	f.failViews = true
	s.Run(ctx)
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM plays WHERE reference_id < 0`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// A wrong Plex match must not make an item Radarr or Sonarr manages look
// Plex-only: it would be deleted through Plex and could download again.
func TestWrongPlexMatchStillFindsArrByPath(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	movie := plex.Item{RatingKey: "10", Type: "movie", Title: "Wrong Film", Guids: nil}
	if err := json.Unmarshal([]byte(`{"ratingKey":"10","type":"movie","title":"Wrong Film","Guid":[{"id":"tmdb://999"}],"Media":[{"Part":[{"file":"/m/Right (2000)/Right.mkv"}]}]}`), &movie); err != nil {
		t.Fatal(err)
	}
	f.secs = []plex.Section{{Key: "1", Type: "movie", Title: "Movies"}, {Key: "2", Type: "show", Title: "TV"}}
	f.items = map[string][]plex.Item{"1": {movie}, "2": {{RatingKey: "20", Type: "show", Title: "Wrong Show"}}}
	f.movies = []arr.Movie{{ID: 7, TmdbID: 100, Path: "/m/Right (2000)", SizeOnDisk: 5}}
	f.series = []arr.Series{{ID: 8, TvdbID: 200, Path: "/t/Right"}}
	f.showPaths = map[string]string{"20": "/t/Right"}
	s.Run(ctx)
	admin := storage.User{IsAdmin: true}
	movies, err := s.DB.LibraryItems(ctx, admin, "1", storage.LibraryFilter{})
	if err != nil || len(movies) != 1 || movies[0].ArrID.Int64 != 7 || movies[0].TmdbID.Int64 != 100 || movies[0].SizeBytes != 5 {
		t.Fatalf("%+v %v", movies, err)
	}
	shows, err := s.DB.LibraryItems(ctx, admin, "2", storage.LibraryFilter{})
	if err != nil || len(shows) != 1 || shows[0].ArrID.Int64 != 8 || shows[0].TvdbID.Int64 != 200 {
		t.Fatalf("%+v %v", shows, err)
	}
}

func TestUserGoneFromSeerrIsSignedOut(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	s.Run(ctx)
	gone, err := s.DB.CreateSession(ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stays, err := s.DB.CreateSession(ctx, 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.users = f.users[1:] // user 1 removed in Seerr
	s.Run(ctx)
	if _, err := s.DB.SessionUser(ctx, gone); !errors.Is(err, storage.ErrNoSession) {
		t.Fatalf("removed user's session: %v", err)
	}
	if _, err := s.DB.SessionUser(ctx, stays); err != nil {
		t.Fatalf("other user's session: %v", err)
	}
	// A reply with nobody in it is a bad reply: nobody is signed out.
	f.users = nil
	s.Run(ctx)
	if _, err := s.DB.SessionUser(ctx, stays); err != nil {
		t.Fatalf("session after an empty user list: %v", err)
	}
}

func TestNoLibrariesFromPlexKeepsIgnoredFlag(t *testing.T) {
	s, f := setup(t)
	ctx := context.Background()
	s.Run(ctx)
	if _, err := s.DB.SetSectionIgnored(ctx, "1", true); err != nil {
		t.Fatal(err)
	}
	secs := f.secs
	f.secs = nil
	s.Run(ctx)
	f.secs = secs
	s.Run(ctx)
	got, _ := s.DB.Sections(ctx)
	if len(got) != 2 || got[1].Key != "1" || !got[1].Ignored {
		t.Fatalf("sections %+v, want library 1 still ignored", got)
	}
}

// The file is in one Radarr movie's folder and Plex matched it as another
// movie Radarr has: the folder wins, or a deletion would act on the wrong one.
func TestFolderBeatsPlexIDWhenBothMatchRadarr(t *testing.T) {
	s, f := setup(t)
	var it plex.Item
	if err := json.Unmarshal([]byte(`{"ratingKey":"10","type":"movie","title":"Y","Guid":[{"id":"tmdb://200"}],"Media":[{"Part":[{"file":"/m/X (2000)/X.mkv"}]}]}`), &it); err != nil {
		t.Fatal(err)
	}
	f.secs = []plex.Section{{Key: "1", Type: "movie", Title: "Movies"}}
	f.items = map[string][]plex.Item{"1": {it}}
	f.movies = []arr.Movie{{ID: 7, TmdbID: 100, Path: "/m/X (2000)"}, {ID: 8, TmdbID: 200, Path: "/m/Y (2001)"}}
	s.Run(context.Background())
	var id, tmdb int64
	if err := s.DB.QueryRow(`SELECT arr_id, tmdb_id FROM media WHERE rating_key = '10'`).Scan(&id, &tmdb); err != nil || id != 7 || tmdb != 100 {
		t.Fatalf("linked to Radarr movie %d (tmdb %d) %v, want 7 (100)", id, tmdb, err)
	}
}
