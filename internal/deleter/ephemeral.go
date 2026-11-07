package deleter

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// checkEphemeral schedules every episode all of its watchers have seen.
func (w *Worker) checkEphemeral(ctx context.Context) error {
	eps, err := w.DB.WatchedEpisodes(ctx, w.DryRun)
	if err != nil {
		return err
	}
	for _, e := range eps {
		ok, err := w.DB.ScheduleEpisode(ctx, e, w.DryRun)
		if err != nil {
			return fmt.Errorf("schedule %s: %w", e.Title, err)
		}
		if ok {
			w.DB.Audit(ctx, "worker", "schedule deletion", e.Title, fmt.Sprintf("ephemeral: S%02dE%02d", e.Season, e.Episode))
		}
	}
	return nil
}

// announce posts newly ephemeral shows and tells the admin about Add me
// presses from people nobody has linked. Rows are marked only once Discord
// confirms, so a failure is retried.
func (w *Worker) announce(ctx context.Context) error {
	if w.Notify == nil || w.Now().Before(w.notifyAfter) {
		return nil
	}
	shows, err := w.DB.UnannouncedShows(ctx, notifyBatch)
	if err != nil {
		return err
	}
	for _, s := range shows {
		text := fmt.Sprintf("**%s** is now ephemeral: each episode is deleted once everyone on it has watched it. Press Add me if you're watching too.", s.Title)
		if s.Watchers != "" {
			text += " On it: " + s.Watchers + "."
		}
		if err := w.Notify.Announce(ctx, text, s.RatingKey); err != nil {
			w.notifyAfter = w.Now().Add(notifyRetry)
			return err
		}
		if err := w.DB.MarkAnnounced(ctx, s.RatingKey); err != nil {
			return err
		}
	}
	joins, err := w.DB.UnalertedJoins(ctx, notifyBatch)
	if err != nil || len(joins) == 0 {
		return err
	}
	admin, err := w.DB.AdminDiscord(ctx)
	if err != nil {
		return err
	}
	for _, j := range joins {
		name := strings.NewReplacer("*", "", "`", "", "@", "").Replace(j.Name)
		text := fmt.Sprintf("%s pressed Add me on **%s** but isn't linked to a Seerr user. Link them on the users page and they'll be added.", name, j.Title)
		if err := w.Notify.Alert(ctx, text, admin); err != nil {
			w.notifyAfter = w.Now().Add(notifyRetry)
			return err
		}
		if err := w.DB.MarkAlerted(ctx, j.DiscordID, j.RatingKey); err != nil {
			return err
		}
	}
	log.Printf("deleter: told the admin about %d unlinked Add me presses", len(joins))
	return nil
}
