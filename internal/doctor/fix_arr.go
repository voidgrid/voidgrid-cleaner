package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

const (
	appRadarr = "radarr"
	appSonarr = "sonarr"
	// emptyFoldersSubject is the settings finding about Radarr leaving empty folders.
	emptyFoldersSubject = "radarr:empty"
	// commandTries with FixWait/6 between them allows about a minute at 3 s.
	commandTries = 120
	// removeChoice removes an untracked show folder with no video instead of adding it.
	removeChoice = "remove"
)

// importQualities are the ones a manual import offers, most likely first.
var importQualities = []string{"WEBDL-1080p", "WEBRip-1080p", "Bluray-1080p", "Remux-1080p", "HDTV-1080p", "WEBDL-720p", "Bluray-720p", "DVD", "WEBDL-2160p", "Bluray-2160p"}

func (d *Doctor) previewArr(ctx context.Context, p *Preview, data fixData) error {
	switch p.Finding.Kind {
	case KindNaming:
		p.CanFix = true
		p.Lines = []string{fmt.Sprintf("Rename %s to %s.", data.OldName, data.NewName), "Plex follows the new name on its next scan."}
	case KindUntracked:
		return d.previewUntracked(ctx, p, data)
	case KindNotAccepted:
		return d.previewImport(ctx, p, data)
	case KindMissing:
		d.previewMissing(p, data)
	case KindHealth:
		d.previewRemovedMovie(p, data)
	case KindSettings:
		p.CanFix = true
		p.Lines = []string{"In Radarr, turn on Delete empty folders and turn off Create empty movie folders.", "Movies you add get a folder when they're imported, not before."}
	}
	return nil
}

// hasVideo reports whether a folder holds a video. ok is false if it can't be read.
func hasVideo(dir string) (found, ok bool) {
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		found = found || (!e.IsDir() && isVideoFile(p))
		return nil
	})
	return found, err == nil
}

func (d *Doctor) previewUntracked(ctx context.Context, p *Preview, data fixData) error {
	// Nothing to import, whatever a search finds for the folder's name.
	here, err := d.local(ctx, data.Folder)
	if err != nil {
		return err
	}
	found, ok := hasVideo(here)
	noVideo := ok && !found
	if noVideo && data.App != appSonarr {
		p.Lines = []string{"There's no video in " + data.Folder + ", so there's nothing to add."}
		return nil
	}
	term := folderYear.ReplaceAllString(path.Base(data.Folder), "")
	var results []map[string]any
	key := "tmdbId"
	if data.App == appSonarr {
		key = "tvdbId"
		results, err = d.Sonarr.LookupSeries(ctx, term)
	} else {
		results, err = d.Radarr.LookupMovies(ctx, term)
	}
	if err != nil {
		return err
	}
	for _, r := range results[:min(len(results), 6)] {
		id, ok := r[key].(float64)
		if !ok {
			continue
		}
		p.Choices = append(p.Choices, Choice{Value: strconv.FormatInt(int64(id), 10), Label: fmt.Sprintf("%v (%v)", r["title"], r["year"])})
	}
	p.CanFix = len(p.Choices) > 0
	p.Lines = []string{"Add " + data.Folder + " to " + data.App + " as the one you pick: unmonitored, with no search, so nothing downloads."}
	if !p.CanFix {
		p.Lines = []string{"Nothing found for " + term + ", so this can't be added automatically."}
	}
	if noVideo {
		d.previewRemoveFolder(ctx, p, data, here)
	}
	return nil
}

// previewRemoveFolder offers removing a show folder with no video, beside
// adding it to Sonarr.
func (d *Doctor) previewRemoveFolder(ctx context.Context, p *Preview, data fixData, here string) {
	p.Lines = append([]string{"There's no video in " + data.Folder + "."}, p.Lines...)
	if _, err := d.managedRoot(ctx, data.Folder); err != nil {
		p.Lines = append(p.Lines, "Removing it needs its library path managed on settings.")
		return
	}
	names, err := folderEntries(here)
	if err != nil {
		p.Lines = append(p.Lines, "Couldn't read it: "+err.Error())
		return
	}
	p.CanFix = true
	p.Choices = append(p.Choices, Choice{Value: removeChoice, Label: "remove the folder"})
	line := "Or remove the folder for good"
	if len(names) > 0 {
		shown := names[:min(len(names), 10)]
		line += ", with " + strings.Join(shown, ", ")
		if more := len(names) - len(shown); more > 0 {
			line += fmt.Sprintf(" and %d more", more)
		}
	}
	p.Lines = append(p.Lines, line+". If a video appears in it by then, it's left alone.")
	if d.DryRun {
		p.Lines = append(p.Lines, "This is a dry run, so removing will only report what it would delete.")
	}
}

