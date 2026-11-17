package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// removedPrefix marks a health finding about a movie TMDb removed.
const removedPrefix = "radarr:removed:"

// What can be done about something missing.
const (
	doSearch        = "search"
	doMonitorSearch = "monitor-search"
	doUnmonitor     = "unmonitor"
	doRemove        = "remove"
	// doRemoveAll takes an item with no media out of Seerr, off the disk and out
	// of Radarr or Sonarr in one go, so it doesn't turn up again as a stale
	// request or an untracked folder.
	doRemoveAll = "remove-all"
)

func (d *Doctor) previewMissing(p *Preview, data fixData) {
	p.CanFix = true
	switch {
	case data.Episodes != nil:
		p.Lines = []string{"Episodes: " + data.OldName + "."}
		p.Choices = []Choice{{Value: doSearch, Label: "search for them"}, {Value: doUnmonitor, Label: "stop monitoring them"}}
	case data.App == appSonarr:
		p.Lines = []string{"Sonarr has this series and no files for it."}
		p.Choices = []Choice{{Value: doSearch, Label: "search for the whole series"}, {Value: doUnmonitor, Label: "stop monitoring it"}, {Value: doRemoveAll, Label: "remove it from Seerr, disk and Sonarr"}}
	case data.Monitored:
		p.Lines = []string{"Radarr has this movie and no file for it."}
		p.Choices = []Choice{{Value: doSearch, Label: "search for it"}, {Value: doUnmonitor, Label: "stop monitoring it"}, {Value: doRemoveAll, Label: "remove it from Seerr, disk and Radarr"}}
	default:
		p.Lines = []string{"Radarr has this movie, unmonitored, with no file, so nothing will fetch it."}
		p.Choices = []Choice{{Value: doMonitorSearch, Label: "monitor it and search"}, {Value: doRemoveAll, Label: "remove it from Seerr, disk and Radarr"}}
	}
	p.Lines = append(p.Lines, "A search can download something, depending on what your indexers have. Removing deletes its folder, adds no exclusion, and clears its Seerr request, so it can be requested again.")
}

func (d *Doctor) applyMissing(ctx context.Context, data fixData, choice string) (string, error) {
	switch {
	case data.Episodes != nil:
		return d.applyEpisodes(ctx, data, choice)
	case data.App == appSonarr:
		return d.applySeries(ctx, data, choice)
	}
	return d.applyMovie(ctx, data, choice)
}

func (d *Doctor) applyEpisodes(ctx context.Context, data fixData, choice string) (string, error) {
	switch choice {
	case doSearch:
		if _, err := d.Sonarr.Command(ctx, map[string]any{"name": "EpisodeSearch", "episodeIds": data.Episodes}); err != nil {
			return "", err
		}
		return "search started; check the queue", nil
	case doUnmonitor:
		if err := d.Sonarr.UnmonitorEpisodes(ctx, data.Episodes); err != nil {
			return "", err
		}
		return "no longer monitored", nil
	}
	return "", errors.New("nothing chosen")
}

func (d *Doctor) applySeries(ctx context.Context, data fixData, choice string) (string, error) {
	switch choice {
	case doSearch:
		if _, err := d.Sonarr.Command(ctx, map[string]any{"name": "SeriesSearch", "seriesId": data.ArrID}); err != nil {
			return "", err
		}
		return "search started; check the queue", nil
	case doUnmonitor:
		if err := d.Sonarr.UnmonitorSeries(ctx, data.ArrID); err != nil {
			return "", err
		}
		return "no longer monitored", nil
	case doRemoveAll:
		s, err := d.Sonarr.OneSeries(ctx, data.ArrID)
		if err != nil {
			return "", err
		}
		if s.Statistics.EpisodeFileCount > 0 {
			return "", errors.New("it has files now, so it was left in Sonarr")
		}
		if err := d.noVideoIn(ctx, s.Path); err != nil {
			return "", err
		}
		if d.DryRun {
			return "dry run: would have removed " + s.Title + " from Seerr, disk and Sonarr", nil
		}
		if err := d.Sonarr.RemoveSeriesAndFiles(ctx, data.ArrID); err != nil {
			return "", err
		}
		return d.removeRequest(ctx, "removed from sonarr and disk", "tv", data)
	}
	return "", errors.New("nothing chosen")
}

