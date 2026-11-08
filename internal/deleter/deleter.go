// Package deleter runs scheduled deletions: it sends the notification, waits
// out the delay, then removes each item in an order that stops it being
// downloaded again.
package deleter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/doctor"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

const (
	notifyBatch = 5 // messages per tick, inside Discord's rate limit for a channel
	// notifyRetry spaces out failed sends: a send that reached Discord but
	// whose reply we couldn't read must not repeat every tick.
	notifyRetry = time.Hour
	playingWait = time.Hour
)

// The services the worker uses.
type (
	RadarrAPI interface {
		Movies(context.Context) ([]arr.Movie, error)
		UnmonitorMovie(context.Context, int64) error
		MovieQueue(context.Context, int64) ([]arr.QueueItem, error)
		RemoveFromQueue(context.Context, int64) error
		DeleteMovie(context.Context, int64) error
		DiskSpace(context.Context) ([]arr.DiskSpace, error)
	}
	SonarrAPI interface {
		Series(context.Context) ([]arr.Series, error)
		UnmonitorSeries(context.Context, int64) error
		SeriesQueue(context.Context, int64, []int64) ([]arr.QueueItem, error)
		RemoveFromQueue(context.Context, int64) error
		DeleteSeries(context.Context, int64) error
		DiskSpace(context.Context) ([]arr.DiskSpace, error)
		Episodes(context.Context, int64) ([]arr.Episode, error)
		UnmonitorEpisodes(context.Context, []int64) error
		DeleteEpisodeFile(context.Context, int64) error
	}
	PlexAPI interface {
		RefreshSection(context.Context, string) error
		EmptyTrash(context.Context, string) error
		DeleteItem(context.Context, string) error
		Sessions(context.Context) ([]plex.Session, error)
	}
	SeerrAPI interface {
		DeleteMedia(context.Context, int64) error
	}
	TautulliAPI interface {
		Activity(context.Context) ([]tautulli.Session, error)
	}
	Notifier interface {
		Notify(ctx context.Context, text string, deletionID int64) error
		Announce(ctx context.Context, text, ratingKey string) error
		Alert(ctx context.Context, text, userID string) error
		Post(ctx context.Context, admin bool, text string) error
	}
)

// Worker runs deletions one at a time. Notify may be nil: dry-run then only
// logs, and a live run waits, so the clock never starts without a notice.
type Worker struct {
	DB       *storage.DB
	Radarr   RadarrAPI
	Sonarr   SonarrAPI
	Plex     PlexAPI
	Seerr    SeerrAPI
	Tautulli TautulliAPI
	Notify   Notifier
	DryRun   bool
	Now      func() time.Time
	// AdminChannel says a tech alerts channel is set.
	AdminChannel bool
	// Disks reports disk use for the digest; nil leaves it out.
	Disks interface {
		Space(context.Context) ([]doctor.Disk, []storage.SpaceItem, error)
	}

	notifyAfter time.Time
	spaceAfter  time.Time
	auditCalls  bool // the log level, read each tick
}

// recording makes every service call on ctx land in the audit log under item,
// at the audit log level.
func (w *Worker) recording(ctx context.Context, item string) context.Context {
	if !w.auditCalls {
		return ctx
	}
	return httpjson.WithRecorder(ctx, func(c httpjson.Call) { w.DB.Audit(ctx, "worker", "api call", item, c.String()) })
}