func (d *Doctor) previewImport(ctx context.Context, p *Preview, data fixData) error {
	files, err := d.Radarr.ImportFiles(ctx, data.ArrID, data.Folder)
	if err != nil {
		return err
	}
	f, ok := importable(files)
	if !ok {
		p.Lines = []string{"Radarr finds no video in " + data.Folder + "."}
		return nil
	}
	p.Lines = []string{"Import " + f.RelativePath + " into Radarr as the quality you pick. The file stays where it is."}
	for _, r := range f.Rejections {
		p.Lines = append(p.Lines, "Radarr's concern: "+r.Reason)
	}
	p.Choices = qualityChoices(data.Resolution)
	p.HasDefault = len(p.Choices) > 0 && p.Choices[0].Default
	if p.HasDefault {
		p.Lines = append(p.Lines, "Plex measured the resolution. No tool can tell the source from the file, so WEB or Bluray is a guess; change it if you know.")
	}
	p.CanFix = true
	return nil
}

// qualityChoices lists the qualities with those matching what Plex measured
// first, and the first of them selected.
func qualityChoices(resolution string) []Choice {
	suffix := map[string]string{"4k": "-2160p", "2160": "-2160p", "1080": "-1080p", "720": "-720p"}[strings.ToLower(resolution)]
	var first, rest []Choice
	for _, q := range importQualities {
		sd := q == "DVD" && (resolution == "sd" || resolution == "480" || resolution == "576")
		if (suffix != "" && strings.HasSuffix(q, suffix)) || sd {
			first = append(first, Choice{Value: q, Label: q})
		} else {
			rest = append(rest, Choice{Value: q, Label: q})
		}
	}
	if len(first) > 0 {
		first[0].Default = true
	}
	return append(first, rest...)
}

func importable(files []arr.ImportFile) (arr.ImportFile, bool) {
	for _, f := range files {
		if isVideoFile(f.Path) {
			return f, true
		}
	}
	return arr.ImportFile{}, false
}

func (d *Doctor) applyArr(ctx context.Context, f storage.Finding, data fixData, choice string) (string, error) {
	switch f.Kind {
	case KindNaming:
		return d.applyRename(ctx, data)
	case KindUntracked:
		return d.applyAdd(ctx, data, choice)
	case KindNotAccepted:
		return d.applyImport(ctx, data, choice)
	case KindMissing:
		return d.applyMissing(ctx, data, choice)
	case KindHealth:
		return d.applyRemovedMovie(ctx, data, choice)
	}
	return d.applyEmptyFolders(ctx)
}

type commander interface {
	Command(context.Context, map[string]any) (int64, error)
	CommandStatus(context.Context, int64) (string, string, error)
}

// runCommand starts a task and waits for its result, so a fix never reports
// done on the strength of having asked.
func (d *Doctor) runCommand(ctx context.Context, c commander, body map[string]any) error {
	id, err := c.Command(ctx, body)
	if err != nil {
		return err
	}
	for try := 0; try < commandTries; try++ {
		status, message, err := c.CommandStatus(ctx, id)
		if err != nil {
			return err
		}
		switch status {
		case "completed":
			return nil
		case "failed", "aborted", "cancelled", "orphaned":
			return fmt.Errorf("%v failed: %s", body["name"], message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.FixWait / 6):
		}
	}
	return fmt.Errorf("%v didn't finish in time; check its queue", body["name"])
}

func (d *Doctor) applyRename(ctx context.Context, data fixData) (string, error) {
	var err error
	if data.App == appSonarr {
		err = d.runCommand(ctx, d.Sonarr, map[string]any{"name": "RenameFiles", "seriesId": data.ArrID, "files": []int64{data.FileID}})
	} else {
		err = d.runCommand(ctx, d.Radarr, map[string]any{"name": "RenameFiles", "movieId": data.ArrID, "files": []int64{data.FileID}})
	}
	if err != nil {
		return "", err
	}
	return "renamed to " + data.NewName, nil
}

func (d *Doctor) applyAdd(ctx context.Context, data fixData, choice string) (string, error) {
	if choice == removeChoice && data.App == appSonarr {
		return d.applyRemoveFolder(ctx, data)
	}
	if _, err := strconv.ParseInt(choice, 10, 64); err != nil {
		return "", errors.New("nothing chosen to add")
	}
	if data.App == appSonarr {
		return d.addSeries(ctx, data, choice)
	}
	return d.addMovie(ctx, data, choice)
}

// mostUsed returns the quality profile most items use, for anything added.
func mostUsed(profiles []int64) (int64, error) {
	counts := map[int64]int{}
	var best int64
	for _, p := range profiles {
		counts[p]++
		if counts[p] > counts[best] {
			best = p
		}
	}
	if best == 0 {
		return 0, errors.New("no quality profile to copy")
	}
	return best, nil
}

