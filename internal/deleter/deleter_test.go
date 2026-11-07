package deleter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// fake records every call, in order, across all services.
type fake struct {
	calls     []string
	failOn    string
	movies    []arr.Movie
	series    []arr.Series
	queue     []arr.QueueItem
	sessions  []tautulli.Session
	activity  error
	notifyErr error
	seerrErr  error
	disks     []arr.DiskSpace
	episodes  []arr.Episode
	posts     []string
}

func (f *fake) rec(format string, a ...any) error {
	c := fmt.Sprintf(format, a...)
	f.calls = append(f.calls, c)
	if f.failOn == c {
		return errors.New("boom")
	}
	return nil
}

type (
	radarr struct{ *fake }
	sonarr struct{ *fake }
)

func (f sonarr) Episodes(context.Context, int64) ([]arr.Episode, error) { return f.episodes, nil }
func (f sonarr) UnmonitorEpisodes(_ context.Context, ids []int64) error {
	return f.rec("sonarr unmonitor episode %d", ids[0])
}

func (f sonarr) DeleteEpisodeFile(_ context.Context, id int64) error {
	return f.rec("sonarr delete file %d", id)
}

func (f *fake) Announce(_ context.Context, text, key string) error {
	f.posts = append(f.posts, text)
	return f.rec("announce %s", key)
}

func (f *fake) Alert(_ context.Context, text, user string) error {
	f.posts = append(f.posts, text)
	return f.rec("alert %s", user)
}

func (f *fake) DiskSpace(context.Context) ([]arr.DiskSpace, error) { return f.disks, nil }

func (f radarr) Movies(context.Context) ([]arr.Movie, error) { return f.movies, nil }
func (f radarr) UnmonitorMovie(_ context.Context, id int64) error {
	return f.rec("radarr unmonitor %d", id)
}
func (f radarr) MovieQueue(context.Context, int64) ([]arr.QueueItem, error) { return f.queue, nil }
func (f radarr) RemoveFromQueue(_ context.Context, id int64) error {
	return f.rec("radarr dequeue %d", id)
}

func (f radarr) DeleteMovie(_ context.Context, id int64) error { return f.rec("radarr delete %d", id) }

func (f sonarr) Series(context.Context) ([]arr.Series, error) { return f.series, nil }
func (f sonarr) UnmonitorSeries(_ context.Context, id int64) error {
	return f.rec("sonarr unmonitor %d", id)
}

func (f sonarr) SeriesQueue(context.Context, int64, []int64) ([]arr.QueueItem, error) {
	return f.queue, nil
}

func (f sonarr) RemoveFromQueue(_ context.Context, id int64) error {
	return f.rec("sonarr dequeue %d", id)
}

func (f sonarr) DeleteSeries(_ context.Context, id int64) error { return f.rec("sonarr delete %d", id) }

func (f *fake) RefreshSection(_ context.Context, s string) error { return f.rec("plex refresh %s", s) }

func (f *fake) EmptyTrash(_ context.Context, s string) error { return f.rec("plex trash %s", s) }

func (f *fake) DeleteItem(_ context.Context, k string) error { return f.rec("plex delete %s", k) }

func (f *fake) DeleteMedia(_ context.Context, id int64) error {
	if err := f.rec("seerr delete %d", id); err != nil {
		return err
	}
	return f.seerrErr
}

func (f *fake) Activity(context.Context) ([]tautulli.Session, error) { return f.sessions, f.activity }

func (f *fake) Notify(_ context.Context, text string, _ int64) error {
	f.calls = append(f.calls, "notify")
	f.posts = append(f.posts, text)
	return f.notifyErr
}