func (d *Doctor) applyMovie(ctx context.Context, data fixData, choice string) (string, error) {
	switch choice {
	case doMonitorSearch:
		if err := d.Radarr.MonitorMovie(ctx, data.ArrID); err != nil {
			return "", err
		}
		fallthrough
	case doSearch:
		if _, err := d.Radarr.Command(ctx, map[string]any{"name": "MoviesSearch", "movieIds": []int64{data.ArrID}}); err != nil {
			return "", err
		}
		return "search started; check the queue", nil
	case doUnmonitor:
		if err := d.Radarr.UnmonitorMovie(ctx, data.ArrID); err != nil {
			return "", err
		}
		return "no longer monitored", nil
	case doRemoveAll:
		m, err := d.Radarr.Movie(ctx, data.ArrID)
		if err != nil {
			return "", err
		}
		if m.HasFile {
			return "", errors.New("it has a file now, so it was left in Radarr")
		}
		if err := d.noVideoIn(ctx, m.Path); err != nil {
			return "", err
		}
		if d.DryRun {
			return "dry run: would have removed " + m.Title + " from Seerr, disk and Radarr", nil
		}
		if err := d.Radarr.RemoveMovieAndFiles(ctx, data.ArrID); err != nil {
			return "", err
		}
		return d.removeRequest(ctx, "removed from radarr and disk", "movie", data)
	}
	return "", errors.New("nothing chosen")
}

func (d *Doctor) previewRemovedMovie(p *Preview, data fixData) {
	p.CanFix = true
	p.Lines = []string{"TMDb removed this movie, so Radarr can't update it and warns about it until it's gone from Radarr."}
	p.Choices = []Choice{{Value: doRemove, Label: "remove it from Radarr and keep the file"}}
	if data.HasFile {
		p.Lines = append(p.Lines, "The file stays and Plex keeps playing it, but Radarr stops tracking it, so it will show as an untracked folder. If the warning doesn't bother you, dismiss this instead.")
	}
}

func (d *Doctor) applyRemovedMovie(ctx context.Context, data fixData, choice string) (string, error) {
	if choice != doRemove {
		return "", errors.New("nothing chosen")
	}
	if err := d.Radarr.RemoveMovie(ctx, data.ArrID); err != nil {
		return "", err
	}
	return "removed from radarr; any file stayed", nil
}

// episodeList writes "S07E02 Chrysalis, S07E03 Next" for a preview.
func episodeList(parts []string) string {
	const show = 10
	if len(parts) > show {
		return strings.Join(parts[:show], ", ") + fmt.Sprintf(" and %d more", len(parts)-show)
	}
	return strings.Join(parts, ", ")
}

// noVideoIn refuses when a folder holds a video the app doesn't track: the
// removal deletes the whole folder, so it must hold no media. A folder that's
// already gone is fine.
func (d *Doctor) noVideoIn(ctx context.Context, folder string) error {
	if folder == "" {
		return errors.New("it has no folder to check, so it was left alone")
	}
	if _, err := d.managedRoot(ctx, folder+"/x"); err != nil {
		return err
	}
	here, err := d.local(ctx, folder)
	if err != nil {
		return err
	}
	if _, err := os.Stat(here); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	names, err := folderEntries(here)
	if err != nil {
		return err
	}
	for _, n := range names {
		if isVideoFile(n) {
			return errors.New("a video is in its folder, so it was left alone")
		}
	}
	return nil
}

// removeRequest finishes a removal by clearing the item's Seerr request, if it
// has one. By then the item is gone from Radarr or Sonarr, so a failure here
// says so.
func (d *Doctor) removeRequest(ctx context.Context, done, mediaType string, data fixData) (string, error) {
	id, ok, err := d.DB.SeerrMediaID(ctx, mediaType, data.TmdbID, data.TvdbID)
	if err != nil {
		return "", fmt.Errorf("%s, but looking up its request failed: %w", done, err)
	}
	if !ok {
		return done + "; it had no request in seerr", nil
	}
	if _, err := d.applyStale(ctx, fixData{MediaID: id}); err != nil {
		return "", fmt.Errorf("%s, but removing its seerr request failed: %w", done, err)
	}
	return done + " and seerr", nil
}