// Loop ticks until ctx ends.
func (w *Worker) Loop(ctx context.Context, every time.Duration) {
	if err := w.DB.FailInterruptedDeletions(ctx); err != nil {
		log.Printf("deleter: %v", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick checks disk space and watched episodes, sends what's waiting for
// Discord and runs everything that's due.
func (w *Worker) Tick(ctx context.Context) {
	level, err := w.DB.LogLevel(ctx)
	if err != nil {
		log.Printf("deleter: log level: %v", err) // not knowing it means recording everything
	}
	w.auditCalls = level != storage.LogNormal
	ctx = w.recording(ctx, "")
	if !w.Now().Before(w.spaceAfter) {
		w.spaceAfter = w.Now().Add(spaceCheck)
		if err := w.checkSpace(ctx); err != nil {
			log.Printf("deleter: space check: %v", err)
		}
		if err := w.checkEphemeral(ctx); err != nil {
			log.Printf("deleter: ephemeral check: %v", err)
		}
		if err := w.DB.PruneAudit(ctx, w.Now()); err != nil {
			log.Printf("deleter: prune audit log: %v", err)
		}
	}
	window, err := w.DB.DeleteWindow(ctx)
	if err != nil {
		log.Printf("deleter: delete window: %v", err) // not knowing it means not deleting
		return
	}
	if err := w.notify(ctx, window); err != nil {
		log.Printf("deleter: notify: %v", err)
	}
	if err := w.announce(ctx); err != nil {
		log.Printf("deleter: announce: %v", err)
	}
	if err := w.digest(ctx); err != nil {
		log.Printf("deleter: digest: %v", err)
	}
	// Times are already set to fall in the window; this covers a window
	// changed after they were set.
	if !window.Contains(w.Now()) {
		return
	}
	for ctx.Err() == nil {
		d, found, err := w.DB.ClaimDueDeletion(ctx, w.Now())
		if err != nil {
			log.Printf("deleter: claim: %v", err)
			return
		}
		if !found {
			return
		}
		w.run(ctx, d)
	}
}

func (w *Worker) notify(ctx context.Context, window storage.DeleteWindow) error {
	if w.Now().Before(w.notifyAfter) || (w.Notify == nil && !w.DryRun) {
		return nil
	}
	ds, err := w.DB.UnnotifiedDeletions(ctx, notifyBatch)
	if err != nil || len(ds) == 0 {
		return err
	}
	shows := map[string]bool{}
	for _, d := range ds {
		after := window.Next(w.Now().Add(storage.DeletionDelay))
		// <t:...> shows in each reader's own time zone.
		text := fmt.Sprintf("**%s** will be deleted <t:%d:f> unless someone presses Keep.", d.Title, after.Unix())
		batch := []storage.Deletion{d}
		// Ephemeral episodes go out as one notice for the show, never one each.
		if d.Kind == storage.KindEphemeral {
			if shows[d.ParentKey] {
				continue
			}
			shows[d.ParentKey] = true
			if batch, err = w.DB.UnnotifiedEpisodes(ctx, d.ParentKey); err != nil {
				return err
			}
			text = fmt.Sprintf("**%s**: watched episodes (%d) will be deleted <t:%d:f> unless someone presses Keep.", d.Title, len(batch), after.Unix())
		}
		if d.DryRun || w.DryRun {
			text += " (dry run: nothing will really be deleted)"
		}
		if w.Notify == nil {
			log.Printf("deleter: dry run, no notifier: %s", text)
		} else if err := w.Notify.Notify(ctx, text, d.ID); err != nil {
			w.notifyAfter = w.Now().Add(notifyRetry)
			return err
		}
		for _, b := range batch {
			if err := w.DB.MarkNotified(ctx, b.ID, after); err != nil {
				return err
			}
		}
	}
	return nil
}

// errBusy puts a deletion back on the clock instead of failing it.
type errBusy struct{ reason string }

func (e errBusy) Error() string { return e.reason }

func (w *Worker) run(ctx context.Context, d storage.Deletion) {
	dry := d.DryRun || w.DryRun
	ctx = w.recording(ctx, d.Title)
	var steps []string
	err := w.execute(ctx, d, dry, &steps)
	// The outcome is recorded even if shutdown is what stopped the steps.
	ctx = context.WithoutCancel(ctx)
	detail := strings.Join(steps, "; ")
	var busy errBusy
	state := storage.DeletionDone
	switch {
	case errors.As(err, &busy):
		log.Printf("deleter: %s: pushed back: %s", d.Title, busy.reason)
		until := w.Now().Add(playingWait)
		if window, err := w.DB.DeleteWindow(ctx); err == nil {
			until = window.Next(until)
		}
		if err := w.DB.PushBackDeletion(ctx, d.ID, until, busy.reason); err != nil {
			log.Printf("deleter: push back: %v", err)
		}
		return
	case err != nil:
		state = storage.DeletionFailed
		detail = strings.TrimPrefix(detail+"; failed: "+err.Error(), "; ")
	}
	log.Printf("deleter: %s: %s (dry run: %v): %s", d.Title, state, dry, detail)
	w.DB.Audit(ctx, "worker", "deletion "+state, d.Title, d.Kind)
	if err := w.DB.FinishDeletion(ctx, d.ID, state, detail); err != nil {
		log.Printf("deleter: finish: %v", err)
	}
}

func (w *Worker) execute(ctx context.Context, d storage.Deletion, dry bool, steps *[]string) error {
	target := d.RatingKey
	if d.ParentKey != "" {
		target = d.ParentKey
	}
	m, seerrMedia, found, err := w.DB.DeletionTarget(ctx, target)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no longer in the library")
	}
	// Either source answering is enough; both failing means not deleting.
	plexNow, plexErr := w.Plex.Sessions(ctx)
	tautulliNow, tautulliErr := w.Tautulli.Activity(ctx)
	if plexErr != nil && tautulliErr != nil {
		return errBusy{"couldn't check what's playing: " + tautulliErr.Error()}
	}
	for _, s := range plexNow {
		if s.RatingKey == d.RatingKey || s.GrandparentRatingKey == d.RatingKey {
			return errBusy{"being watched"}
		}
	}
	for _, s := range tautulliNow {
		if strconv.FormatInt(int64(s.RatingKey), 10) == d.RatingKey || strconv.FormatInt(int64(s.GrandparentRatingKey), 10) == d.RatingKey {
			return errBusy{"being watched"}
		}
	}

	do := func(name string, fn func() error) error {
		if dry {
			*steps = append(*steps, "would "+name)
			w.DB.Audit(ctx, "worker", "deletion step (dry run)", d.Title, name)
			return nil
		}
		if err := fn(); err != nil {
			w.DB.Audit(ctx, "worker", "deletion step failed", d.Title, name+": "+err.Error())
			return fmt.Errorf("%s: %w", name, err)
		}
		*steps = append(*steps, name)
		w.DB.Audit(ctx, "worker", "deletion step", d.Title, name)
		return nil
	}

	// The path comes from live state, not the last sync: Plex delete must
	// never touch something Radarr or Sonarr manages.
	var arrID sql.NullInt64
	if m.Type == "movie" {
		arrID, err = w.liveMovie(ctx, m)
	} else {
		arrID, err = w.liveSeries(ctx, m)
	}
	if err != nil {
		return err
	}
	if m.ArrID.Valid && !arrID.Valid {
		return errors.New("was in Radarr/Sonarr at the last sync but isn't now; left alone")
	}

	// An episode leaves the series and its Seerr request alone, so later ones
	// keep coming; only Sonarr can stop it being grabbed again.
	if d.ParentKey != "" {
		if !arrID.Valid {
			return errors.New("not in Sonarr, so nothing can stop it coming back; left alone")
		}
		if err := w.deleteEpisode(ctx, arrID.Int64, d, do); err != nil {
			return err
		}
		seerrMedia = nil
	}

	switch {
	case d.ParentKey != "":
	case !arrID.Valid:
		if err := do("delete item and files in plex", func() error { return w.Plex.DeleteItem(ctx, m.RatingKey) }); err != nil {
			return err
		}
	case m.Type == "movie":
		if err := w.deleteMovie(ctx, arrID.Int64, do); err != nil {
			return err
		}
	default:
		if err := w.deleteSeries(ctx, arrID.Int64, do); err != nil {
			return err
		}
	}

	for _, id := range seerrMedia {
		if err := do("remove media and requests from seerr", func() error {
			var se *httpjson.StatusError
			if err := w.Seerr.DeleteMedia(ctx, id); err != nil && (!errors.As(err, &se) || se.Code != http.StatusNotFound) {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if !arrID.Valid {
		return nil
	}
	if err := do("refresh plex library", func() error { return w.Plex.RefreshSection(ctx, m.SectionKey) }); err != nil {
		return err
	}
	return do("empty plex trash", func() error { return w.Plex.EmptyTrash(ctx, m.SectionKey) })
}

func (w *Worker) liveMovie(ctx context.Context, m storage.Media) (sql.NullInt64, error) {
	var id sql.NullInt64
	movies, err := w.Radarr.Movies(ctx)
	if err != nil {
		return id, err
	}
	for _, rm := range movies {
		if m.TmdbID.Valid && rm.TmdbID == m.TmdbID.Int64 {
			id = sql.NullInt64{Int64: rm.ID, Valid: true}
		}
	}
	return id, nil
}

func (w *Worker) liveSeries(ctx context.Context, m storage.Media) (sql.NullInt64, error) {
	var id sql.NullInt64
	series, err := w.Sonarr.Series(ctx)
	if err != nil {
		return id, err
	}
	for _, s := range series {
		if m.TvdbID.Valid && s.TvdbID == m.TvdbID.Int64 {
			id = sql.NullInt64{Int64: s.ID, Valid: true}
		}
	}
	return id, nil
}

type stepFn func(name string, fn func() error) error

// deleteMovie: unmonitor, clear the queue, then delete. Never reorder these:
// files must not go while Radarr could still grab the movie.
func (w *Worker) deleteMovie(ctx context.Context, id int64, do stepFn) error {
	if err := do("unmonitor in radarr", func() error { return w.Radarr.UnmonitorMovie(ctx, id) }); err != nil {
		return err
	}
	queue, err := w.Radarr.MovieQueue(ctx, id)
	if err != nil {
		return err
	}
	for _, q := range queue {
		if err := do("remove "+q.Title+" from radarr queue", func() error { return w.Radarr.RemoveFromQueue(ctx, q.ID) }); err != nil {
			return err
		}
	}
	return do("delete movie and files in radarr, with import exclusion", func() error { return w.Radarr.DeleteMovie(ctx, id) })
}

// deleteEpisode is deleteMovie for one episode; the same order rule applies.
func (w *Worker) deleteEpisode(ctx context.Context, seriesID int64, d storage.Deletion, do stepFn) error {
	eps, err := w.Sonarr.Episodes(ctx, seriesID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(eps, func(e arr.Episode) bool { return e.SeasonNumber == d.Season && e.EpisodeNumber == d.Episode })
	if i < 0 {
		return errors.New("episode not in sonarr; left alone")
	}
	ep := eps[i]
	if err := do("unmonitor episode in sonarr", func() error { return w.Sonarr.UnmonitorEpisodes(ctx, []int64{ep.ID}) }); err != nil {
		return err
	}
	queue, err := w.Sonarr.SeriesQueue(ctx, seriesID, []int64{ep.ID})
	if err != nil {
		return err
	}
	for _, q := range queue {
		if err := do("remove "+q.Title+" from sonarr queue", func() error { return w.Sonarr.RemoveFromQueue(ctx, q.ID) }); err != nil {
			return err
		}
	}
	if ep.EpisodeFileID == 0 {
		return nil // already gone; it's unmonitored now, which is what matters
	}
	return do(fmt.Sprintf("delete S%02dE%02d file in sonarr", d.Season, d.Episode), func() error { return w.Sonarr.DeleteEpisodeFile(ctx, ep.EpisodeFileID) })
}

// deleteSeries is deleteMovie for Sonarr; the same order rule applies.
func (w *Worker) deleteSeries(ctx context.Context, id int64, do stepFn) error {
	if err := do("unmonitor in sonarr", func() error { return w.Sonarr.UnmonitorSeries(ctx, id) }); err != nil {
		return err
	}
	queue, err := w.Sonarr.SeriesQueue(ctx, id, nil)
	if err != nil {
		return err
	}
	for _, q := range queue {
		if err := do("remove "+q.Title+" from sonarr queue", func() error { return w.Sonarr.RemoveFromQueue(ctx, q.ID) }); err != nil {
			return err
		}
	}
	return do("delete series and files in sonarr, with import list exclusion", func() error { return w.Sonarr.DeleteSeries(ctx, id) })
}
