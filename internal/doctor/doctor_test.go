package doctor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

type fakeArr struct {
	state  arrState
	fail   error
	renErr error
	rename []arr.Rename

	commands []map[string]any
	status   string

	queue         []arr.QueueRecord
	queueErr      error
	removed       []string
	clearOnRemove bool
	imports       []arr.DownloadFile
}

func (f *fakeArr) RootFolders(context.Context) ([]arr.RootFolder, error) {
	return f.state.roots, f.fail
}

func (f *fakeArr) DiskSpace(context.Context) ([]arr.DiskSpace, error) { return f.state.disks, nil }

func (f *fakeArr) Health(context.Context) ([]arr.Health, error) { return f.state.health, nil }

func (f *fakeArr) MediaManagement(context.Context) (arr.MediaManagement, error) {
	return f.state.settings, nil
}

func (f *fakeArr) Queue(context.Context) ([]arr.QueueRecord, error) { return f.queue, f.queueErr }
func (f *fakeArr) RemoveQueue(_ context.Context, ids []int64, blocklist, skip bool) error {
	f.removed = append(f.removed, fmt.Sprintf("%v blocklist=%v skip=%v", ids, blocklist, skip))
	if f.clearOnRemove {
		f.queue = nil
	}
	return nil
}

func (f *fakeArr) DownloadImports(context.Context, string) ([]arr.DownloadFile, error) {
	return f.imports, nil
}

func (f *fakeArr) Command(_ context.Context, body map[string]any) (int64, error) {
	f.commands = append(f.commands, body)
	return 1, nil
}

func (f *fakeArr) CommandStatus(context.Context, int64) (string, string, error) {
	if f.status == "" {
		return "completed", "", nil
	}
	return f.status, "boom", nil
}

type fakeRadarr struct {
	fakeArr
	movies []arr.Movie

	gotFile        bool
	added          map[string]any
	files          []arr.ImportFile
	savedSettings  map[string]any
	ignoreSettings bool
	calls          []string
}

func (f *fakeRadarr) Movies(context.Context) ([]arr.Movie, error) { return f.movies, nil }
func (f *fakeRadarr) RenamePreview(context.Context, []int64) ([]arr.Rename, error) {
	return f.rename, f.renErr
}

func (f *fakeRadarr) Movie(_ context.Context, id int64) (arr.Movie, error) {
	return arr.Movie{ID: id, Title: "Added", HasFile: f.gotFile}, nil
}

func (f *fakeRadarr) LookupMovies(_ context.Context, term string) ([]map[string]any, error) {
	if strings.HasPrefix(term, "tmdb:") {
		return []map[string]any{{"title": "Found", "tmdbId": float64(7), "id": float64(9), "folder": "x"}}, nil
	}
	return []map[string]any{{"title": "Found", "year": float64(2001), "tmdbId": float64(7)}}, nil
}

func (f *fakeRadarr) AddMovie(_ context.Context, m map[string]any) (int64, error) {
	f.added = m
	return 5, nil
}

func (f *fakeRadarr) QualityDefinitions(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"id": float64(3), "name": "WEBDL-1080p"}}, nil
}

func (f *fakeRadarr) ImportFiles(context.Context, int64, string) ([]arr.ImportFile, error) {
	return f.files, nil
}

func (f *fakeRadarr) UnmonitorMovie(_ context.Context, id int64) error {
	f.calls = append(f.calls, fmt.Sprintf("unmonitor %d", id))
	return nil
}

func (f *fakeRadarr) MonitorMovie(_ context.Context, id int64) error {
	f.calls = append(f.calls, fmt.Sprintf("monitor %d", id))
	return nil
}

func (f *fakeRadarr) RemoveMovie(_ context.Context, id int64) error {
	f.calls = append(f.calls, fmt.Sprintf("remove %d", id))
	return nil
}

func (f *fakeRadarr) MediaManagementRaw(context.Context) (map[string]any, error) {
	return map[string]any{"keepMe": "yes", "deleteEmptyFolders": false, "createEmptyMovieFolders": true}, nil
}

func (f *fakeRadarr) SetMediaManagement(_ context.Context, s map[string]any) error {
	f.savedSettings = s
	if !f.ignoreSettings {
		f.state.settings.DeleteEmptyFolders, f.state.settings.CreateEmptyMovieFolders = s["deleteEmptyFolders"] == true, s["createEmptyMovieFolders"] == true
	}
	return nil
}

type fakeSonarr struct {
	fakeArr
	series   []arr.Series
	files    []arr.EpisodeFile
	filesErr error
	added    map[string]any

	episodes  []arr.MissingEpisode
	fileCount int
	calls     []string
}

func (f *fakeSonarr) Series(context.Context) ([]arr.Series, error) { return f.series, nil }
func (f *fakeSonarr) RenamePreview(context.Context, int64) ([]arr.Rename, error) {
	return nil, nil
}

func (f *fakeSonarr) EpisodeFiles(context.Context, int64) ([]arr.EpisodeFile, error) {
	return f.files, f.filesErr
}

func (f *fakeSonarr) LookupSeries(context.Context, string) ([]map[string]any, error) {
	return []map[string]any{{"title": "Found Show", "year": float64(2001), "tvdbId": float64(8), "seasons": []any{map[string]any{"seasonNumber": float64(1), "monitored": true}}}}, nil
}

func (f *fakeSonarr) MissingEpisodes(context.Context) ([]arr.MissingEpisode, error) {
	return f.episodes, nil
}

func (f *fakeSonarr) OneSeries(_ context.Context, id int64) (arr.Series, error) {
	s := arr.Series{ID: id}
	s.Statistics.EpisodeFileCount = f.fileCount
	return s, nil
}

func (f *fakeSonarr) UnmonitorSeries(_ context.Context, id int64) error {
	f.calls = append(f.calls, fmt.Sprintf("unmonitor series %d", id))
	return nil
}

func (f *fakeSonarr) UnmonitorEpisodes(_ context.Context, ids []int64) error {
	f.calls = append(f.calls, fmt.Sprintf("unmonitor episodes %v", ids))
	return nil
}

func (f *fakeSonarr) RemoveSeries(_ context.Context, id int64) error {
	f.calls = append(f.calls, fmt.Sprintf("remove series %d", id))
	return nil
}

func (f *fakeSonarr) AddSeries(_ context.Context, m map[string]any) (int64, error) {
	f.added = m
	return 6, nil
}

type fakePlex struct {
	secs  []plex.Section
	items map[string][]plex.Item
	local bool
	fail  error

	matches    []plex.Match
	calls      []string
	takes      bool // a match applies
	guidAfter  string
	stuckLocal bool
}

func (f *fakePlex) Sections(context.Context) ([]plex.Section, error) { return f.secs, f.fail }
func (f *fakePlex) Items(_ context.Context, k string) ([]plex.Item, error) {
	return f.items[k], nil
}
func (f *fakePlex) ShowPath(context.Context, string) (string, error) { return "", nil }
func (f *fakePlex) Item(_ context.Context, k string) (plex.Item, error) {
	for _, items := range f.items {
		for _, it := range items {
			if it.RatingKey == k {
				if f.guidAfter != "" {
					it.PlexGUID = f.guidAfter
				}
				return it, nil
			}
		}
	}
	return plex.Item{}, errors.New("no item")
}

func (f *fakePlex) Matches(context.Context, string, string, int, string) ([]plex.Match, error) {
	return f.matches, nil
}

func (f *fakePlex) Match(_ context.Context, key, guid, name string) error {
	f.calls = append(f.calls, "match "+key+" "+guid+" "+name)
	if f.takes {
		f.guidAfter = guid
	}
	return nil
}

func (f *fakePlex) SetPreferLocalMetadata(_ context.Context, k string, on bool) error {
	f.calls = append(f.calls, fmt.Sprintf("prefs %s %v", k, on))
	f.local = on || f.stuckLocal
	return nil
}

func (f *fakePlex) RefreshItem(_ context.Context, k string) error {
	f.calls = append(f.calls, "refresh item "+k)
	return nil
}

func (f *fakePlex) RefreshMetadata(_ context.Context, k string) error {
	f.calls = append(f.calls, "refresh "+k)
	return nil
}

