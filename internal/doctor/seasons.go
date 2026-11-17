package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// KindDuplicateSeason is a show folder holding two folders for one season
// ("Season 01" and "Season 1"). Sonarr wants the files in one of them, and
// every rename into it fails on the copies already there.
const KindDuplicateSeason = "duplicate_season"

var (
	seasonFolder = regexp.MustCompile(`(?i)^season\s*0*(\d+)$`)
	// episodeTag finds S01E02, and S01E02E03 or S01E02-E03 for a double episode.
	episodeTag   = regexp.MustCompile(`(?i)\bS(\d+)E(\d+)((?:-?E\d+)*)`)
	extraEpisode = regexp.MustCompile(`(?i)E(\d+)`)
)

// episodeKeys names the episodes a file name holds, like "1x24".
func episodeKeys(name string) []string {
	m := episodeTag.FindStringSubmatch(name)
	if m == nil {
		return nil
	}
	season, _ := strconv.Atoi(m[1])
	first, _ := strconv.Atoi(m[2])
	keys := []string{fmt.Sprintf("%dx%d", season, first)}
	for _, e := range extraEpisode.FindAllStringSubmatch(m[3], -1) {
		n, _ := strconv.Atoi(e[1])
		keys = append(keys, fmt.Sprintf("%dx%d", season, n))
	}
	return keys
}