func (d *Doctor) addMovie(ctx context.Context, data fixData, tmdb string) (string, error) {
	found, err := d.Radarr.LookupMovies(ctx, "tmdb:"+tmdb)
	if err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", fmt.Errorf("radarr found %d movies for TMDB %s", len(found), tmdb)
	}
	movies, err := d.Radarr.Movies(ctx)
	if err != nil {
		return "", err
	}
	profiles := make([]int64, 0, len(movies))
	for _, m := range movies {
		profiles = append(profiles, m.QualityProfileID)
	}
	profile, err := mostUsed(profiles)
	if err != nil {
		return "", err
	}
	m := found[0]
	delete(m, "id")
	delete(m, "folder")
	m["qualityProfileId"], m["monitored"], m["minimumAvailability"] = profile, false, "released"
	m["rootFolderPath"], m["path"] = data.Root, data.Folder
	m["addOptions"] = map[string]any{"searchForMovie": false, "monitor": "none"}
	id, err := d.Radarr.AddMovie(ctx, m)
	if err != nil {
		return "", err
	}
	for try := 0; ; try++ {
		got, err := d.Radarr.Movie(ctx, id)
		if err == nil && got.HasFile {
			return fmt.Sprintf("added %s unmonitored, with its file", got.Title), nil
		}
		if try == verifyTries {
			return fmt.Sprintf("added %v unmonitored, but Radarr hasn't taken the file; it may need a manual import", m["title"]), nil
		}
		if err := d.sleep(ctx); err != nil {
			return "", err
		}
	}
}

func (d *Doctor) addSeries(ctx context.Context, data fixData, tvdb string) (string, error) {
	found, err := d.Sonarr.LookupSeries(ctx, "tvdb:"+tvdb)
	if err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", fmt.Errorf("sonarr found %d series for TVDB %s", len(found), tvdb)
	}
	series, err := d.Sonarr.Series(ctx)
	if err != nil {
		return "", err
	}
	profiles := make([]int64, 0, len(series))
	for _, sr := range series {
		profiles = append(profiles, sr.QualityProfileID)
	}
	profile, err := mostUsed(profiles)
	if err != nil {
		return "", err
	}
	m := found[0]
	delete(m, "id")
	delete(m, "folder")
	m["qualityProfileId"], m["monitored"], m["seasonFolder"] = profile, false, true
	m["rootFolderPath"], m["path"] = data.Root, data.Folder
	if seasons, ok := m["seasons"].([]any); ok {
		for _, s := range seasons {
			if season, ok := s.(map[string]any); ok {
				season["monitored"] = false
			}
		}
	}
	m["addOptions"] = map[string]any{"monitor": "none", "searchForMissingEpisodes": false, "searchForCutoffUnmetEpisodes": false}
	if _, err := d.Sonarr.AddSeries(ctx, m); err != nil {
		return "", err
	}
	return fmt.Sprintf("added %v unmonitored", m["title"]), nil
}

func (d *Doctor) applyImport(ctx context.Context, data fixData, choice string) (string, error) {
	defs, err := d.Radarr.QualityDefinitions(ctx)
	if err != nil {
		return "", err
	}
	var quality map[string]any
	for _, q := range defs {
		if q["name"] == choice {
			quality = q
		}
	}
	if quality == nil {
		return "", errors.New("no quality chosen")
	}
	files, err := d.Radarr.ImportFiles(ctx, data.ArrID, data.Folder)
	if err != nil {
		return "", err
	}
	f, ok := importable(files)
	if !ok {
		return "", errors.New("radarr no longer sees a video in that folder")
	}
	languages := f.Languages
	if len(languages) == 0 {
		languages = []any{map[string]any{"id": 1, "name": "English"}}
	}
	if err := d.runCommand(ctx, d.Radarr, map[string]any{"name": "ManualImport", "importMode": "auto", "files": []map[string]any{{
		"path": f.Path, "folderName": f.FolderName, "movieId": data.ArrID, "releaseGroup": f.ReleaseGroup, "languages": languages,
		"quality": map[string]any{"quality": quality, "revision": map[string]any{"version": 1, "real": 0, "isRepack": false}},
	}}}); err != nil {
		return "", err
	}
	got, err := d.Radarr.Movie(ctx, data.ArrID)
	if err != nil {
		return "", err
	}
	if !got.HasFile {
		return "", errors.New("radarr ran the import, and still shows no file")
	}
	return fmt.Sprintf("imported %s as %s", path.Base(f.Path), choice), nil
}

func (d *Doctor) applyEmptyFolders(ctx context.Context) (string, error) {
	s, err := d.Radarr.MediaManagementRaw(ctx)
	if err != nil {
		return "", err
	}
	s["deleteEmptyFolders"], s["createEmptyMovieFolders"] = true, false
	if err := d.Radarr.SetMediaManagement(ctx, s); err != nil {
		return "", err
	}
	after, err := d.Radarr.MediaManagement(ctx)
	if err != nil {
		return "", err
	}
	if !after.DeleteEmptyFolders || after.CreateEmptyMovieFolders {
		return "", errors.New("radarr didn't keep the change")
	}
	return "radarr now removes empty folders and doesn't create them", nil
}

func (d *Doctor) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d.FixWait):
		return nil
	}
}

func isArrFix(f storage.Finding) bool {
	return f.Kind == KindNaming || f.Kind == KindUntracked || f.Kind == KindNotAccepted || f.Kind == KindMissing || f.Kind == KindHealth || (f.Kind == KindSettings && !strings.HasPrefix(f.Subject, localPrefix))
}
