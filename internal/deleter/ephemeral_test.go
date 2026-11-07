package deleter

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// ephemeralSetup makes show 20 ephemeral, requested by X (Plex 1), with Z
// (Plex 2) added. X has watched S01E02; Z hasn't yet.
func ephemeralSetup(t *testing.T) (*Worker, *fake) {
	t.Helper()
	w, f, _ := setup(t)
	ctx := context.Background()
	for _, u := range []storage.User{{SeerrID: 2, Name: "X", PlexID: nullInt(1)}, {SeerrID: 3, Name: "Z", PlexID: nullInt(2)}} {
		if err := w.DB.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.DB.ReplaceRequests(ctx, []storage.Request{{SeerrRequestID: 1, SeerrMediaID: 55, SeerrUserID: 2, RatingKey: sql.NullString{String: "20", Valid: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.DB.SetItemStatus(ctx, "20", storage.StatusEphemeral, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := w.DB.AddWatcher(ctx, "20", 3); err != nil || !ok {
		t.Fatalf("add: %v %v", ok, err)
	}
	played(t, w, play(1, 1))
	f.episodes = []arr.Episode{{ID: 71, SeasonNumber: 1, EpisodeNumber: 1, EpisodeFileID: 81}, {ID: 72, SeasonNumber: 1, EpisodeNumber: 2, EpisodeFileID: 82}}
	f.queue = []arr.QueueItem{{ID: 5, Title: "q"}}
	return w, f
}

func play(ref, plexUser int64) storage.Play {
	return storage.Play{ReferenceID: ref, PlexUserID: plexUser, MediaType: "episode", RatingKey: "202", ShowRatingKey: "20", Season: 1, Episode: 2, Watched: true, Stopped: 100}
}

func played(t *testing.T, w *Worker, ps ...storage.Play) {
	t.Helper()
	if err := w.DB.ReplacePlays(context.Background(), ps); err != nil {
		t.Fatal(err)
	}
}

func TestEpisodeWaitsForEveryWatcher(t *testing.T) {
	w, _ := ephemeralSetup(t)
	ctx := context.Background()
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	if got := activeKeys(t, w); got != "" {
		t.Fatalf("deleted before Z watched: %q", got)
	}
	played(t, w, play(1, 1), play(2, 2))
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	ds, err := w.DB.ActiveDeletions(ctx, storage.User{IsAdmin: true})
	if err != nil || len(ds) != 1 {
		t.Fatalf("%+v %v", ds, err)
	}
	if d := ds[0]; d.RatingKey != "202" || d.ParentKey != "20" || d.Kind != storage.KindEphemeral || d.Title != "Show" {
		t.Fatalf("%+v", d)
	}
	if vis, _ := w.DB.ActiveDeletions(ctx, storage.User{SeerrID: 2}); len(vis) != 1 {
		t.Fatalf("the requester can't see their show's episode: %+v", vis)
	}
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	if ds, _ := w.DB.ActiveDeletions(ctx, storage.User{IsAdmin: true}); len(ds) != 1 {
		t.Fatalf("scheduled twice: %+v", ds)
	}
}

func TestEpisodeDeleteOrder(t *testing.T) {
	w, f, now := setup(t)
	ctx := context.Background()
	f.episodes = []arr.Episode{{ID: 72, SeasonNumber: 1, EpisodeNumber: 2, EpisodeFileID: 82}}
	f.queue = []arr.QueueItem{{ID: 5, Title: "q"}}
	if _, err := w.DB.ScheduleEpisode(ctx, storage.WatchedEpisode{ShowKey: "20", RatingKey: "202", Title: "Show", Season: 1, Episode: 2}, false); err != nil {
		t.Fatal(err)
	}
	runDue(w, now)
	want := "notify|sonarr unmonitor episode 72|sonarr dequeue 5|sonarr delete file 82|plex refresh 2|plex trash 2"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls\n got %s\nwant %s", got, want)
	}
	if d := recent(t, w); d.State != storage.DeletionDone {
		t.Fatalf("%+v", d)
	}
}

func TestEpisodeOfShowNotInSonarrIsLeftAlone(t *testing.T) {
	w, f, now := setup(t)
	f.series = nil
	if _, err := w.DB.ScheduleEpisode(context.Background(), storage.WatchedEpisode{ShowKey: "20", RatingKey: "202", Title: "Show", Season: 1, Episode: 2}, false); err != nil {
		t.Fatal(err)
	}
	runDue(w, now)
	if got := strings.Join(f.calls, "|"); got != "notify" {
		t.Fatalf("calls: %s", got)
	}
	if d := recent(t, w); d.State != storage.DeletionFailed {
		t.Fatalf("%+v", d)
	}
}

func TestKeepOnEpisodeKeepsOnlyIt(t *testing.T) {
	w, _ := ephemeralSetup(t)
	ctx := context.Background()
	played(t, w, play(1, 1), play(2, 2))
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := w.DB.ActiveDeletions(ctx, storage.User{IsAdmin: true})
	if _, kept, err := w.DB.KeepFromDiscord(ctx, ds[0].ID, "900", "Someone"); err != nil || !kept {
		t.Fatalf("keep: %v %v", kept, err)
	}
	if shows, _ := w.DB.EphemeralShows(ctx, storage.User{}); len(shows) != 1 {
		t.Fatalf("the show stopped being ephemeral: %+v", shows)
	}
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	if got := activeKeys(t, w); got != "" {
		t.Fatalf("a kept episode came back: %q", got)
	}
}

func TestLeavingEphemeralCancelsEpisodes(t *testing.T) {
	for _, status := range []string{storage.StatusNormal, storage.StatusKeep} {
		w, _ := ephemeralSetup(t)
		ctx := context.Background()
		played(t, w, play(1, 1), play(2, 2))
		if err := w.checkEphemeral(ctx); err != nil {
			t.Fatal(err)
		}
		if err := w.DB.SetItemStatus(ctx, "20", status, 1); err != nil {
			t.Fatal(err)
		}
		if got := activeKeys(t, w); got != "" {
			t.Fatalf("%s: still scheduled: %q", status, got)
		}
		if err := w.DB.SetItemStatus(ctx, "20", storage.StatusEphemeral, 1); err != nil {
			t.Fatal(err)
		}
		if shows, _ := w.DB.EphemeralShows(ctx, storage.User{}); shows[0].Watchers != "X" {
			t.Fatalf("%s: old watchers stayed: %+v", status, shows)
		}
	}
}

func TestNobodyOnAShowMeansNoDeletion(t *testing.T) {
	w, _ := ephemeralSetup(t)
	ctx := context.Background()
	if err := w.DB.ReplaceRequests(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.DB.SetItemStatus(ctx, "20", storage.StatusNormal, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.DB.SetItemStatus(ctx, "20", storage.StatusEphemeral, 1); err != nil {
		t.Fatal(err)
	}
	played(t, w, play(1, 1), play(2, 2))
	if err := w.checkEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
	if got := activeKeys(t, w); got != "" {
		t.Fatalf("active %q", got)
	}
}

func TestAnnounceOnceAndRetry(t *testing.T) {
	w, f := ephemeralSetup(t)
	ctx := context.Background()
	f.failOn = "announce 20"
	if err := w.announce(ctx); err == nil {
		t.Fatal("no error from a failed post")
	}
	f.failOn, f.calls, f.posts = "", nil, nil
	w.notifyAfter = w.Now()
	for range 2 {
		if err := w.announce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.calls, "|"); got != "announce 20" {
		t.Fatalf("calls: %s", got)
	}
	if !strings.Contains(f.posts[0], "**Show**") || !strings.Contains(f.posts[0], "X, Z") {
		t.Fatalf("post: %s", f.posts[0])
	}
}

func TestUnlinkedAddMeAlertsThenAppliesOnLink(t *testing.T) {
	w, f := ephemeralSetup(t)
	ctx := context.Background()
	if err := w.DB.SetAdminDiscord(ctx, "12345"); err != nil {
		t.Fatal(err)
	}
	if err := w.DB.UpsertUser(ctx, storage.User{SeerrID: 4, Name: "Y"}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a second press doesn't tag the admin again
		if _, result, err := w.DB.JoinFromDiscord(ctx, "20", "900", "@Y*"); err != nil || result != storage.JoinPending {
			t.Fatalf("join: %d %v", result, err)
		}
	}
	f.calls = nil
	for range 2 {
		if err := w.announce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(strings.Join(f.calls, "|"), "alert 12345"); got != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
	last := f.posts[len(f.posts)-1]
	if !strings.HasPrefix(last, "Y pressed Add me on **Show**") {
		t.Fatalf("post: %s", last)
	}
	if ok, err := w.DB.SetDiscordUser(ctx, "900", 4); err != nil || !ok {
		t.Fatalf("link: %v %v", ok, err)
	}
	shows, _ := w.DB.EphemeralShows(ctx, storage.User{SeerrID: 4})
	if len(shows) != 1 || !shows[0].Joined || shows[0].Watchers != "X, Y, Z" {
		t.Fatalf("not added on link: %+v", shows)
	}
	if _, result, err := w.DB.JoinFromDiscord(ctx, "20", "900", "Y"); err != nil || result != storage.JoinAdded {
		t.Fatalf("linked join: %d %v", result, err)
	}
	if _, result, _ := w.DB.JoinFromDiscord(ctx, "10", "900", "Y"); result != storage.JoinGone {
		t.Fatalf("not ephemeral: %d", result)
	}
}

func TestEphemeralNoticeIsOnePerShow(t *testing.T) {
	w, f, _ := setup(t)
	ctx := context.Background()
	for i := range notifyBatch + 3 { // more than one notify batch
		e := storage.WatchedEpisode{ShowKey: "20", RatingKey: strconv.Itoa(300 + i), Title: "Show", Season: 1, Episode: i + 1}
		if ok, err := w.DB.ScheduleEpisode(ctx, e, false); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
	}
	w.Tick(ctx)
	w.notifyAfter = time.Time{}
	w.Tick(ctx)
	if len(f.posts) != 1 || !strings.Contains(f.posts[0], "**Show**: watched episodes (8)") || strings.Contains(f.posts[0], "S01") {
		t.Fatalf("posts: %q", f.posts)
	}
	ds, _ := w.DB.ActiveDeletions(ctx, storage.User{IsAdmin: true})
	if len(ds) != 8 {
		t.Fatalf("%d rows", len(ds))
	}
	for _, d := range ds {
		if d.State != storage.DeletionNotified {
			t.Fatalf("not on the clock: %+v", d)
		}
	}
	title, kept, err := w.DB.KeepFromDiscord(ctx, ds[3].ID, "900", "Someone")
	if err != nil || !kept || title != "Show (these episodes)" {
		t.Fatalf("keep: %q %v %v", title, kept, err)
	}
	if got := activeKeys(t, w); got != "" {
		t.Fatalf("Keep missed some: %q", got)
	}
}
