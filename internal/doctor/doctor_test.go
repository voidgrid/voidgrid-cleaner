package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

type fakeArr struct {
	state  arrState
	fail   error
	renErr error
	rename []arr.Rename
}

func (f *fakeArr) RootFolders(context.Context) ([]arr.RootFolder, error) {
	return f.state.roots, f.fail
}

func (f *fakeArr) DiskSpace(context.Context) ([]arr.DiskSpace, error) { return f.state.disks, nil }

func (f *fakeArr) Health(context.Context) ([]arr.Health, error) { return f.state.health, nil }

func (f *fakeArr) BlocklistSize(context.Context) (int, error) { return f.state.block, nil }

func (f *fakeArr) MissingCount(context.Context) (int, error) { return f.state.missing, nil }

func (f *fakeArr) MediaManagement(context.Context) (arr.MediaManagement, error) {
	return f.state.settings, nil
}

type fakeRadarr struct {
	fakeArr
	movies []arr.Movie
}

func (f *fakeRadarr) Movies(context.Context) ([]arr.Movie, error) { return f.movies, nil }
func (f *fakeRadarr) RenamePreview(context.Context, []int64) ([]arr.Rename, error) {
	return f.rename, f.renErr
}

type fakeSonarr struct {
	fakeArr
	series   []arr.Series
	files    []arr.EpisodeFile
	filesErr error
}

func (f *fakeSonarr) Series(context.Context) ([]arr.Series, error) { return f.series, nil }
func (f *fakeSonarr) RenamePreview(context.Context, int64) ([]arr.Rename, error) {
	return nil, nil
}

func (f *fakeSonarr) EpisodeFiles(context.Context, int64) ([]arr.EpisodeFile, error) {
	return f.files, f.filesErr
}

type fakePlex struct {
	secs  []plex.Section
	items map[string][]plex.Item
	local bool
	fail  error
}

func (f *fakePlex) Sections(context.Context) ([]plex.Section, error) { return f.secs, f.fail }
func (f *fakePlex) Items(_ context.Context, k string) ([]plex.Item, error) {
	return f.items[k], nil
}
func (f *fakePlex) ShowPath(context.Context, string) (string, error) { return "", nil }
func (f *fakePlex) PreferLocalMetadata(context.Context, string) (bool, error) {
	return f.local, nil
}

type fakeSeerr struct{}

func (fakeSeerr) Title(context.Context, string, int64) (string, error) { return "Gone Film", nil }

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
	w := &world{root: root}
	w.radarr = &fakeRadarr{movies: []arr.Movie{{ID: 1, Title: "Film", Year: 2000, TmdbID: 100, Path: film, HasFile: true, MovieFile: &struct {
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
	w.d = &Doctor{DB: db, Radarr: w.radarr, Sonarr: w.sonarr, Plex: w.plex, Seerr: fakeSeerr{}}
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
		"same":                   {"Film", 2000, "Film", nil, "", 0},
		"punctuation and the":    {"The Film!", 2000, "Film", nil, "", 0},
		"different title":        {"Completely Else", 2000, "Film Night", nil, "", 1},
		"alternate title counts": {"Fuck Me", 2000, "Baise-moi", []arr.AltTitle{{Title: "Fuck Me"}}, "", 0},
		"original title counts":  {"Fuck Me", 2000, "Baise-moi", nil, "Baise-moi", 0},
		"year far off":           {"Film", 2020, "Film", nil, "", 1},
		"year one off is normal": {"Film", 2001, "Film", nil, "", 0},
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
	w.radarr.state.block = 3
	w.radarr.state.health = []arr.Health{{Source: "RemovedMovieCheck", Type: "error", Message: "removed"}}
	w.sonarr.state.missing = 2
	w.plex.local = true
	w.radarr.rename = []arr.Rename{{ExistingPath: "/m/a.mkv", NewPath: "/m/b.mkv"}}
	w.radarr.movies = append(w.radarr.movies, arr.Movie{ID: 2, Title: "Lost", TmdbID: 2, IsAvailable: true})
	w.run(t)
	if fs := w.open(t, KindSettings); len(fs) != 3 {
		t.Fatalf("%+v", fs)
	}
	for kind, want := range map[string]int{KindBlocklist: 1, KindHealth: 1, KindNaming: 1, KindMissing: 2} {
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
	w.radarr.state.block = 3
	w.run(t)
	w.plex.fail = errors.New("plex down")
	w.radarr.fail = errors.New("radarr down")
	w.run(t)
	for _, kind := range []string{KindReleaseTitle, KindNaming, KindBlocklist} {
		if fs := w.open(t, kind); len(fs) != 1 {
			t.Fatalf("%s resolved during an outage: %+v", kind, fs)
		}
	}
	w.radarr.fail, w.plex.fail = nil, nil
	w.radarr.renErr = errors.New("preview failed")
	w.radarr.state.block = 0
	w.run(t)
	if fs := w.open(t, KindNaming); len(fs) != 1 {
		t.Fatalf("a failed check resolved its findings: %+v", fs)
	}
	if fs := w.open(t, KindBlocklist); len(fs) != 0 {
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