// videoEpisodes lists the episodes of every video under dir. unnamed counts
// videos whose name says no episode, which can't be shown to be covered.
func videoEpisodes(dir string) (episodes map[string]bool, unnamed int, err error) {
	episodes = map[string]bool{}
	names, err := folderEntries(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, n := range names {
		if !isVideoFile(n) {
			continue
		}
		keys := episodeKeys(path.Base(n))
		if len(keys) == 0 {
			unnamed++
		}
		for _, k := range keys {
			episodes[k] = true
		}
	}
	return episodes, unnamed, nil
}

// uncovered says why a folder can't be removed: an episode only it holds. An
// empty result means every video in extra has a copy in keep.
func uncovered(extra, keep string) (string, error) {
	have, _, err := videoEpisodes(keep)
	if err != nil {
		return "", err
	}
	want, unnamed, err := videoEpisodes(extra)
	if err != nil {
		return "", err
	}
	var missing []string
	for k := range want {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	switch {
	case len(missing) > 0:
		return "only it has " + strings.Join(missing, ", ") + " (season x episode)", nil
	case unnamed > 0:
		return fmt.Sprintf("%d of its videos don't name an episode", unnamed), nil
	}
	return "", nil
}

// duplicateSeasons finds seasons that have two folders in a series' folder.
// The one to keep is the one Sonarr's rename wants, or failing that the one
// holding the files it tracks.
func (d *Doctor) duplicateSeasons(ctx context.Context, s *snapshot) ([]storage.Finding, error) {
	var out []storage.Finding
	for _, sr := range s.series {
		if !s.inManaged(sr.Path) {
			continue
		}
		entries, err := os.ReadDir(s.local(sr.Path))
		if err != nil {
			continue
		}
		bySeason := map[string][]string{}
		for _, e := range entries {
			if m := seasonFolder.FindStringSubmatch(e.Name()); e.IsDir() && m != nil {
				bySeason[m[1]] = append(bySeason[m[1]], e.Name())
			}
		}
		for season, names := range bySeason {
			if len(names) < 2 {
				continue
			}
			keep, err := d.keepFolder(ctx, sr.ID, sr.Path, names)
			if err != nil {
				return nil, err
			}
			slices.Sort(names)
			for _, n := range names {
				if n == keep {
					continue
				}
				f := storage.Finding{Subject: path.Join(sr.Path, n), Title: sr.Title + " / " + n, Severity: check}
				data := fixData{App: appSonarr, ArrID: sr.ID, Folder: path.Join(sr.Path, n)}
				switch keep {
				case "":
					data.Reason = "can't tell which of " + strings.Join(names, " and ") + " Sonarr uses"
				default:
					data.Keep = path.Join(sr.Path, keep)
					data.Reason, err = uncovered(s.local(data.Folder), s.local(data.Keep))
					if err != nil {
						continue // unreadable now; the next run looks again
					}
				}
				f.Detail = fmt.Sprintf("Season %s has two folders, and Sonarr can't rename into %s.", season, keep)
				if keep == "" {
					f.Detail = "Season " + season + " has two folders."
				}
				if data.Reason != "" {
					f.Detail += " Not removable: " + data.Reason + "."
				}
				f.Data = encode(data)
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// keepFolder picks which of a season's folders stays: the one Sonarr's rename
// moves files into, else the one holding the files it tracks. "" means neither.
func (d *Doctor) keepFolder(ctx context.Context, seriesID int64, seriesPath string, names []string) (string, error) {
	renames, err := d.Sonarr.RenamePreview(ctx, seriesID)
	if err != nil {
		return "", err
	}
	for _, r := range renames {
		if dir := path.Base(path.Dir(r.NewPath)); slices.Contains(names, dir) {
			return dir, nil
		}
	}
	files, err := d.Sonarr.EpisodeFiles(ctx, seriesID)
	if err != nil {
		return "", err
	}
	var holding []string
	for _, f := range files {
		rel, ok := strings.CutPrefix(f.Path, seriesPath+"/")
		if dir, _, _ := strings.Cut(rel, "/"); ok && slices.Contains(names, dir) && !slices.Contains(holding, dir) {
			holding = append(holding, dir)
		}
	}
	if len(holding) == 1 {
		return holding[0], nil
	}
	return "", nil
}

func (d *Doctor) previewDuplicateSeason(p *Preview, data fixData) {
	if data.Reason != "" {
		p.CanFix = false
		p.Lines = []string{"Can't remove " + data.Folder + ": " + data.Reason + "."}
		return
	}
	p.Lines = []string{
		"Remove " + data.Folder + " for good, with everything in it. Every episode in it also has a file in " + data.Keep + ".",
		"Then Sonarr rescans the series, so it tracks the files in " + path.Base(data.Keep) + ". It can't be undone.",
	}
	if d.DryRun {
		p.Lines = append(p.Lines, "This is a dry run, so it will only report what it would remove.")
	}
}

func (d *Doctor) applyDuplicateSeason(ctx context.Context, data fixData) (string, error) {
	if data.Reason != "" || data.Keep == "" {
		return "", errors.New("it isn't safe to remove: " + data.Reason)
	}
	root, err := d.managedRoot(ctx, data.Folder)
	if err != nil {
		return "", err
	}
	here, err := d.local(ctx, data.Folder)
	if err != nil {
		return "", err
	}
	keep, err := d.local(ctx, data.Keep)
	if err != nil {
		return "", err
	}
	// Look again right before removing: files move.
	if why, err := uncovered(here, keep); err != nil {
		return "", err
	} else if why != "" {
		return "", errors.New("left alone: " + why)
	}
	names, err := folderEntries(here)
	if err != nil {
		return "", err
	}
	if d.DryRun {
		return fmt.Sprintf("dry run: would have removed %s and %d things in it", path.Base(data.Folder), len(names)), nil
	}
	targets := []string{data.Folder}
	for _, n := range names {
		targets = append(targets, path.Join(data.Folder, n))
	}
	slices.Reverse(targets)
	localRoot, err := d.local(ctx, root)
	if err != nil {
		return "", err
	}
	if err := removeUnder(localRoot, filepath.Clean(root), targets...); err != nil {
		return "", err
	}
	if err := d.runCommand(ctx, d.Sonarr, map[string]any{"name": "RescanSeries", "seriesId": data.ArrID}); err != nil {
		return "", fmt.Errorf("removed %s, but the rescan failed: %w", path.Base(data.Folder), err)
	}
	return "removed " + path.Base(data.Folder) + " and rescanned", nil
}
