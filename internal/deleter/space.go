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

// checkSpace schedules deletions on each disk used past its range's start,
// enough to bring it down to its floor: space-based items first, oldest
// watched first, then normal items longest unwatched first. Deletions already
// on their way count. They go through the usual notice and delay. Any failed
// read means doing nothing this round.
func (w *Worker) checkSpace(ctx context.Context) error {
	ranges, err := w.DB.SpaceRanges(ctx)
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
	diskOf := func(it storage.SpaceItem) (arr.DiskSpace, bool) {
		if it.Type == "movie" {
			return diskFor(radarrDisks, moviePaths[it.TmdbID.Int64])
		}
		return diskFor(sonarrDisks, seriesPaths[it.TvdbID.Int64])
	}
	for _, p := range planSpace(ranges, slices.Concat(radarrDisks, sonarrDisks), diskOf, items) {
		scheduled, err := w.DB.ScheduleDeletion(ctx, p.item.RatingKey, 0, w.DryRun, storage.KindSpace) // 0: nobody pressed anything
		if err != nil {
			return fmt.Errorf("schedule %s: %w", p.item.Title, err)
		}
		if scheduled {
			rng := ranges.For(p.disk.Path)
			why := fmt.Sprintf("%s is over %d%% used, clearing to %d%%", p.disk.Path, rng.Start, rng.Floor)
			log.Printf("deleter: %s scheduled: %s", p.item.Title, why)
			w.DB.Audit(ctx, "worker", "schedule deletion", p.item.RatingKey, "space: "+why)
		}
	}
	return nil
}

type spacePick struct {
	item storage.SpaceItem
	disk arr.DiskSpace
}

// planSpace picks items, in the order given, until each disk past its start
// (reaching it counts) is down to its floor once they and everything already
// going are deleted. Picks name the disk as arr.Canonical does.
func planSpace(ranges storage.SpaceRanges, all []arr.DiskSpace, diskOf func(storage.SpaceItem) (arr.DiskSpace, bool), items []storage.SpaceItem) []spacePick {
	var picks []spacePick
	need := map[string]int64{} // bytes still to free, by disk
	for _, it := range items {
		d, found := diskOf(it)
		if !found {
			continue // no longer managed, so nothing says which disk it's on
		}
		d = arr.Canonical(all, d)
		i := d.Path
		if _, seen := need[i]; !seen {
			rng := ranges.For(i)
			used := d.TotalSpace - d.FreeSpace
			need[i] = 0
			if used*100 >= d.TotalSpace*int64(rng.Start) {
				need[i] = used - d.TotalSpace*int64(rng.Floor)/100
			}
		}
		switch {
		case it.Active:
			need[i] -= it.SizeBytes
		case need[i] > 0:
			picks = append(picks, spacePick{it, d})
			need[i] -= it.SizeBytes
		}
	}
	return picks
}

// diskFor lives in arr, shared with the doctor's space card.
var diskFor = arr.DiskFor