func (f *fakePlex) PreferLocalMetadata(context.Context, string) (bool, error) {
	return f.local, nil
}

type fakeSeerr struct {
	deleted []int64
	err     error
	noTitle bool
}

func (f *fakeSeerr) Title(context.Context, string, int64) (string, error) {
	if f.noTitle {
		return "", errors.New("404")
	}
	return "Gone Film", nil
}

func (f *fakeSeerr) DeleteMedia(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return f.err
}

func item(t *testing.T, js string) plex.Item {
	t.Helper()
	var it plex.Item
	if err := json.Unmarshal([]byte(js), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

type world struct {
	d      *Doctor
	radarr *fakeRadarr
	sonarr *fakeSonarr
	plex   *fakePlex
	seerr  *fakeSeerr
	root   string
}

// newWorld is a movies root with one real file, the shape of a healthy mount.
func newWorld(t *testing.T) *world {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "movies")
	film := filepath.Join(root, "Film (2000)")
	if err := os.MkdirAll(film, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(film, "Film.mkv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &world{root: root, seerr: &fakeSeerr{}}
	w.radarr = &fakeRadarr{movies: []arr.Movie{{ID: 1, Title: "Film", Year: 2000, TmdbID: 100, Path: film, HasFile: true, QualityProfileID: 4, MovieFile: &struct {
		RelativePath string `json:"relativePath"`
	}{"Film.mkv"}}}}
	w.radarr.state.roots = []arr.RootFolder{{Path: root}}
	w.radarr.state.settings.RecycleBin = "/bin"
	w.sonarr = &fakeSonarr{}
	w.sonarr.state.settings.RecycleBin = "/bin"
	w.plex = &fakePlex{
		secs: []plex.Section{{Key: "1", Type: "movie", Title: "Movies", Location: []struct {
			Path string `json:"path"`
		}{{Path: root}}}},
		items: map[string][]plex.Item{"1": {item(t, `{"ratingKey":"10","title":"Film","year":2000,"guid":"plex://movie/1","Guid":[{"id":"tmdb://100"}],"Media":[{"Part":[{"file":"`+filepath.Join(film, "Film.mkv")+`"}]}]}`)}},
	}
	w.d = &Doctor{DB: db, Radarr: w.radarr, Sonarr: w.sonarr, Plex: w.plex, Seerr: w.seerr}
	return w
}

func (w *world) open(t *testing.T, kind string) []storage.Finding {
	t.Helper()
	all, err := w.d.DB.Findings(context.Background(), "open")
	if err != nil {
		t.Fatal(err)
	}
	var out []storage.Finding
	for _, f := range all {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func (w *world) run(t *testing.T) {
	t.Helper()
	if !w.d.Run(context.Background()) {
		t.Fatal("didn't run")
	}
}

func TestHealthyLibraryHasNoFindings(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	if fs, _ := w.d.DB.Findings(context.Background(), "open"); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	ps, _ := w.d.DB.LibraryPaths(context.Background())
	if len(ps) != 1 || !ps[0].Visible || !ps[0].Verified || !ps[0].Media || ps[0].UsedBy != "plex, radarr" {
		t.Fatalf("%+v", ps)
	}
}

func TestPathChecks(t *testing.T) {
	w := newWorld(t)
	w.radarr.state.disks = []arr.DiskSpace{{Path: "/data/nowhere", TotalSpace: 1}, {Path: "/config", TotalSpace: 1}, {Path: "/", TotalSpace: 1}}
	w.run(t)
	if fs := w.open(t, KindPathNotMounted); len(fs) != 1 || fs[0].Subject != "/data/nowhere" {
		t.Fatalf("%+v", fs)
	}
}

// A mount that didn't come up looks like an empty folder. It must not pass as
// verified, or every file would look like it had vanished.
func TestEmptyMountpointIsNotVerified(t *testing.T) {
	w := newWorld(t)
	if err := os.RemoveAll(filepath.Join(w.root, "Film (2000)")); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if fs := w.open(t, KindPathNotVerified); len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	ps, _ := w.d.DB.LibraryPaths(context.Background())
	if ps[0].Verified || !ps[0].Visible {
		t.Fatalf("%+v", ps)
	}
}

func TestWrongMatch(t *testing.T) {
	w := newWorld(t)
	film := filepath.Join(w.root, "Film (2000)", "Film.mkv")
	w.plex.items["1"] = []plex.Item{item(t, `{"ratingKey":"10","title":"Other","year":2000,"guid":"plex://movie/2","Guid":[{"id":"tmdb://555"}],"Media":[{"Part":[{"file":"`+film+`"}]}]}`)}
	w.run(t)
	if fs := w.open(t, KindWrongMatch); len(fs) != 1 || !strings.Contains(fs[0].Detail, `"Film"`) {
		t.Fatalf("%+v", fs)
	}
	if fs := w.open(t, KindQuestionable); len(fs) != 0 {
		t.Fatalf("reported twice: %+v", fs)
	}
}

func TestQuestionableMatches(t *testing.T) {
	for name, c := range map[string]struct {
		plexTitle string
		plexYear  int
		arrTitle  string
		alts      []arr.AltTitle
		orig      string
		want      int
	}{
		"same":                                  {"Film", 2000, "Film", nil, "", 0},
		"punctuation and the":                   {"The Film!", 2000, "Film", nil, "", 0},
		"different title":                       {"Completely Else", 2000, "Film Night", nil, "", 1},
		"alternate title counts":                {"Fuck Me", 2000, "Baise-moi", []arr.AltTitle{{Title: "Fuck Me"}}, "", 0},
		"original title counts":                 {"Fuck Me", 2000, "Baise-moi", nil, "Baise-moi", 0},
		"a year apart with the same ID is fine": {"Film", 2020, "Film", nil, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			film := filepath.Join(w.root, "Film (2000)", "Film.mkv")
			it := item(t, `{"ratingKey":"10","guid":"plex://movie/1","Guid":[{"id":"tmdb://100"}],"Media":[{"Part":[{"file":"`+film+`"}]}]}`)
			it.Title, it.Year = c.plexTitle, c.plexYear
			w.plex.items["1"] = []plex.Item{it}
			m := &w.radarr.movies[0]
			m.Title, m.AlternateTitles, m.Path = c.arrTitle, c.alts, filepath.Dir(film)
			if c.orig != "" {
				it.OriginalTitle = c.orig
				w.plex.items["1"] = []plex.Item{it}
			}
			if name != "same" && strings.Contains(name, "title") { // the folder name must not rescue a real mismatch
				m.Path = filepath.Dir(film)
			}
			w.run(t)
			if fs := w.open(t, KindQuestionable); len(fs) != c.want {
				t.Fatalf("%+v", fs)
			}
		})
	}
}

func TestUnmatchedItemIsQuestionable(t *testing.T) {
	w := newWorld(t)
	w.plex.items["1"] = append(w.plex.items["1"], item(t, `{"ratingKey":"11","title":"Home Video","guid":"local://11"}`))
	w.run(t)
	if fs := w.open(t, KindQuestionable); len(fs) != 1 || fs[0].Severity != "problem" {
		t.Fatalf("%+v", fs)
	}
}

func TestReleaseTitleAndFileNotAccepted(t *testing.T) {
	w := newWorld(t)
	w.plex.items["1"][0].Title = "Film.2000.1080p.BluRay.x265-RARBG"
	w.radarr.movies[0].HasFile, w.radarr.movies[0].MovieFile = false, nil
	w.run(t)
	if fs := w.open(t, KindReleaseTitle); len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	if fs := w.open(t, KindNotAccepted); len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	if fs := w.open(t, KindQuestionable); len(fs) != 0 {
		t.Fatalf("a release name isn't also a bad match: %+v", fs)
	}
}

func TestDiskChecksOnlyInManagedVerifiedPaths(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	empty, stray := filepath.Join(w.root, "Empty (2001)"), filepath.Join(w.root, "Film (2000)", "extra.m2ts")
	if err := os.Mkdir(empty, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.radarr.state.roots[0].UnmappedFolders = []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{{Name: "Empty (2001)", Path: empty}}
	w.run(t)
	if n := len(w.open(t, KindEmptyFolder)) + len(w.open(t, KindStrayFile)); n != 0 {
		t.Fatal("looked inside an unmanaged path")
	}
	if fs := w.open(t, KindUntracked); len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	if err := w.d.DB.SetManagedPaths(ctx, []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if fs := w.open(t, KindEmptyFolder); len(fs) != 1 || fs[0].Subject != empty {
		t.Fatalf("%+v", fs)
	}
	if fs := w.open(t, KindStrayFile); len(fs) != 1 || fs[0].Subject != stray {
		t.Fatalf("%+v", fs)
	}
	if fs := w.open(t, KindUntracked); len(fs) != 0 {
		t.Fatalf("an empty folder isn't also untracked: %+v", fs)
	}
}

func TestStaleRequest(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.d.DB.ReplaceRequests(ctx, []storage.Request{
		{SeerrRequestID: 1, SeerrMediaID: 7, SeerrUserID: 1, MediaType: "movie", TmdbID: 42, Status: 5},
		{SeerrRequestID: 2, SeerrMediaID: 8, SeerrUserID: 1, MediaType: "movie", TmdbID: 100, Status: 5}, // Radarr has it
	}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if fs := w.open(t, KindStaleRequest); len(fs) != 1 || fs[0].Subject != "7" || fs[0].Title != "Gone Film" {
		t.Fatalf("%+v", fs)
	}
}

func TestSettingsAndReports(t *testing.T) {
	w := newWorld(t)
	w.radarr.state.settings = arr.MediaManagement{CreateEmptyMovieFolders: true}
	w.radarr.state.health = []arr.Health{{Source: "IndexerCheck", Type: "error", Message: "no indexers", WikiURL: "https://wiki/x"}}
	w.sonarr.episodes = []arr.MissingEpisode{{ID: 1, SeriesID: 3, SeasonNumber: 7, EpisodeNumber: 2, Title: "Chrysalis"}}
	w.sonarr.episodes[0].Series.Title = "Miraculous"
	w.plex.local = true
	w.radarr.rename = []arr.Rename{{ExistingPath: "/m/a.mkv", NewPath: "/m/b.mkv"}}
	w.radarr.movies = append(w.radarr.movies, arr.Movie{ID: 2, Title: "Lost", TmdbID: 2, IsAvailable: true})
	w.run(t)
	if fs := w.open(t, KindSettings); len(fs) != 3 {
		t.Fatalf("%+v", fs)
	}
	for kind, want := range map[string]int{KindHealth: 1, KindNaming: 1, KindMissing: 2} {
		if fs := w.open(t, kind); len(fs) != want {
			t.Fatalf("%s: %+v", kind, fs)
		}
	}
}

// An outage must never read as "all clear".
func TestFailedSourceKeepsFindings(t *testing.T) {
	w := newWorld(t)
	w.plex.items["1"][0].Title = "Film.2000.1080p.BluRay.x265-RARBG"
	w.radarr.rename = []arr.Rename{{ExistingPath: "/m/a.mkv", NewPath: "/m/b.mkv"}}
	w.radarr.state.health = []arr.Health{{Source: "IndexerCheck", Type: "error", Message: "no indexers"}}
	w.run(t)
	w.plex.fail = errors.New("plex down")
	w.radarr.fail = errors.New("radarr down")
	w.run(t)
	for _, kind := range []string{KindReleaseTitle, KindNaming, KindHealth} {
		if fs := w.open(t, kind); len(fs) != 1 {
			t.Fatalf("%s resolved during an outage: %+v", kind, fs)
		}
	}
	w.radarr.fail, w.plex.fail = nil, nil
	w.radarr.renErr = errors.New("preview failed")
	w.radarr.state.health = nil
	w.run(t)
	if fs := w.open(t, KindNaming); len(fs) != 1 {
		t.Fatalf("a failed check resolved its findings: %+v", fs)
	}
	if fs := w.open(t, KindHealth); len(fs) != 0 {
		t.Fatalf("a real fix should resolve: %+v", fs)
	}
}

func TestSimilarity(t *testing.T) {
	if s := similarity("The Matrix", "matrix"); s != 1 {
		t.Fatal(s)
	}
	if s := similarity("Andrew Hamilton: The Story", "Hamilton"); s >= minTitleMatch {
		t.Fatal(s)
	}
	if s := similarity("", "x"); s != 0 {
		t.Fatal(s)
	}
}

func TestTVStrayFiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	show := filepath.Join(w.root, "Show", "Season 1")
	if err := os.MkdirAll(show, 0o750); err != nil {
		t.Fatal(err)
	}
	tracked, stray := filepath.Join(show, "S01E01.mkv"), filepath.Join(show, "old.mkv")
	for _, f := range []string{tracked, stray, filepath.Join(show, "S01E01.srt")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.sonarr.series = []arr.Series{{ID: 3, Title: "Show", TvdbID: 9, Path: filepath.Dir(show)}}
	w.sonarr.series[0].Statistics.EpisodeFileCount = 1
	w.sonarr.files = []arr.EpisodeFile{{Path: tracked}}
	w.plex.items["1"] = w.plex.items["1"][:1]
	w.run(t)
	if fs := w.open(t, KindStrayFile); len(fs) != 0 {
		t.Fatalf("looked inside an unmanaged path: %+v", fs)
	}
	if err := w.d.DB.SetManagedPaths(ctx, []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if fs := w.open(t, KindStrayFile); len(fs) != 1 || fs[0].Subject != stray {
		t.Fatalf("%+v", fs)
	}
	w.sonarr.filesErr = errors.New("sonarr hiccup")
	w.run(t)
	if fs := w.open(t, KindStrayFile); len(fs) != 1 {
		t.Fatalf("a failed read resolved the finding: %+v", fs)
	}
}

func (w *world) wrongMatch(t *testing.T) storage.Finding {
	t.Helper()
	film := filepath.Join(w.root, "Film (2000)", "Film.mkv")
	w.plex.items["1"] = []plex.Item{item(t, `{"ratingKey":"10","title":"Other","year":2000,"guid":"plex://movie/2","Guid":[{"id":"tmdb://555"}],"Media":[{"Part":[{"file":"`+film+`"}]}]}`)}
	w.run(t)
	fs := w.open(t, KindWrongMatch)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0]
}

func (w *world) fix(t *testing.T, f storage.Finding, choice string) storage.FixJob {
	t.Helper()
	ctx := context.Background()
	if ok, err := w.d.DB.QueueFix(ctx, f.ID, choice, "admin"); err != nil || !ok {
		t.Fatalf("queue: %v %v", ok, err)
	}
	if n := w.d.RunFixes(ctx); n != 1 {
		t.Fatalf("ran %d", n)
	}
	js, err := w.d.DB.RecentFixes(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return js[0]
}

func TestPreviewChangesNothing(t *testing.T) {
	w := newWorld(t)
	f := w.wrongMatch(t)
	w.plex.matches = []plex.Match{{GUID: "plex://movie/9", Name: "Film", Year: 2000}, {GUID: "plex://movie/2", Name: "Other", Year: 2000}}
	p, err := w.d.Preview(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanFix || len(p.Choices) != 1 || p.Choices[0].Value != "plex://movie/9|Film" {
		t.Fatalf("what it's matched to now isn't a choice: %+v", p)
	}
	if len(w.plex.calls) != 0 {
		t.Fatalf("a preview changed something: %v", w.plex.calls)
	}
}

func TestPreviewOfAnAlreadyRightMatchOffersNothing(t *testing.T) {
	w := newWorld(t)
	f := w.wrongMatch(t)
	w.plex.matches = []plex.Match{{GUID: "plex://movie/2", Name: "Other", Year: 2000}}
	p, err := w.d.Preview(context.Background(), f)
	if err != nil || p.CanFix {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestFixIsVerified(t *testing.T) {
	w := newWorld(t)
	f := w.wrongMatch(t)
	w.plex.takes = true
	if j := w.fix(t, f, "plex://movie/9|Film"); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if got := strings.Join(w.plex.calls, "|"); got != "match 10 plex://movie/9 Film" {
		t.Fatal(got)
	}
	es, _ := w.d.DB.AuditLog(context.Background(), "doctor fix", 10, 0)
	if len(es) != 1 || es[0].Who != "admin" {
		t.Fatalf("%+v", es)
	}
}

func TestFixThatDoesntTakeFails(t *testing.T) {
	w := newWorld(t)
	f := w.wrongMatch(t)
	if j := w.fix(t, f, "plex://movie/9|Film"); j.State != "failed" || !strings.Contains(j.Detail, "still isn't") {
		t.Fatalf("%+v", j)
	}
}

func TestFixRefusesABadChoice(t *testing.T) {
	w := newWorld(t)
	f := w.wrongMatch(t)
	if j := w.fix(t, f, "not-a-guid"); j.State != "failed" || len(w.plex.calls) != 0 {
		t.Fatalf("%+v %v", j, w.plex.calls)
	}
}

func TestFixSkipsItemBeingDeleted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.wrongMatch(t)
	if err := w.d.DB.ReplaceLibrary(ctx, []storage.Section{{Key: "1", Title: "Movies", Type: "movie"}}, []storage.Media{{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "Film"}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := w.d.DB.ScheduleDeletion(ctx, "10", 1, true, storage.KindManual); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if j := w.fix(t, f, "plex://movie/9|Film"); j.State != "skipped" || len(w.plex.calls) != 0 {
		t.Fatalf("%+v %v", j, w.plex.calls)
	}
}

func TestFixSkipsDismissedFinding(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.wrongMatch(t)
	if _, err := w.d.DB.SetFindingState(ctx, f.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if j := w.fix(t, f, "plex://movie/9|Film"); j.State != "skipped" {
		t.Fatalf("%+v", j)
	}
}

func TestPreferLocalFix(t *testing.T) {
	w := newWorld(t)
	w.plex.local = true
	w.run(t)
	var f storage.Finding
	for _, s := range w.open(t, KindSettings) {
		if Fixable(s.Kind, s.Subject) {
			f = s
		}
	}
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if got := strings.Join(w.plex.calls, "|"); got != "prefs 1 false|refresh 1" {
		t.Fatal(got)
	}
}

func TestPreferLocalFixStopsIfSettingStays(t *testing.T) {
	w := newWorld(t)
	w.plex.local, w.plex.stuckLocal = true, true
	w.run(t)
	fs := w.open(t, KindSettings)
	if j := w.fix(t, fs[0], ""); j.State != "failed" || strings.Contains(strings.Join(w.plex.calls, "|"), "refresh") {
		t.Fatalf("%+v %v", j, w.plex.calls)
	}
}

func TestInterruptedFixDoesntResume(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.wrongMatch(t)
	if _, err := w.d.DB.QueueFix(ctx, f.ID, "x", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := w.d.DB.ClaimFix(ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	if err := w.d.DB.FailInterruptedFixes(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.d.RunFixes(ctx); n != 0 {
		t.Fatal(n)
	}
}

// Plex can have the right match and show the wrong name. The fix is a refresh:
// a list of other films would suggest the match was wrong and hide that it isn't.
func TestRightMatchWrongTitleOffersRefresh(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.plex.items["1"][0].Title = "Completely Else"
	w.plex.matches = []plex.Match{{GUID: "plex://movie/1", Name: "Film", Year: 2000}, {GUID: "plex://movie/8", Name: "Unrelated", Year: 2000}}
	w.run(t)
	fs := w.open(t, KindQuestionable)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	p, err := w.d.Preview(ctx, fs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanFix || len(p.Choices) != 1 || p.Choices[0].Value != refreshChoice || !strings.Contains(p.Lines[0], "right match") {
		t.Fatalf("%+v", p)
	}
	if j := w.fix(t, fs[0], refreshChoice); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if got := strings.Join(w.plex.calls, "|"); got != "refresh item 10" {
		t.Fatal(got)
	}
}

func TestReleaseTitleFixRefreshesTheItem(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.plex.items["1"][0].Title = "Film.2000.1080p.BluRay.x265-RARBG"
	w.run(t)
	fs := w.open(t, KindReleaseTitle)
	if len(fs) != 1 || !Fixable(fs[0].Kind, fs[0].Subject) || NeedsChoice(fs[0].Kind) {
		t.Fatalf("%+v", fs)
	}
	p, err := w.d.Preview(ctx, fs[0])
	if err != nil || !p.CanFix || len(p.Choices) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if j := w.fix(t, fs[0], ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if got := strings.Join(w.plex.calls, "|"); got != "refresh item 10" {
		t.Fatal(got)
	}
}

func TestRenameFix(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.radarr.rename = []arr.Rename{{MovieID: 1, MovieFileID: 22, ExistingPath: "Old.mkv", NewPath: "New.mkv"}}
	w.run(t)
	fs := w.open(t, KindNaming)
	if len(fs) != 1 || fs[0].Subject != "radarr:22" {
		t.Fatalf("%+v", fs)
	}
	p, err := w.d.Preview(ctx, fs[0])
	if err != nil || !p.CanFix || !strings.Contains(p.Lines[0], "Old.mkv to New.mkv") {
		t.Fatalf("%+v %v", p, err)
	}
	if len(w.radarr.commands) != 0 {
		t.Fatal("a preview ran a command")
	}
	if j := w.fix(t, fs[0], ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	c := w.radarr.commands[0]
	if c["name"] != "RenameFiles" || c["movieId"] != int64(1) || fmt.Sprint(c["files"]) != "[22]" {
		t.Fatalf("%+v", c)
	}
}

func TestRenameFixReportsAFailedCommand(t *testing.T) {
	w := newWorld(t)
	w.sonarr.series = []arr.Series{{ID: 3, Title: "Show", TvdbID: 9, Path: w.root}}
	w.radarr.rename = []arr.Rename{{SeriesID: 3, EpisodeFileID: 44, ExistingPath: "a.mkv", NewPath: "b.mkv"}}
	w.run(t)
	fs := w.open(t, KindNaming)
	if len(fs) != 1 || fs[0].Subject != "sonarr:44" {
		t.Fatalf("%+v", fs)
	}
	w.sonarr.status = "failed"
	if j := w.fix(t, fs[0], ""); j.State != "failed" || !strings.Contains(j.Detail, "boom") {
		t.Fatalf("%+v", j)
	}
}

func TestFixSkipsArrItemBeingDeleted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.radarr.rename = []arr.Rename{{MovieID: 1, MovieFileID: 22, ExistingPath: "Old.mkv", NewPath: "New.mkv"}}
	w.run(t)
	if err := w.d.DB.ReplaceLibrary(ctx, []storage.Section{{Key: "1", Title: "Movies", Type: "movie"}}, []storage.Media{{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "Film", ArrID: sql.NullInt64{Int64: 1, Valid: true}}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := w.d.DB.ScheduleDeletion(ctx, "10", 1, true, storage.KindManual); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if j := w.fix(t, w.open(t, KindNaming)[0], ""); j.State != "skipped" || len(w.radarr.commands) != 0 {
		t.Fatalf("%+v", j)
	}
}

func (w *world) untrackedFolder(t *testing.T, withVideo bool) storage.Finding {
	t.Helper()
	dir := filepath.Join(w.root, "Lost (2001)")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	name := ".plexmatch"
	if withVideo {
		name = "Lost.mkv"
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.radarr.state.roots[0].UnmappedFolders = []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{{Name: "Lost (2001)", Path: dir}}
	w.run(t)
	fs := w.open(t, KindUntracked)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0]
}

func TestAddUntrackedFolderUnmonitoredWithNoSearch(t *testing.T) {
	w := newWorld(t)
	f := w.untrackedFolder(t, true)
	p, err := w.d.Preview(context.Background(), f)
	if err != nil || !p.CanFix || len(p.Choices) != 1 || p.Choices[0].Value != "7" {
		t.Fatalf("%+v %v", p, err)
	}
	w.radarr.gotFile = true
	if j := w.fix(t, f, "7"); j.State != "done" || !strings.Contains(j.Detail, "with its file") {
		t.Fatalf("%+v", j)
	}
	m := w.radarr.added
	if m["monitored"] != false || m["path"] != filepath.Join(w.root, "Lost (2001)") || m["rootFolderPath"] != w.root || m["qualityProfileId"] != int64(4) {
		t.Fatalf("%+v", m)
	}
	if m["addOptions"].(map[string]any)["searchForMovie"] != false {
		t.Fatal("a search would download something")
	}
	if _, ok := m["id"]; ok {
		t.Fatal("kept the lookup's ID")
	}
}

func TestFolderWithNoVideoCantBeAdded(t *testing.T) {
	w := newWorld(t)
	f := w.untrackedFolder(t, false)
	p, err := w.d.Preview(context.Background(), f)
	if err != nil || p.CanFix || len(p.Choices) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestAddRefusesNoChoice(t *testing.T) {
	w := newWorld(t)
	f := w.untrackedFolder(t, true)
	if j := w.fix(t, f, "nope"); j.State != "failed" || w.radarr.added != nil {
		t.Fatalf("%+v", j)
	}
}

func TestManualImport(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.radarr.movies[0].HasFile, w.radarr.movies[0].MovieFile = false, nil
	w.run(t)
	fs := w.open(t, KindNotAccepted)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	w.radarr.files = []arr.ImportFile{{Path: "/m/Film.mkv", RelativePath: "Film.mkv", FolderName: "Film"}}
	p, err := w.d.Preview(ctx, fs[0])
	if err != nil || !p.CanFix || len(p.Choices) == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	w.radarr.gotFile = true
	if j := w.fix(t, fs[0], "WEBDL-1080p"); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if c := w.radarr.commands[0]; c["name"] != "ManualImport" {
		t.Fatalf("%+v", c)
	}
}

func TestManualImportThatDoesntTakeFails(t *testing.T) {
	w := newWorld(t)
	w.radarr.movies[0].HasFile, w.radarr.movies[0].MovieFile = false, nil
	w.run(t)
	w.radarr.files = []arr.ImportFile{{Path: "/m/Film.mkv"}}
	if j := w.fix(t, w.open(t, KindNotAccepted)[0], "WEBDL-1080p"); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
}

func TestEmptyFoldersFixKeepsOtherSettings(t *testing.T) {
	w := newWorld(t)
	w.radarr.state.settings = arr.MediaManagement{CreateEmptyMovieFolders: true, RecycleBin: "/bin"}
	w.run(t)
	fs := w.open(t, KindSettings)
	if len(fs) != 1 || !Fixable(fs[0].Kind, fs[0].Subject) {
		t.Fatalf("%+v", fs)
	}
	if j := w.fix(t, fs[0], ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if s := w.radarr.savedSettings; s["keepMe"] != "yes" || s["deleteEmptyFolders"] != true || s["createEmptyMovieFolders"] != false {
		t.Fatalf("%+v", s)
	}
}

func TestEmptyFoldersFixNoticesRadarrIgnoringIt(t *testing.T) {
	w := newWorld(t)
	w.radarr.state.settings = arr.MediaManagement{CreateEmptyMovieFolders: true, RecycleBin: "/bin"}
	w.radarr.ignoreSettings = true
	w.run(t)
	if j := w.fix(t, w.open(t, KindSettings)[0], ""); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
}

func TestRecycleBinIsReportOnly(t *testing.T) {
	if Fixable(KindSettings, "radarr:recycle") {
		t.Fatal("a recycle bin needs a path someone chooses")
	}
}

func TestQualityChoicesFollowPlex(t *testing.T) {
	for res, want := range map[string]string{"1080": "WEBDL-1080p", "720": "WEBDL-720p", "4k": "WEBDL-2160p", "2160": "WEBDL-2160p", "sd": "DVD", "576": "DVD"} {
		cs := qualityChoices(res)
		if cs[0].Value != want || !cs[0].Default {
			t.Fatalf("%s: %+v", res, cs[0])
		}
		if len(cs) != len(importQualities) {
			t.Fatalf("%s: lost choices: %d", res, len(cs))
		}
		for _, c := range cs[1:] {
			if c.Default {
				t.Fatalf("%s: two defaults", res)
			}
		}
	}
	for _, res := range []string{"", "something"} { // nothing measured: nothing is presumed
		for _, c := range qualityChoices(res) {
			if c.Default {
				t.Fatalf("%q: %+v", res, c)
			}
		}
	}
}

func TestManualImportUsesPlexResolution(t *testing.T) {
	w := newWorld(t)
	film := filepath.Join(w.root, "Film (2000)", "Film.mkv")
	w.plex.items["1"] = []plex.Item{item(t, `{"ratingKey":"10","title":"Film","year":2000,"guid":"plex://movie/1","Guid":[{"id":"tmdb://100"}],"Media":[{"videoResolution":"720","Part":[{"file":"`+film+`"}]}]}`)}
	w.radarr.movies[0].HasFile, w.radarr.movies[0].MovieFile = false, nil
	w.radarr.files = []arr.ImportFile{{Path: film, RelativePath: "Film.mkv"}}
	w.run(t)
	p, err := w.d.Preview(context.Background(), w.open(t, KindNotAccepted)[0])
	if err != nil || !p.HasDefault || p.Choices[0].Value != "WEBDL-720p" {
		t.Fatalf("%+v %v", p, err)
	}
}

func (w *world) staleRequest(t *testing.T) storage.Finding {
	t.Helper()
	if err := w.d.DB.ReplaceRequests(context.Background(), []storage.Request{{SeerrRequestID: 1, SeerrMediaID: 7, SeerrUserID: 1, MediaType: "movie", TmdbID: 42, Status: 5}}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	fs := w.open(t, KindStaleRequest)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0]
}

func TestStaleRequestFix(t *testing.T) {
	w := newWorld(t)
	f := w.staleRequest(t)
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if fmt.Sprint(w.seerr.deleted) != "[7]" {
		t.Fatal(w.seerr.deleted)
	}
	w.run(t)
	if fs := w.open(t, KindStaleRequest); len(fs) != 0 {
		t.Fatalf("should clear without waiting for a sync: %+v", fs)
	}
}

func TestStaleRequestAlreadyGoneIsFine(t *testing.T) {
	w := newWorld(t)
	f := w.staleRequest(t)
	w.seerr.err = &httpjson.StatusError{Code: 404}
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
}

func TestStaleRequestErrorIsReported(t *testing.T) {
	w := newWorld(t)
	f := w.staleRequest(t)
	w.seerr.err = errors.New("seerr down")
	if j := w.fix(t, f, ""); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
}

// emptyFolder sets up an untracked folder under a managed path.
func (w *world) emptyFolder(t *testing.T, files ...string) (storage.Finding, string) {
	t.Helper()
	dir := filepath.Join(w.root, "Left (2001)")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.radarr.state.roots[0].UnmappedFolders = []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{{Name: "Left (2001)", Path: dir}}
	w.run(t)
	if err := w.d.DB.SetManagedPaths(context.Background(), []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	fs := w.open(t, KindEmptyFolder)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0], dir
}

func TestEmptyFolderFix(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyFolder(t)
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("still there", err)
	}
}

func TestPlexmatchOnlyFolderCountsAsEmpty(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyFolder(t, leftoverFile)
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("still there", err)
	}
}

func TestEmptyFolderFixLeavesAFolderThatFilledUp(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyFolder(t)
	if err := os.WriteFile(filepath.Join(dir, "new.mkv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if j := w.fix(t, f, ""); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.mkv")); err != nil {
		t.Fatal("deleted a new file", err)
	}
}

func TestEmptyFolderFixLeavesAFolderThatGotTracked(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyFolder(t)
	w.radarr.state.roots[0].UnmappedFolders = nil
	if j := w.fix(t, f, ""); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("removed a tracked folder", err)
	}
}

func TestDiskFixRefusesUnmanagedPath(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f, dir := w.emptyFolder(t)
	if err := w.d.DB.SetManagedPaths(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if j := w.fix(t, f, ""); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("touched an unmanaged path", err)
	}
}

func TestRootRefusesToEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.mkv")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Dir(outside), link); err != nil {
		t.Fatal(err)
	}
	if err := removeUnder(root, root, filepath.Join(link, "keep.mkv")); err == nil {
		t.Fatal("followed a link out of the root")
	}
	if err := removeUnder(root, root, filepath.Join(root, "..", filepath.Base(filepath.Dir(outside)), "keep.mkv")); err == nil {
		t.Fatal("walked out of the root")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveUnderWontTakeAFullFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "full")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.mkv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeUnder(root, root, dir); err == nil {
		t.Fatal("removed a folder with a file in it")
	}
}

func (w *world) strayFile(t *testing.T) (storage.Finding, string) {
	t.Helper()
	stray := filepath.Join(w.root, "Film (2000)", "extra.m2ts")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if err := w.d.DB.SetManagedPaths(context.Background(), []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	fs := w.open(t, KindStrayFile)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0], stray
}

func TestStrayFileFix(t *testing.T) {
	w := newWorld(t)
	f, stray := w.strayFile(t)
	if j := w.fix(t, f, ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("still there", err)
	}
	if _, err := os.Stat(filepath.Join(w.root, "Film (2000)", "Film.mkv")); err != nil {
		t.Fatal("deleted the tracked file", err)
	}
}

func TestStrayFileDryRunDeletesNothing(t *testing.T) {
	w := newWorld(t)
	w.d.DryRun = true
	f, stray := w.strayFile(t)
	if j := w.fix(t, f, ""); j.State != "done" || !strings.Contains(j.Detail, "dry run") {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("a dry run deleted a file", err)
	}
}

func TestStaleRequestSaysWhoAskedWhenTheNameIsGone(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.d.DB.UpsertUser(ctx, storage.User{SeerrID: 1, Name: "Ann"}); err != nil {
		t.Fatal(err)
	}
	w.seerr.noTitle = true
	if err := w.d.DB.ReplaceRequests(ctx, []storage.Request{{SeerrRequestID: 1, SeerrMediaID: 7, SeerrUserID: 1, MediaType: "movie", TmdbID: 42, Status: 2, CreatedAt: 1_700_000_000}}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	fs := w.open(t, KindStaleRequest)
	if len(fs) != 1 || fs[0].Title != "unknown movie (TMDB 42)" || !strings.Contains(fs[0].Detail, "by Ann on 2023-11-14") || !strings.Contains(fs[0].Detail, "no longer has") {
		t.Fatalf("%+v", fs)
	}
}

func TestMissingEpisodesNameTheirSeries(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sonarr.series = []arr.Series{{ID: 3, Title: "Miraculous", TvdbID: 9, Path: w.root}, {ID: 4, Title: "Empty", TvdbID: 10, Path: w.root}}
	w.sonarr.series[0].Statistics.EpisodeFileCount = 5
	a := arr.MissingEpisode{ID: 71, SeriesID: 3, SeasonNumber: 7, EpisodeNumber: 2, Title: "Chrysalis"}
	a.Series.Title = "Miraculous"
	b := arr.MissingEpisode{ID: 72, SeriesID: 4, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}
	b.Series.Title = "Empty"
	w.sonarr.episodes = []arr.MissingEpisode{a, b}
	w.run(t)
	var eps storage.Finding
	for _, f := range w.open(t, KindMissing) {
		if strings.HasPrefix(f.Subject, "episodes:") {
			if eps.ID != 0 {
				t.Fatal("a series with no files was listed twice")
			}
			eps = f
		}
	}
	if eps.Title != "Miraculous: 1 missing" || !strings.Contains(eps.Detail, "S07E02 Chrysalis") {
		t.Fatalf("%+v", eps)
	}
	p, err := w.d.Preview(ctx, eps)
	if err != nil || !p.CanFix || len(p.Choices) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if j := w.fix(t, eps, doSearch); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if c := w.sonarr.commands[0]; c["name"] != "EpisodeSearch" || fmt.Sprint(c["episodeIds"]) != "[71]" {
		t.Fatalf("%+v", c)
	}
}

func (w *world) missingMovie(t *testing.T, monitored bool) storage.Finding {
	t.Helper()
	w.radarr.movies = append(w.radarr.movies, arr.Movie{ID: 2, Title: "Lost", Year: 2008, TmdbID: 2, IsAvailable: true, Monitored: monitored})
	w.run(t)
	fs := w.open(t, KindMissing)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0]
}

func TestMissingMovieChoicesFollowMonitoring(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	p, err := w.d.Preview(ctx, w.missingMovie(t, true))
	if err != nil || p.Choices[0].Value != doSearch {
		t.Fatalf("%+v %v", p, err)
	}
	w = newWorld(t)
	p, err = w.d.Preview(ctx, w.missingMovie(t, false))
	if err != nil || p.Choices[0].Value != doMonitorSearch {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestMonitorAndSearchDoesBoth(t *testing.T) {
	w := newWorld(t)
	f := w.missingMovie(t, false)
	if j := w.fix(t, f, doMonitorSearch); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if fmt.Sprint(w.radarr.calls) != "[monitor 2]" || w.radarr.commands[0]["name"] != "MoviesSearch" {
		t.Fatalf("%v %+v", w.radarr.calls, w.radarr.commands)
	}
}

func TestRemoveMissingMovieLeavesOneThatGotAFile(t *testing.T) {
	w := newWorld(t)
	f := w.missingMovie(t, true)
	w.radarr.gotFile = true
	if j := w.fix(t, f, doRemove); j.State != "failed" || len(w.radarr.calls) != 0 {
		t.Fatalf("%+v %v", j, w.radarr.calls)
	}
}

func TestRemoveSeriesLeavesOneThatGotFiles(t *testing.T) {
	w := newWorld(t)
	w.sonarr.series = []arr.Series{{ID: 4, Title: "Empty", TvdbID: 10, Path: w.root}}
	w.run(t)
	f := w.open(t, KindMissing)[0]
	w.sonarr.fileCount = 2
	if j := w.fix(t, f, doRemove); j.State != "failed" || len(w.sonarr.calls) != 0 {
		t.Fatalf("%+v %v", j, w.sonarr.calls)
	}
}

func TestMissingNeedsAChoice(t *testing.T) {
	w := newWorld(t)
	f := w.missingMovie(t, true)
	if j := w.fix(t, f, "whatever"); j.State != "failed" || len(w.radarr.commands) != 0 {
		t.Fatalf("%+v", j)
	}
}

func TestRemovedMovieGetsItsOwnFinding(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.radarr.state.health = []arr.Health{{Source: "RemovedMovieCheck", Type: "error", Message: "Movie Film (tmdbid 100) was removed from TMDb"}}
	w.run(t)
	fs := w.open(t, KindHealth)
	if len(fs) != 1 || !Fixable(fs[0].Kind, fs[0].Subject) || fs[0].Title != "Film (2000)" {
		t.Fatalf("%+v", fs)
	}
	p, err := w.d.Preview(ctx, fs[0])
	if err != nil || !p.CanFix || !strings.Contains(strings.Join(p.Lines, " "), "untracked") {
		t.Fatalf("%+v %v", p, err)
	}
	if j := w.fix(t, fs[0], doRemove); j.State != "done" || fmt.Sprint(w.radarr.calls) != "[remove 1]" {
		t.Fatalf("%+v %v", j, w.radarr.calls)
	}
}

func TestOtherHealthIsReportedWithItsLink(t *testing.T) {
	w := newWorld(t)
	w.radarr.state.health = []arr.Health{{Source: "IndexerCheck", Type: "warning", Message: "no indexers", WikiURL: "https://wiki/x"}}
	w.run(t)
	fs := w.open(t, KindHealth)
	if len(fs) != 1 || Fixable(fs[0].Kind, fs[0].Subject) || !strings.Contains(fs[0].Detail, "https://wiki/x") {
		t.Fatalf("%+v", fs)
	}
}

type fakeAlerts struct{ posted []string }

func (f *fakeAlerts) DownloadAlert(_ context.Context, text string, id int64) error {
	f.posted = append(f.posted, fmt.Sprintf("%d %s", id, text))
	return nil
}

// stuckPack is a season pack whose import is blocked, as two queue records.
func stuckPack() []arr.QueueRecord {
	mk := func(id int64, ep int) arr.QueueRecord {
		q := arr.QueueRecord{ID: id, DownloadID: "SAB1", Title: "Show.S01.1080p", TrackedDownloadStatus: "warning", TrackedDownloadState: "importBlocked"}
		q.StatusMessages = append(q.StatusMessages, struct {
			Title    string   `json:"title"`
			Messages []string `json:"messages"`
		}{Title: "Show.S01.1080p", Messages: []string{"Not an upgrade for existing episode file(s)"}})
		q.Series = &struct {
			Title string `json:"title"`
		}{"Show"}
		q.Episode = &struct {
			SeasonNumber  int `json:"seasonNumber"`
			EpisodeNumber int `json:"episodeNumber"`
		}{1, ep}
		return q
	}
	healthy := arr.QueueRecord{ID: 9, DownloadID: "OK1", Title: "Fine", TrackedDownloadStatus: "ok", TrackedDownloadState: "downloading", Size: 100, Sizeleft: 25}
	return []arr.QueueRecord{mk(1, 1), mk(2, 2), healthy}
}

func TestStuckDownloadsAreOneFindingPerDownload(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sonarr.queue = stuckPack()
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	fs := w.open(t, KindStuck)
	if len(fs) != 1 || fs[0].Title != "Show (2 episodes)" || !strings.Contains(fs[0].Detail, "Not an upgrade") {
		t.Fatalf("%+v", fs)
	}
	rows, err := w.d.Downloads(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	for _, r := range rows {
		if (r.Release == "Fine") == (r.FindingID != 0) {
			t.Fatalf("only the stuck one is fixable: %+v", r)
		}
		if r.Release == "Fine" && r.Progress != 75 {
			t.Fatalf("%+v", r)
		}
	}
}

// stalledPack is stuckPack still downloading with a warning, which can clear itself.
func stalledPack() []arr.QueueRecord {
	q := stuckPack()
	for i := range q[:2] {
		q[i].TrackedDownloadState = "downloading"
	}
	return q
}

func TestBlockedDownloadAlertsAtOnce(t *testing.T) {
	w := newWorld(t)
	alerts := &fakeAlerts{}
	w.d.Alerts = alerts
	w.sonarr.queue = stuckPack()
	if err := w.d.CheckDownloads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(alerts.posted) != 1 {
		t.Fatalf("a blocked import waited: %q", alerts.posted)
	}
}

func TestStuckDownloadAlertsOnceAfterAWhile(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	alerts := &fakeAlerts{}
	w.d.Alerts = alerts
	now := time.Now()
	w.d.Now = func() time.Time { return now }
	w.sonarr.queue = stalledPack()
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(alerts.posted) != 0 {
		t.Fatal("posted a download that only just stuck")
	}
	now = now.Add(stuckFor + time.Minute)
	for range 2 {
		if err := w.d.CheckDownloads(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(alerts.posted) != 1 || !strings.Contains(alerts.posted[0], "Not an upgrade") || !strings.Contains(alerts.posted[0], "Show.S01.1080p") {
		t.Fatalf("%q", alerts.posted)
	}
	// Unstuck, then stuck again later: that's news again.
	w.sonarr.queue = nil
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	w.sonarr.queue = stalledPack()
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(stuckFor + time.Minute)
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(alerts.posted) != 2 {
		t.Fatalf("%q", alerts.posted)
	}
}

func TestQueueOutageChangesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sonarr.queue = stuckPack()
	if err := w.d.CheckDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	w.sonarr.queueErr = errors.New("sonarr down")
	if err := w.d.CheckDownloads(ctx); err == nil {
		t.Fatal("no error")
	}
	if fs := w.open(t, KindStuck); len(fs) != 1 {
		t.Fatalf("an outage resolved a stuck download: %+v", fs)
	}
}

func (w *world) stuck(t *testing.T) storage.Finding {
	t.Helper()
	w.sonarr.queue = stuckPack()
	if err := w.d.CheckDownloads(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w.open(t, KindStuck)[0]
}

func TestStuckDownloadRemovals(t *testing.T) {
	for choice, want := range map[string]string{
		doRemoveDownload: "[1 2] blocklist=false skip=true",
		doBlocklist:      "[1 2] blocklist=true skip=true",
		doBlocklistAgain: "[1 2] blocklist=true skip=false",
	} {
		w := newWorld(t)
		f := w.stuck(t)
		w.sonarr.clearOnRemove = true
		if j := w.fix(t, f, choice); j.State != "done" {
			t.Fatalf("%s: %+v", choice, j)
		}
		if fmt.Sprint(w.sonarr.removed) != "["+want+"]" {
			t.Fatalf("%s: %v", choice, w.sonarr.removed)
		}
	}
}

func TestStuckDownloadStillQueuedFails(t *testing.T) {
	w := newWorld(t)
	f := w.stuck(t)
	if j := w.fix(t, f, doRemoveDownload); j.State != "failed" || !strings.Contains(j.Detail, "still in the queue") {
		t.Fatalf("%+v", j)
	}
}

func importFile(withEpisodes bool) arr.DownloadFile {
	f := arr.DownloadFile{Path: "/dl/Show.S01E01.mkv", FolderName: "Show.S01", Quality: map[string]any{"quality": map[string]any{"id": 3}}}
	f.Series = &struct {
		ID int64 `json:"id"`
	}{ID: 4}
	if withEpisodes {
		f.Episodes = []struct {
			ID int64 `json:"id"`
		}{{ID: 77}}
	}
	f.Rejections = []struct {
		Reason string `json:"reason"`
	}{{Reason: "Not an upgrade"}}
	return f
}

func TestStuckDownloadImport(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.stuck(t)
	w.sonarr.imports = []arr.DownloadFile{importFile(true), {Path: "/dl/info.nfo"}}
	p, err := w.d.Preview(ctx, f)
	if err != nil || p.Choices[0].Value != doImport || !strings.Contains(strings.Join(p.Lines, " "), "Not an upgrade") {
		t.Fatalf("%+v %v", p, err)
	}
	w.sonarr.queue = nil // the import takes it out of the queue
	if j := w.fix(t, f, doImport); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	c := w.sonarr.commands[0]
	files := c["files"].([]map[string]any)
	if c["name"] != "ManualImport" || len(files) != 1 || files[0]["seriesId"] != int64(4) || fmt.Sprint(files[0]["episodeIds"]) != "[77]" || files[0]["downloadId"] != "SAB1" {
		t.Fatalf("%+v", c)
	}
}

func TestStuckDownloadWithUnknownFilesOffersNoImport(t *testing.T) {
	w := newWorld(t)
	f := w.stuck(t)
	w.sonarr.imports = []arr.DownloadFile{importFile(false)}
	p, err := w.d.Preview(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Choices {
		if c.Value == doImport {
			t.Fatal("offered an import of files nobody identified")
		}
	}
	if len(p.Choices) != 3 {
		t.Fatalf("%+v", p.Choices)
	}
}

func TestNamingGroups(t *testing.T) {
	for want, names := range map[string][2]string{
		"capital letters only":        {"A Bit Of Advice WEBDL-720p.mkv", "A Bit of Advice WEBDL-720p.mkv"},
		"punctuation only":            {"August Rush (2007) Bluray-1080p.mkv", "August Rush - (2007) - Bluray-1080p.mkv"},
		"part number only":            {"S01E05 - Now and Then (2) WEBDL-1080p.mkv", "S01E05 - Now and Then WEBDL-1080p.mkv"},
		"quality label only":          {"S08E08 - Renewal Bluray-1080p.mkv", "S08E08 - Renewal Bluray-1080p Remux.mkv"},
		"episode title changed":       {"S02E16 - The Ascent WEBDL-1080p.mkv", "S02E16 - Ascension WEBDL-1080p.mkv"},
		"movie title or year changed": {"Nausicaa - (1984) - Bluray-1080p.mkv", "Warriors of the Wind - (1984) - Bluray-1080p.mkv"},
	} {
		app := appSonarr
		if want == "movie title or year changed" || want == "punctuation only" {
			app = appRadarr
		}
		f := storage.Finding{Data: encode(fixData{App: app, OldName: names[0], NewName: names[1]})}
		if got := NamingGroup(f); got != want {
			t.Fatalf("%v: got %q, want %q", names, got, want)
		}
	}
}

func TestSpaceCardSkipsTheAppsOwnRoot(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.radarr.state.disks = []arr.DiskSpace{{Path: "/", FreeSpace: 1, TotalSpace: 100}, {Path: w.root, FreeSpace: 70, TotalSpace: 100}}
	w.run(t)
	if err := w.d.DB.SaveLibraryPaths(ctx, []storage.LibraryPath{{Path: w.root, UsedBy: "radarr", Media: true}, {Path: "/srv/elsewhere", UsedBy: "plex", Media: true}}); err != nil {
		t.Fatal(err)
	}
	disks, _, err := w.d.Space(ctx)
	if err != nil || len(disks) != 1 || disks[0].Path != w.root || disks[0].Used != 30 {
		t.Fatalf("%+v %v", disks, err)
	}
}

func TestSpaceCardReportsDownloadsOnTheSameDisk(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	torrents, sab := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(torrents, "a.mkv"), make([]byte, 1234), 0o600); err != nil {
		t.Fatal(err)
	}
	w.radarr.state.disks = []arr.DiskSpace{
		{Path: w.root, FreeSpace: 70, TotalSpace: 100},
		{Path: torrents, FreeSpace: 70, TotalSpace: 100}, // one filesystem, two mounts
		{Path: sab, FreeSpace: 900, TotalSpace: 1000},
	}
	w.run(t)
	if err := w.d.DB.SaveLibraryPaths(ctx, []storage.LibraryPath{
		{Path: w.root, UsedBy: "radarr", Media: true},
		{Path: torrents, UsedBy: "radarr", Visible: true},
		{Path: sab, UsedBy: "radarr", Visible: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sab, "b.mkv"), make([]byte, 99), 0o600); err != nil {
		t.Fatal(err)
	}
	disks, _, err := w.d.Space(ctx)
	if err != nil || len(disks) != 2 || len(disks[0].Downloads) != 1 || disks[0].Downloads[0] != (Usage{torrents, 1234}) || disks[0].DownloadsOnly {
		t.Fatalf("%+v %v", disks, err)
	}
	if d := disks[1]; !d.DownloadsOnly || d.Path != sab || d.Used != 10 || len(d.Downloads) != 1 || d.Downloads[0] != (Usage{sab, 99}) {
		t.Fatalf("a download folder on its own disk: %+v", d)
	}
}

func TestNotManagedOnlyOutsideRootFolders(t *testing.T) {
	w := newWorld(t)
	inRoot := filepath.Join(w.root, "Lost (2001)", "Lost.mkv")
	elsewhere := filepath.Join(t.TempDir(), "Gone (2002)", "Gone.mkv")
	w.plex.items["1"] = append(w.plex.items["1"],
		item(t, `{"ratingKey":"11","title":"Lost","guid":"plex://movie/2","Guid":[{"id":"tmdb://200"}],"Media":[{"Part":[{"file":"`+inRoot+`"}]}]}`),
		item(t, `{"ratingKey":"12","title":"Gone","guid":"plex://movie/3","Guid":[{"id":"tmdb://300"}],"Media":[{"Part":[{"file":"`+elsewhere+`"}]}]}`))
	w.radarr.state.roots[0].UnmappedFolders = []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{{Name: "Lost (2001)", Path: filepath.Dir(inRoot)}}
	w.run(t)
	fs := w.open(t, KindNotManaged)
	if len(fs) != 1 || fs[0].Subject != "12" || !strings.Contains(fs[0].Detail, "Radarr root folder") || Fixable(fs[0].Kind, fs[0].Subject) {
		t.Fatalf("want only the one outside the root, report only: %+v", fs)
	}
}

func TestMappedPathIsVerifiedCheckedAndFixedWhereItsMounted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// The services see w.root; here the files are mounted somewhere else.
	here := filepath.Join(t.TempDir(), "mounted")
	if err := os.Rename(w.root, here); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if ps, _ := w.d.DB.LibraryPaths(ctx); len(ps) == 0 || ps[0].Visible {
		t.Fatalf("unmapped and gone, yet seen: %+v", ps)
	}
	if err := w.d.DB.SetLocalPaths(ctx, map[string]string{w.root: here}); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(here, "Film (2000)", "extra.m2ts")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if err := w.d.DB.SetManagedPaths(ctx, []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if ps, _ := w.d.DB.LibraryPaths(ctx); !ps[0].Verified || ps[0].Local != here {
		t.Fatalf("mapped path not verified: %+v", ps)
	}
	fs := w.open(t, KindStrayFile)
	if len(fs) != 1 || fs[0].Subject != filepath.Join(w.root, "Film (2000)", "extra.m2ts") {
		t.Fatalf("want the stray named as the services see it: %+v", fs)
	}
	if j := w.fix(t, fs[0], ""); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("still there", err)
	}
	if _, err := os.Stat(filepath.Join(here, "Film (2000)", "Film.mkv")); err != nil {
		t.Fatal("deleted the tracked file", err)
	}
}

func TestLocalPathMapping(t *testing.T) {
	ps := []storage.LibraryPath{{Path: "/data/media", Local: "/mnt/media"}, {Path: "/data/media/tv", Local: "/tv"}, {Path: "/data/other"}}
	for in, want := range map[string]string{
		"/data/media/movies/a.mkv": "/mnt/media/movies/a.mkv",
		"/data/media/tv/b":         "/tv/b",
		"/data/media":              "/mnt/media",
		"/data/mediax/c":           "/data/mediax/c",
		"/data/other/d":            "/data/other/d",
	} {
		if got := storage.Local(ps, in); got != want {
			t.Fatalf("%s: got %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"relative", "/a/../b", "/"} {
		if storage.ValidLocal(bad) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// emptyShow sets up an untracked Sonarr folder with no video, under a managed path.
func (w *world) emptyShow(t *testing.T) (storage.Finding, string) {
	t.Helper()
	dir := filepath.Join(w.root, "Gone (2001)")
	if err := os.MkdirAll(filepath.Join(dir, "Season 01"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tvshow.nfo", "Season 01/poster.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.sonarr.series = []arr.Series{{ID: 3, Title: "Show", TvdbID: 9, Path: w.root, QualityProfileID: 2}}
	w.sonarr.state.roots = []arr.RootFolder{{Path: w.root, UnmappedFolders: []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{{Name: "Gone (2001)", Path: dir}}}}
	w.run(t)
	if err := w.d.DB.SetManagedPaths(context.Background(), []string{w.root}); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	fs := w.open(t, KindUntracked)
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	return fs[0], dir
}

func TestShowFolderWithNoVideoCanBeAddedOrRemoved(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyShow(t)
	p, err := w.d.Preview(context.Background(), f)
	if err != nil || !p.CanFix || len(p.Choices) != 2 || p.Choices[0].Value != "8" || p.Choices[1].Value != removeChoice {
		t.Fatalf("%+v %v", p, err)
	}
	if !strings.Contains(strings.Join(p.Lines, " "), "tvshow.nfo") {
		t.Fatalf("doesn't say what goes: %v", p.Lines)
	}
	if j := w.fix(t, f, removeChoice); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("still there", err)
	}
	if w.sonarr.added != nil {
		t.Fatal("added as well")
	}
}

func TestShowFolderWithNoVideoAddsUnmonitored(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyShow(t)
	if j := w.fix(t, f, "8"); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if m := w.sonarr.added; m == nil || m["monitored"] != false || m["path"] != dir {
		t.Fatalf("%+v", m)
	}
}

func TestShowFolderRemovalStopsIfAVideoAppears(t *testing.T) {
	w := newWorld(t)
	f, dir := w.emptyShow(t)
	if err := os.WriteFile(filepath.Join(dir, "Season 01", "Gone.S01E01.mkv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if j := w.fix(t, f, removeChoice); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, "tvshow.nfo")); err != nil {
		t.Fatal("removed something", err)
	}
}
