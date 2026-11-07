package deleter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// spaceSetup has three space-based items of 60 bytes on a 1000-byte pool at
// 95% used, seen by Radarr and Sonarr through separate mounts, so getting to
// 90% takes one item, and a fourth on another disk with plenty free.
func spaceSetup(t *testing.T) (*Worker, *fake, *time.Time) {
	t.Helper()
	w, f, now := setup(t)
	ctx := context.Background()
	movie := func(key, title string, tmdb int64) storage.Media {
		return storage.Media{RatingKey: key, SectionKey: "1", Type: "movie", Title: title, TmdbID: nullInt(tmdb), ArrID: nullInt(tmdb), SizeBytes: 60}
	}
	if err := w.DB.ReplaceLibrary(ctx, []storage.Section{{Key: "1", Title: "Movies", Type: "movie"}, {Key: "2", Title: "TV", Type: "show"}}, []storage.Media{
		movie("1", "Never Watched", 1), movie("2", "Watched Long Ago", 2), movie("3", "Other Disk", 3),
		{RatingKey: "4", SectionKey: "2", Type: "show", Title: "Watched Lately", TvdbID: nullInt(4), ArrID: nullInt(4), SizeBytes: 60},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.DB.ReplacePlays(ctx, []storage.Play{
		{ReferenceID: 1, PlexUserID: 1, RatingKey: "2", ShowRatingKey: "2", Stopped: 100},
		{ReferenceID: 2, PlexUserID: 1, RatingKey: "4", ShowRatingKey: "4", Stopped: 200},
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"1", "2", "3", "4"} {
		if err := w.DB.SetItemStatus(ctx, k, storage.StatusSpace, 1); err != nil {
			t.Fatal(err)
		}
	}
	f.movies = []arr.Movie{{ID: 1, TmdbID: 1, Path: "/pool/movies/a"}, {ID: 2, TmdbID: 2, Path: "/pool/movies/b"}, {ID: 3, TmdbID: 3, Path: "/spare/c"}}
	f.series = []arr.Series{{ID: 4, TvdbID: 4, Path: "/pool/tv/d"}}
	f.disks = []arr.DiskSpace{
		{Path: "/", FreeSpace: 1, TotalSpace: 100},
		{Path: "/pool/movies", FreeSpace: 50, TotalSpace: 1000},
		{Path: "/pool/tv", FreeSpace: 50, TotalSpace: 1000},
		{Path: "/spare", FreeSpace: 5000, TotalSpace: 9000},
	}
	return w, f, now
}

func activeKeys(t *testing.T, w *Worker) string {
	t.Helper()
	ds, err := w.DB.ActiveDeletions(context.Background(), storage.User{IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, d := range ds {
		keys = append(keys, d.RatingKey)
	}
	return strings.Join(keys, " ")
}

func TestSpaceSchedulesOnlyWhatsNeeded(t *testing.T) {
	w, _, now := spaceSetup(t)
	ctx := context.Background()
	w.Tick(ctx)
	if got := activeKeys(t, w); got != "2" {
		t.Fatalf("active %q, want the oldest watched only", got)
	}
	*now = now.Add(2 * spaceCheck)
	w.Tick(ctx)
	if got := activeKeys(t, w); got != "2" {
		t.Fatalf("the pending deletion wasn't counted: %q", got)
	}
}

func TestSpaceUnderLimitDoesNothing(t *testing.T) {
	w, _, _ := spaceSetup(t)
	ctx := context.Background()
	if err := w.DB.SetSpaceLimit(ctx, 96); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	if got := activeKeys(t, w); got != "" {
		t.Fatalf("active %q", got)
	}
}

func TestSpaceCancelIsNotRetried(t *testing.T) {
	w, _, now := spaceSetup(t)
	ctx := context.Background()
	w.Tick(ctx)
	*now = now.Add(2 * spaceCheck)
	time.Sleep(1100 * time.Millisecond) // finished_at must land after set_at, in whole seconds
	if ok, err := w.DB.CancelDeletion(ctx, "2"); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	w.Tick(ctx)
	if got := activeKeys(t, w); got != "4" {
		t.Fatalf("active %q, want the next item, not the cancelled one", got)
	}
}

func TestSpaceDryRunDoesNotRepeat(t *testing.T) {
	w, _, now := spaceSetup(t)
	w.DryRun = true
	ctx := context.Background()
	time.Sleep(1100 * time.Millisecond)
	runDue(w, now) // 2 runs as a dry run and the disk stays full
	if d := recent(t, w); d.RatingKey != "2" || d.State != storage.DeletionDone {
		t.Fatalf("recent: %+v", d)
	}
	*now = now.Add(2 * spaceCheck)
	w.Tick(ctx)
	if got := activeKeys(t, w); strings.Contains(got, "2") {
		t.Fatalf("dry run repeated: %q", got)
	}
}

func TestSpaceSkipsIgnoredLibraries(t *testing.T) {
	w, _, _ := spaceSetup(t)
	ctx := context.Background()
	if _, err := w.DB.SetSectionIgnored(ctx, "1", true); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	if got := activeKeys(t, w); got != "4" {
		t.Fatalf("active %q", got)
	}
}

func TestDiskFor(t *testing.T) {
	disks := []arr.DiskSpace{{Path: "/", TotalSpace: 1}, {Path: "/data", TotalSpace: 1}, {Path: "/data/media", TotalSpace: 1}, {Path: "/data/media2/x", TotalSpace: 1}}
	for path, want := range map[string]string{"/data/media/a": "/data/media", "/data/media2/b": "/data", "/other": "/"} {
		if d, ok := diskFor(disks, path); !ok || d.Path != want {
			t.Fatalf("%s: got %q %v, want %s", path, d.Path, ok, want)
		}
	}
	if _, ok := diskFor(disks, ""); ok {
		t.Fatal("an unknown path matched a disk")
	}
}
