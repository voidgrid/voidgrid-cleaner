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
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

const (
	notifyBatch = 30 // titles per message, well inside Discord's size limit
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
	}
	SonarrAPI interface {
		Series(context.Context) ([]arr.Series, error)
		UnmonitorSeries(context.Context, int64) error
		SeriesQueue(context.Context, int64, []int64) ([]arr.QueueItem, error)
		RemoveFromQueue(context.Context, int64) error
		DeleteSeries(context.Context, int64) error
	}
	PlexAPI interface {
		RefreshSection(context.Context, string) error
		EmptyTrash(context.Context, string) error
		DeleteItem(context.Context, string) error
	}
	SeerrAPI interface {
		DeleteMedia(context.Context, int64) error
	}
	TautulliAPI interface {
		Activity(context.Context) ([]tautulli.Session, error)
	}
	Notifier interface {
		Notify(ctx context.Context, title, text string) error
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

	notifyAfter time.Time
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

// Tick sends due notifications and runs everything that's due.
func (w *Worker) Tick(ctx context.Context) {
	if err := w.notify(ctx); err != nil {
		log.Printf("deleter: notify: %v", err)
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

func (w *Worker) notify(ctx context.Context) error {
	if w.Now().Before(w.notifyAfter) || (w.Notify == nil && !w.DryRun) {
		return nil
	}
	ds, err := w.DB.UnnotifiedDeletions(ctx, notifyBatch)
	if err != nil || len(ds) == 0 {
		return err
	}
	lines := make([]string, len(ds))
	for i, d := range ds {
		lines[i] = "- " + d.Title
		if d.DryRun || w.DryRun {
			lines[i] += " (dry run, nothing will be deleted)"
		}
	}
	text := "Deleting in 24 hours unless kept:\n" + strings.Join(lines, "\n")
	if w.Notify == nil {
		log.Printf("deleter: dry run, no notifier: %s", text)
	} else if err := w.Notify.Notify(ctx, "Scheduled for deletion", text); err != nil {
		w.notifyAfter = w.Now().Add(notifyRetry)
		return err
	}
	return w.DB.MarkNotified(ctx, ds, w.Now().Add(storage.DeletionDelay))
}

// errBusy puts a deletion back on the clock instead of failing it.
type errBusy struct{ reason string }

func (e errBusy) Error() string { return e.reason }

func (w *Worker) run(ctx context.Context, d storage.Deletion) {
	dry := d.DryRun || w.DryRun
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
		if err := w.DB.PushBackDeletion(ctx, d.ID, w.Now().Add(playingWait), busy.reason); err != nil {
			log.Printf("deleter: push back: %v", err)
		}
		return
	case err != nil:
		state = storage.DeletionFailed
		detail = strings.TrimPrefix(detail+"; failed: "+err.Error(), "; ")
	}
	log.Printf("deleter: %s: %s (dry run: %v): %s", d.Title, state, dry, detail)
	if err := w.DB.FinishDeletion(ctx, d.ID, state, detail); err != nil {
		log.Printf("deleter: finish: %v", err)
	}
}

func (w *Worker) execute(ctx context.Context, d storage.Deletion, dry bool, steps *[]string) error {
	m, seerrMedia, found, err := w.DB.DeletionTarget(ctx, d.RatingKey)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no longer in the library")
	}
	sessions, err := w.Tautulli.Activity(ctx)
	if err != nil {
		return errBusy{"couldn't check what's playing: " + err.Error()}
	}
	for _, s := range sessions {
		if strconv.FormatInt(int64(s.RatingKey), 10) == m.RatingKey || strconv.FormatInt(int64(s.GrandparentRatingKey), 10) == m.RatingKey {
			return errBusy{"being watched"}
		}
	}

	do := func(name string, fn func() error) error {
		if dry {
			*steps = append(*steps, "would "+name)
			return nil
		}
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*steps = append(*steps, name)
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

	switch {
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
