package deleter

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

const (
	spaceCheck = time.Hour
)

// checkSpace schedules space-based items, oldest watched first, on each disk
// used past the limit, until enough is on its way out to get back under it.
// They then go through the usual notice and delay. Any failed read means
// doing nothing this round.
func (w *Worker) checkSpace(ctx context.Context) error {
	limit, err := w.DB.SpaceLimit(ctx)
	if err != nil {
		return err
	}
	items, err := w.DB.SpaceItems(ctx, w.DryRun)
	if err != nil || len(items) == 0 {
		return err
	}
	radarrDisks, err := w.Radarr.DiskSpace(ctx)
	if err != nil {
		return err
	}
	sonarrDisks, err := w.Sonarr.DiskSpace(ctx)
	if err != nil {
		return err
	}
	movies, err := w.Radarr.Movies(ctx)
	if err != nil {
		return err
	}
	series, err := w.Sonarr.Series(ctx)
	if err != nil {
		return err
	}
	moviePaths := map[int64]string{}
	for _, m := range movies {
		moviePaths[m.TmdbID] = m.Path
	}
	seriesPaths := map[int64]string{}
	for _, s := range series {
		seriesPaths[s.TvdbID] = s.Path
	}

	all := slices.Concat(radarrDisks, sonarrDisks)
	over := map[int]int64{} // bytes still to free, by index of the disk's first entry in all
	for _, it := range items {
		disks, path := radarrDisks, moviePaths[it.TmdbID.Int64]
		if it.Type != "movie" {
			disks, path = sonarrDisks, seriesPaths[it.TvdbID.Int64]
		}
		d, found := diskFor(disks, path)
		if !found {
			continue // no longer managed, so nothing says which disk it's on
		}
		i := slices.IndexFunc(all, func(o arr.DiskSpace) bool { return sameFS(o, d) })
		if _, seen := over[i]; !seen {
			over[i] = d.TotalSpace - d.FreeSpace - d.TotalSpace/100*int64(limit)
		}
		if it.Active {
			over[i] -= it.SizeBytes
			continue
		}
		if over[i] <= 0 {
			continue
		}
		// shortcut: pending deletions of items that aren't space-based don't count, so one extra item can go; count them if that matters.
		scheduled, err := w.DB.ScheduleDeletion(ctx, it.RatingKey, 0, w.DryRun, storage.KindSpace) // 0: nobody pressed anything
		if err != nil {
			return fmt.Errorf("schedule %s: %w", it.Title, err)
		}
		if scheduled {
			over[i] -= it.SizeBytes
			log.Printf("deleter: %s scheduled: %s is over %d%% used", it.Title, d.Path, limit)
			w.DB.Audit(ctx, "worker", "schedule deletion", it.RatingKey, fmt.Sprintf("space: %s over %d%%", d.Path, limit))
		}
	}
	return nil
}

// The disk helpers live in arr, shared with the doctor's space card.
var (
	diskFor = arr.DiskFor
	sameFS  = arr.SameFS
)