func nullInt(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

func setup(t *testing.T) (*Worker, *fake, *time.Time) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secs := []storage.Section{{Key: "1", Title: "Movies", Type: "movie"}, {Key: "2", Title: "TV", Type: "show"}}
	ms := []storage.Media{
		{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "Managed", TmdbID: nullInt(100), ArrID: nullInt(7)},
		{RatingKey: "11", SectionKey: "1", Type: "movie", Title: "Plex Only"},
		{RatingKey: "12", SectionKey: "1", Type: "movie", Title: "Added Since Sync", TmdbID: nullInt(120)},
		{RatingKey: "20", SectionKey: "2", Type: "show", Title: "Show", TvdbID: nullInt(200), ArrID: nullInt(8)},
	}
	if err := db.ReplaceLibrary(ctx, secs, ms); err != nil {
		t.Fatal(err)
	}
	rk := sql.NullString{String: "10", Valid: true}
	if err := db.ReplaceRequests(ctx, []storage.Request{
		{SeerrRequestID: 1, SeerrMediaID: 55, SeerrUserID: 2, RatingKey: rk},
		{SeerrRequestID: 2, SeerrMediaID: 55, SeerrUserID: 3, RatingKey: rk},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeleteWindow(ctx, storage.DeleteWindow{}); err != nil { // tests run at any hour
		t.Fatal(err)
	}
	f := &fake{
		movies: []arr.Movie{{ID: 7, TmdbID: 100}},
		series: []arr.Series{{ID: 8, TvdbID: 200}},
	}
	now := time.Now()
	w := &Worker{
		DB: db, Radarr: radarr{f}, Sonarr: sonarr{f}, Plex: f, Seerr: f, Tautulli: f, Notify: f,
		Now: func() time.Time { return now },
	}
	return w, f, &now
}

func schedule(t *testing.T, w *Worker, key string, dry bool) {
	t.Helper()
	if ok, err := w.DB.ScheduleDeletion(context.Background(), key, 1, dry, storage.KindManual); err != nil || !ok {
		t.Fatalf("schedule %s: %v %v", key, ok, err)
	}
}

// runDue notifies, then jumps past the delay and ticks again.
func runDue(w *Worker, now *time.Time) {
	w.Tick(context.Background())
	*now = now.Add(storage.DeletionDelay + time.Minute)
	w.Tick(context.Background())
}

func recent(t *testing.T, w *Worker) storage.Deletion {
	t.Helper()
	ds, err := w.DB.RecentDeletions(context.Background(), storage.User{IsAdmin: true}, 1)
	if err != nil || len(ds) != 1 {
		t.Fatalf("recent: %v %v", ds, err)
	}
	return ds[0]
}

func active(t *testing.T, w *Worker) []storage.Deletion {
	t.Helper()
	ds, err := w.DB.ActiveDeletions(context.Background(), storage.User{IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func wantCalls(t *testing.T, f *fake, want ...string) {
	t.Helper()
	if got := strings.Join(f.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestMovieOrder(t *testing.T) {
	w, f, now := setup(t)
	f.queue = []arr.QueueItem{{ID: 3}, {ID: 4}}
	schedule(t, w, "10", false)
	runDue(w, now)
	wantCalls(t, f, "notify", "radarr unmonitor 7", "radarr dequeue 3", "radarr dequeue 4", "radarr delete 7",
		"seerr delete 55", "plex refresh 1", "plex trash 1")
	if d := recent(t, w); d.State != storage.DeletionDone || d.DryRun {
		t.Fatalf("%+v", d)
	}
}

func TestShowOrder(t *testing.T) {
	w, f, now := setup(t)
	f.queue = []arr.QueueItem{{ID: 3}}
	schedule(t, w, "20", false)
	runDue(w, now)
	wantCalls(t, f, "notify", "sonarr unmonitor 8", "sonarr dequeue 3", "sonarr delete 8", "plex refresh 2", "plex trash 2")
}

func TestPlexOnlyPath(t *testing.T) {
	w, f, now := setup(t)
	schedule(t, w, "11", false)
	runDue(w, now)
	wantCalls(t, f, "notify", "plex delete 11")
}

// Radarr picked the movie up after the last sync: Plex delete must not run.
func TestLiveStateChoosesArrPath(t *testing.T) {
	w, f, now := setup(t)
	f.movies = append(f.movies, arr.Movie{ID: 9, TmdbID: 120})
	schedule(t, w, "12", false)
	runDue(w, now)
	wantCalls(t, f, "notify", "radarr unmonitor 9", "radarr delete 9", "plex refresh 1", "plex trash 1")
}

func TestArrItemMissingLiveIsLeftAlone(t *testing.T) {
	w, f, now := setup(t)
	f.movies = nil
	schedule(t, w, "10", false)
	runDue(w, now)
	wantCalls(t, f, "notify")
	if d := recent(t, w); d.State != storage.DeletionFailed {
		t.Fatalf("%+v", d)
	}
}

func TestFailureStopsBeforeFiles(t *testing.T) {
	for _, step := range []string{"radarr unmonitor 7", "radarr dequeue 3"} {
		w, f, now := setup(t)
		f.queue = []arr.QueueItem{{ID: 3}}
		f.failOn = step
		schedule(t, w, "10", false)
		runDue(w, now)
		if strings.Contains(strings.Join(f.calls, ","), "radarr delete") {
			t.Fatalf("%s failed but files went: %v", step, f.calls)
		}
		if d := recent(t, w); d.State != storage.DeletionFailed || !strings.Contains(d.Detail, "boom") {
			t.Fatalf("%+v", d)
		}
	}
}

func TestSeerrAlreadyGoneIsFine(t *testing.T) {
	w, f, now := setup(t)
	f.seerrErr = &httpjson.StatusError{Code: http.StatusNotFound}
	schedule(t, w, "10", false)
	runDue(w, now)
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	w, f, now := setup(t)
	w.DryRun = true
	f.queue = []arr.QueueItem{{ID: 3, Title: "grab"}}
	schedule(t, w, "10", true)
	schedule(t, w, "11", true)
	runDue(w, now)
	wantCalls(t, f, "notify", "notify")
	d := recent(t, w)
	if d.State != storage.DeletionDone || !d.DryRun || !strings.Contains(d.Detail, "would ") {
		t.Fatalf("%+v", d)
	}
}

func TestDryRunWorksWithoutNotifier(t *testing.T) {
	w, f, now := setup(t)
	w.DryRun, w.Notify = true, nil
	schedule(t, w, "10", true)
	runDue(w, now)
	wantCalls(t, f)
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

// A notice saying "dry run" must never turn into a real delete after go-live.
func TestDryRunRowStaysDryAfterGoingLive(t *testing.T) {
	w, f, now := setup(t)
	schedule(t, w, "10", true)
	runDue(w, now)
	wantCalls(t, f, "notify")
}

func TestNoClockWithoutNotification(t *testing.T) {
	w, f, now := setup(t)
	f.notifyErr = errors.New("down")
	schedule(t, w, "10", false)
	runDue(w, now)
	if ds := active(t, w); len(ds) != 1 || ds[0].State != storage.DeletionScheduled || ds[0].DeleteAfter != 0 {
		t.Fatalf("%+v", ds)
	}
	wantCalls(t, f, "notify", "notify") // the tick a day later is past the retry wait

	// A failed send isn't repeated every tick.
	w.Tick(context.Background())
	if len(f.calls) != 2 {
		t.Fatalf("retried at once: %v", f.calls)
	}
	f.notifyErr = nil
	*now = now.Add(notifyRetry + time.Minute)
	w.Tick(context.Background())
	if ds := active(t, w); len(ds) != 1 || ds[0].State != storage.DeletionNotified {
		t.Fatalf("%+v", ds)
	}
}

func TestLiveRunWaitsWithoutNotifier(t *testing.T) {
	w, f, now := setup(t)
	w.Notify = nil
	schedule(t, w, "10", false)
	runDue(w, now)
	wantCalls(t, f)
	if ds := active(t, w); len(ds) != 1 || ds[0].State != storage.DeletionScheduled {
		t.Fatalf("%+v", ds)
	}
}

func TestNotDeletedBeforeDelay(t *testing.T) {
	w, f, now := setup(t)
	schedule(t, w, "10", false)
	w.Tick(context.Background())
	*now = now.Add(storage.DeletionDelay - time.Minute)
	w.Tick(context.Background())
	wantCalls(t, f, "notify")
}

func TestBeingWatchedPushesBack(t *testing.T) {
	w, f, now := setup(t)
	f.sessions = []tautulli.Session{{RatingKey: 999, GrandparentRatingKey: 20}} // an episode of the show
	schedule(t, w, "20", false)
	runDue(w, now)
	wantCalls(t, f, "notify")
	ds := active(t, w)
	if len(ds) != 1 || ds[0].State != storage.DeletionNotified || ds[0].DeleteAfter != now.Add(playingWait).Unix() {
		t.Fatalf("%+v", ds)
	}
	f.sessions = nil
	*now = now.Add(playingWait + time.Minute)
	w.Tick(context.Background())
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

func TestActivityDownPushesBack(t *testing.T) {
	w, f, now := setup(t)
	f.activity = errors.New("down")
	schedule(t, w, "10", false)
	runDue(w, now)
	wantCalls(t, f, "notify")
	if ds := active(t, w); len(ds) != 1 || ds[0].State != storage.DeletionNotified {
		t.Fatalf("%+v", ds)
	}
}

func TestChangedDecisionStopsDeletion(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*Worker) error{
		"cancel": func(w *Worker) error {
			ok, err := w.DB.CancelDeletion(ctx, "10")
			if !ok {
				return errors.New("not cancelled")
			}
			return err
		},
		"keep":  func(w *Worker) error { return w.DB.SetItemStatus(ctx, "10", storage.StatusKeep, 1) },
		"space": func(w *Worker) error { return w.DB.SetItemStatus(ctx, "10", storage.StatusSpace, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			w, f, now := setup(t)
			schedule(t, w, "10", false)
			w.Tick(ctx)
			if err := change(w); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(storage.DeletionDelay + time.Minute)
			w.Tick(ctx)
			wantCalls(t, f, "notify")
			if len(active(t, w)) != 0 {
				t.Fatal("still active")
			}
		})
	}
}

func TestPostpone(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	schedule(t, w, "10", false)
	w.Tick(ctx)
	if ok, err := w.DB.PostponeDeletion(ctx, "10"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	*now = now.Add(storage.DeletionDelay + time.Minute)
	w.Tick(ctx)
	wantCalls(t, f, "notify")
	*now = now.Add(storage.DeletionDelay)
	w.Tick(ctx)
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

func TestScheduleRefusals(t *testing.T) {
	w, _, _ := setup(t)
	ctx := context.Background()
	schedule(t, w, "10", false)
	if ok, _ := w.DB.ScheduleDeletion(ctx, "10", 1, false, storage.KindManual); ok {
		t.Fatal("scheduled twice")
	}
	if ok, _ := w.DB.ScheduleDeletion(ctx, "99", 1, false, storage.KindManual); ok {
		t.Fatal("scheduled an unknown item")
	}
	if err := w.DB.SetItemStatus(ctx, "11", storage.StatusKeep, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := w.DB.ScheduleDeletion(ctx, "11", 1, false, storage.KindManual); ok {
		t.Fatal("scheduled a keep item")
	}
}

func TestRestartFailsExecutingRows(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	schedule(t, w, "10", false)
	w.Tick(ctx)
	*now = now.Add(storage.DeletionDelay + time.Minute)
	if _, found, err := w.DB.ClaimDueDeletion(ctx, *now); err != nil || !found {
		t.Fatal(found, err)
	}
	if ok, _ := w.DB.CancelDeletion(ctx, "10"); ok {
		t.Fatal("cancelled while executing")
	}
	if err := w.DB.FailInterruptedDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	wantCalls(t, f, "notify")
	if d := recent(t, w); d.State != storage.DeletionFailed {
		t.Fatalf("%+v", d)
	}
}

func TestKeepFromDiscord(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	schedule(t, w, "10", false)
	w.Tick(ctx)
	id := active(t, w)[0].ID
	title, kept, err := w.DB.KeepFromDiscord(ctx, id, "900", "someone")
	if err != nil || !kept || title != "Managed" {
		t.Fatal(title, kept, err)
	}
	*now = now.Add(storage.DeletionDelay + time.Minute)
	w.Tick(ctx)
	wantCalls(t, f, "notify")
	items, err := w.DB.LibraryItems(ctx, storage.User{IsAdmin: true}, "1", storage.LibraryFilter{Status: storage.StatusKeep})
	if err != nil || len(items) != 1 || items[0].KeptBy != "someone" {
		t.Fatalf("%+v %v", items, err)
	}
	// Pressing again changes nothing, and the name follows the link.
	if _, kept, _ := w.DB.KeepFromDiscord(ctx, id, "901", "other"); kept {
		t.Fatal("kept twice")
	}
	us, err := w.DB.DiscordUsers(ctx)
	if err != nil || len(us) != 1 || us[0].SeerrID != 0 {
		t.Fatalf("%+v %v", us, err)
	}
	if err := w.DB.UpsertUser(ctx, storage.User{SeerrID: 2, Name: "Real Name"}); err != nil {
		t.Fatal(err)
	}
	if found, _ := w.DB.SetDiscordUser(ctx, "900", 99); found {
		t.Fatal("linked to an unknown user")
	}
	if found, err := w.DB.SetDiscordUser(ctx, "900", 2); err != nil || !found {
		t.Fatal(found, err)
	}
	items, _ = w.DB.LibraryItems(ctx, storage.User{IsAdmin: true}, "1", storage.LibraryFilter{Status: storage.StatusKeep})
	if items[0].KeptBy != "Real Name" {
		t.Fatalf("%+v", items[0])
	}
	// An admin's later choice replaces the attribution.
	if err := w.DB.SetItemStatus(ctx, "10", storage.StatusKeep, 1); err != nil {
		t.Fatal(err)
	}
	items, _ = w.DB.LibraryItems(ctx, storage.User{IsAdmin: true}, "1", storage.LibraryFilter{Status: storage.StatusKeep})
	if items[0].KeptBy != "" {
		t.Fatalf("%+v", items[0])
	}
}

// Too late: once executing, a press must not claim to have kept it.
func TestKeepAfterClaimRefused(t *testing.T) {
	w, _, now := setup(t)
	ctx := context.Background()
	schedule(t, w, "10", false)
	w.Tick(ctx)
	*now = now.Add(storage.DeletionDelay + time.Minute)
	d, found, err := w.DB.ClaimDueDeletion(ctx, *now)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, kept, err := w.DB.KeepFromDiscord(ctx, d.ID, "900", "x"); err != nil || kept {
		t.Fatal(kept, err)
	}
	if us, _ := w.DB.DiscordUsers(ctx); len(us) != 0 {
		t.Fatalf("%+v", us)
	}
}

func TestDeleteWindow(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	*now = time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	if err := w.DB.SetDeleteWindow(ctx, storage.DeleteWindow{Start: "03:00", End: "05:00"}); err != nil {
		t.Fatal(err)
	}
	schedule(t, w, "10", false)
	w.Tick(ctx)
	want := time.Date(2026, 1, 7, 3, 0, 0, 0, time.Local) // 24 hours lands at noon, so the next opening after that
	if ds := active(t, w); len(ds) != 1 || ds[0].DeleteAfter != want.Unix() {
		t.Fatalf("%+v, want %d", ds, want.Unix())
	}
	*now = want.Add(-time.Minute)
	w.Tick(ctx)
	wantCalls(t, f, "notify")
	*now = want
	w.Tick(ctx)
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

// A window set after the clock started still holds the deletion back.
func TestWindowChangedAfterNotify(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	*now = time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	schedule(t, w, "10", false)
	w.Tick(ctx)
	if err := w.DB.SetDeleteWindow(ctx, storage.DeleteWindow{Start: "03:00", End: "05:00"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(storage.DeletionDelay + time.Minute)
	w.Tick(ctx)
	wantCalls(t, f, "notify")
	*now = time.Date(2026, 1, 7, 3, 1, 0, 0, time.Local)
	w.Tick(ctx)
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}
