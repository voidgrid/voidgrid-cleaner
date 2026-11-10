package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// counterpart is what Radarr or Sonarr has for a Plex item.
type counterpart struct {
	title, imdb, folder string
	id                  string // TMDB for movies, TVDB for shows
	year                int
	alts                []string
}

func movieCounterpart(m arr.Movie) counterpart {
	c := counterpart{title: m.Title, imdb: m.ImdbID, folder: path.Base(m.Path), id: fmt.Sprint(m.TmdbID), year: m.Year, alts: []string{m.OriginalTitle}}
	for _, a := range m.AlternateTitles {
		c.alts = append(c.alts, a.Title)
	}
	return c
}

func seriesCounterpart(sr arr.Series) counterpart {
	c := counterpart{title: sr.Title, imdb: sr.ImdbID, folder: path.Base(sr.Path), id: fmt.Sprint(sr.TvdbID), year: sr.Year}
	for _, a := range sr.AlternateTitles {
		c.alts = append(c.alts, a.Title)
	}
	return c
}

// matched pairs each Plex item in a video library with its arr entry, by file
// path first because that can't be wrong. ok is false for items with none.
func (s *snapshot) forEachItem(fn func(lib library, it plex.Item, c counterpart, ok bool)) {
	moviePaths, movieIDs := map[string]arr.Movie{}, map[string]arr.Movie{}
	for _, m := range s.movies {
		moviePaths[m.Path] = m
		movieIDs[fmt.Sprint(m.TmdbID)] = m
	}
	seriesPaths, seriesIDs := map[string]arr.Series{}, map[string]arr.Series{}
	for _, sr := range s.series {
		seriesPaths[sr.Path] = sr
		seriesIDs[fmt.Sprint(sr.TvdbID)] = sr
	}
	for _, lib := range s.libraries {
		for _, it := range lib.items {
			if lib.section.Type == "movie" {
				var m arr.Movie
				ok := false
				for _, f := range it.Files() {
					if byPath, found := moviePaths[path.Dir(f)]; found {
						m, ok = byPath, true
					}
				}
				if !ok {
					m, ok = movieIDs[it.ExternalID("tmdb")]
				}
				fn(lib, it, movieCounterpart(m), ok)
				continue
			}
			sr, ok := seriesIDs[it.ExternalID("tvdb")]
			if !ok {
				sr, ok = seriesPaths[s.showPaths[it.RatingKey]]
			}
			fn(lib, it, seriesCounterpart(sr), ok)
		}
	}
}

func agentFor(lib library) string {
	if lib.section.Type == "movie" {
		return "tmdb"
	}
	return "tvdb"
}

func (s *snapshot) wrongMatches() []storage.Finding {
	var out []storage.Finding
	s.forEachItem(func(lib library, it plex.Item, c counterpart, ok bool) {
		if !ok || !idsDisagree(lib, it, c) {
			return
		}
		out = append(out, storage.Finding{
			Subject: it.RatingKey, Title: it.Title, Severity: problem, Data: matchData(lib, it, c),
			Detail: fmt.Sprintf("Plex has this file as %q; %s says it's %q (%d).", it.Title, arrName(lib), c.title, c.year),
		})
	})
	return out
}

// idsDisagree compares Plex's IDs with arr's. IMDb settles it when the main ID
// is missing, as with a title TMDb removed.
func idsDisagree(lib library, it plex.Item, c counterpart) bool {
	return it.ExternalID(agentFor(lib)) != c.id && (c.imdb == "" || it.ExternalID("imdb") != c.imdb)
}

func matchData(lib library, it plex.Item, c counterpart) string {
	return encode(fixData{RatingKey: it.RatingKey, Type: lib.section.Type, Agent: agentFor(lib), WantID: c.id, ArrTitle: c.title, ArrYear: c.year})
}

func arrName(lib library) string {
	if lib.section.Type == "movie" {
		return "Radarr"
	}
	return "Sonarr"
}

var (
	releaseName = regexp.MustCompile(`(?i)\.(19|20)\d\d\.|1080p|720p|2160p|BluRay|WEB-?DL|x26[45]|H26[45]`)
	wordJunk    = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	folderYear  = regexp.MustCompile(`\s*\(\d{0,4}\)\s*$`)
	articles    = map[string]bool{"the": true, "a": true, "an": true, "and": true, "of": true}
)

// normalize reduces a title to what matters when comparing two.
func normalize(s string) string {
	var words []string
	for _, w := range strings.Fields(wordJunk.ReplaceAllString(strings.ToLower(s), " ")) {
		if !articles[w] {
			words = append(words, w)
		}
	}
	return strings.Join(words, "")
}

// similarity is 1 for the same title, falling towards 0 as they differ.
func similarity(a, b string) float64 {
	x, y := []rune(normalize(a)), []rune(normalize(b))
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	row := make([]int, len(y)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(x); i++ {
		diag := row[0]
		row[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			diag, row[j] = row[j], min(row[j]+1, row[j-1]+1, diag+cost)
		}
	}
	return 1 - float64(row[len(y)])/float64(max(len(x), len(y)))
}

// titleMatch is how close Plex's names come to any name arr has.
func titleMatch(it plex.Item, c counterpart) float64 {
	best := 0.0
	names := append([]string{c.title, folderYear.ReplaceAllString(c.folder, "")}, c.alts...)
	for _, n := range names {
		best = max(best, similarity(it.Title, n), similarity(it.OriginalTitle, n))
	}
	return best
}

const minTitleMatch = 0.5

// questionableMatches doesn't compare years: by this point the IDs agree, so
// it's the right title and the sources simply date it differently, which no
// fix can change.
func (s *snapshot) questionableMatches() []storage.Finding {
	var out []storage.Finding
	add := func(lib library, it plex.Item, c counterpart, severity, why string) {
		out = append(out, storage.Finding{Subject: it.RatingKey, Title: fmt.Sprintf("%s (%d)", it.Title, it.Year), Severity: severity, Detail: why, Data: matchData(lib, it, c)})
	}
	s.forEachItem(func(lib library, it plex.Item, c counterpart, ok bool) {
		switch {
		case len(it.Guids) == 0 || strings.HasPrefix(it.PlexGUID, "local://"):
			if !ok {
				c = counterpart{title: it.Title, year: it.Year}
			}
			add(lib, it, c, problem, "Plex has no match for this.")
		case !ok:
		case idsDisagree(lib, it, c): // a wrong match, reported as one
		case !releaseName.MatchString(it.Title) && titleMatch(it, c) < minTitleMatch:
			add(lib, it, c, check, fmt.Sprintf("%s calls this %q.", arrName(lib), c.title))
		}
	})
	return out
}

func (s *snapshot) releaseTitles() []storage.Finding {
	var out []storage.Finding
	for _, lib := range s.libraries {
		for _, it := range lib.items {
			if releaseName.MatchString(it.Title) {
				out = append(out, storage.Finding{
					Subject: it.RatingKey, Title: it.Title, Severity: check, Data: encode(fixData{RatingKey: it.RatingKey}),
					Detail: "This title looks like a release name, likely a tag inside the file. A metadata refresh replaces it once the library no longer prefers local metadata.",
				})
			}
		}
	}
	return out
}

func (s *snapshot) notAccepted() []storage.Finding {
	plexFiles, resolutions := map[string]string{}, map[string]string{}
	for _, lib := range s.libraries {
		for _, it := range lib.items {
			for _, f := range it.Files() {
				plexFiles[path.Dir(f)] = f
				if len(it.Media) > 0 {
					resolutions[path.Dir(f)] = it.Media[0].VideoResolution
				}
			}
		}
	}
	var out []storage.Finding
	for _, m := range s.movies {
		if f, ok := plexFiles[m.Path]; ok && !m.HasFile {
			out = append(out, storage.Finding{
				Subject: m.Path, Title: m.Title, Severity: problem, Data: encode(fixData{App: appRadarr, ArrID: m.ID, Folder: m.Path, Resolution: resolutions[m.Path]}),
				Detail: "Plex plays " + path.Base(f) + ", and Radarr shows the movie as missing.",
			})
		}
	}
	return out
}

const videoExts = ".mkv .mp4 .avi .m4v .ts .m2ts .wmv .mov .mpg .mpeg .3gp .iso .vob"

func isVideoFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext != "" && slices.Contains(strings.Fields(videoExts), ext)
}

// folderKind says what's in a folder: "" means it couldn't be read.
func folderKind(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	if len(entries) == 0 || (len(entries) == 1 && entries[0].Name() == leftoverFile) {
		return "empty"
	}
	return "files"
}

func (s *snapshot) inManaged(p string) bool {
	return slices.ContainsFunc(s.managedRoots(), func(root string) bool { return strings.HasPrefix(p, root+"/") })
}

type unmappedFolder struct {
	path, root, app string
}

func (s *snapshot) unmapped() []unmappedFolder {
	var out []unmappedFolder
	for app, st := range map[string]arrState{appRadarr: s.radarr, appSonarr: s.sonarr} {
		for _, r := range st.roots {
			for _, u := range r.UnmappedFolders {
				out = append(out, unmappedFolder{path: u.Path, root: r.Path, app: app})
			}
		}
	}
	return out
}

func (s *snapshot) untracked() []storage.Finding {
	var out []storage.Finding
	for _, u := range s.unmapped() {
		if s.inManaged(u.path) && folderKind(s.local(u.path)) == "empty" {
			continue // reported as an empty folder
		}
		out = append(out, storage.Finding{
			Subject: u.path, Title: path.Base(u.path), Severity: problem, Detail: u.path + " isn't tracked by Radarr or Sonarr.",
			Data: encode(fixData{App: u.app, Folder: u.path, Root: u.root}),
		})
	}
	return out
}

// notManaged finds Plex items Radarr or Sonarr don't have, outside their
// root folders. One inside a root folder is an untracked folder, which can be
// imported; these can't be until they're moved under one.
func (s *snapshot) notManaged() []storage.Finding {
	unmapped := map[string]bool{}
	for _, u := range s.unmapped() {
		unmapped[u.path] = true
	}
	var out []storage.Finding
	s.forEachItem(func(lib library, it plex.Item, _ counterpart, ok bool) {
		if ok {
			return
		}
		folder := s.showPaths[it.RatingKey]
		if files := it.Files(); lib.section.Type == "movie" && len(files) > 0 {
			folder = path.Dir(files[0])
		}
		if unmapped[folder] {
			return
		}
		out = append(out, storage.Finding{
			Subject: it.RatingKey, Title: it.Title, Severity: problem,
			Detail: fmt.Sprintf("Plex has it at %s, which isn't under a %s root folder, so %s can't import it. Move it under one; the doctor then offers the import.", folder, arrName(lib), arrName(lib)),
		})
	})
	return out
}

func (s *snapshot) emptyFolders() []storage.Finding {
	var out []storage.Finding
	for _, u := range s.unmapped() {
		if s.inManaged(u.path) && folderKind(s.local(u.path)) == "empty" {
			out = append(out, storage.Finding{Subject: u.path, Title: path.Base(u.path), Severity: note, Detail: u.path, Data: encode(fixData{App: u.app, Folder: u.path, Root: u.root})})
		}
	}
	return out
}

// strayFiles finds videos in tracked folders that Radarr or Sonarr don't list.
func (d *Doctor) strayFiles(ctx context.Context, s *snapshot) ([]storage.Finding, error) {
	var out []storage.Finding
	for _, sr := range s.series {
		if !s.inManaged(sr.Path) {
			continue
		}
		files, err := d.Sonarr.EpisodeFiles(ctx, sr.ID)
		if err != nil {
			return nil, err
		}
		tracked := map[string]bool{}
		for _, f := range files {
			tracked[f.Path] = true
		}
		// A series folder that can't be read is skipped, like a movie's below.
		here := s.local(sr.Path)
		_ = filepath.WalkDir(here, func(p string, e fs.DirEntry, err error) error {
			p = sr.Path + strings.TrimPrefix(p, here) // back to how Sonarr names it
			if err == nil && !e.IsDir() && isVideoFile(p) && !tracked[p] {
				out = append(out, storage.Finding{
					Subject: p, Title: e.Name(), Severity: check, Detail: "In " + sr.Title + "'s folder, and Sonarr doesn't track it.",
					Data: encode(fixData{App: appSonarr, ArrID: sr.ID, Folder: p}),
				})
			}
			return nil
		})
	}
	for _, m := range s.movies {
		// With no file in Radarr, a video here may be the movie itself, waiting
		// to be imported: that's a file not accepted, never a stray to delete.
		if !s.inManaged(m.Path) || m.MovieFile == nil {
			continue
		}
		entries, err := os.ReadDir(s.local(m.Path))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isVideoFile(e.Name()) || e.Name() == m.MovieFile.RelativePath {
				continue
			}
			p := path.Join(m.Path, e.Name())
			out = append(out, storage.Finding{
				Subject: p, Title: e.Name(), Severity: check, Detail: "In " + m.Title + "'s folder, and Radarr doesn't track it.",
				Data: encode(fixData{App: appRadarr, ArrID: m.ID, Folder: p}),
			})
		}
	}
	return out, nil
}

var removedID = regexp.MustCompile(`tmdbid (\d+)`)

func (s *snapshot) health() []storage.Finding {
	movies := map[string]arr.Movie{}
	for _, m := range s.movies {
		movies[fmt.Sprint(m.TmdbID)] = m
	}
	var out []storage.Finding
	// One finding per check, holding everything it says now. A dismissed one
	// opens again when that changes: a second indexer failing is news.
	byCheck := map[string]int{}
	said := map[string][]string{}
	for name, st := range map[string]arrState{appRadarr: s.radarr, appSonarr: s.sonarr} {
		for _, h := range st.health {
			severity := note
			if h.Type == "error" {
				severity = problem
			}
			// One warning can name several movies, and each needs its own decision.
			if h.Source == "RemovedMovieCheck" {
				for _, id := range removedID.FindAllStringSubmatch(h.Message, -1) {
					if m, ok := movies[id[1]]; ok {
						out = append(out, storage.Finding{
							Subject: removedPrefix + id[1], Title: fmt.Sprintf("%s (%d)", m.Title, m.Year), Severity: severity,
							Detail: "TMDb removed this movie, so Radarr warns about it.", Data: encode(fixData{App: appRadarr, ArrID: m.ID, HasFile: m.HasFile}),
						})
					}
				}
				continue
			}
			detail := h.Message
			if h.WikiURL != "" {
				detail += " See " + h.WikiURL
			}
			subject := name + ":" + h.Source
			said[subject] = append(said[subject], detail)
			if i, seen := byCheck[subject]; seen {
				if severity == problem {
					out[i].Severity = problem
				}
				continue
			}
			byCheck[subject] = len(out)
			out = append(out, storage.Finding{Subject: subject, Title: name + ": " + h.Source, Severity: severity, ReopenOnChange: true})
		}
	}
	for subject, i := range byCheck {
		slices.Sort(said[subject]) // the order they arrive in mustn't count as a change
		out[i].Detail = strings.Join(said[subject], " ")
	}
	return out
}

func (s *snapshot) missing() []storage.Finding {
	var out []storage.Finding
	for _, m := range s.movies {
		if m.HasFile || !m.IsAvailable {
			continue
		}
		detail := "Released, monitored and has no file."
		if !m.Monitored {
			detail = "Released, unmonitored and has no file, so nothing will fetch it."
		}
		out = append(out, storage.Finding{
			Subject: fmt.Sprintf("movie:%d", m.TmdbID), Title: fmt.Sprintf("%s (%d)", m.Title, m.Year), Severity: note, Detail: detail,
			Data: encode(fixData{App: appRadarr, ArrID: m.ID, Monitored: m.Monitored}),
		})
	}
	empty := map[int64]bool{}
	for _, sr := range s.series {
		if sr.Statistics.EpisodeFileCount == 0 {
			empty[sr.ID] = true
			out = append(out, storage.Finding{
				Subject: fmt.Sprintf("series:%d", sr.TvdbID), Title: sr.Title, Severity: note, Detail: "In Sonarr with no files.",
				Data: encode(fixData{App: appSonarr, ArrID: sr.ID}),
			})
		}
	}
	// A series with no files at all is already listed, so its episodes aren't.
	bySeries := map[int64][]arr.MissingEpisode{}
	for _, e := range s.missingEpisodes {
		if !empty[e.SeriesID] {
			bySeries[e.SeriesID] = append(bySeries[e.SeriesID], e)
		}
	}
	for id, eps := range bySeries {
		var ids []int64
		var names []string
		for _, e := range eps {
			ids = append(ids, e.ID)
			names = append(names, fmt.Sprintf("S%02dE%02d %s", e.SeasonNumber, e.EpisodeNumber, e.Title))
		}
		list := episodeList(names)
		out = append(out, storage.Finding{
			Subject: fmt.Sprintf("episodes:%d", id), Title: fmt.Sprintf("%s: %d missing", eps[0].Series.Title, len(eps)), Severity: note,
			Detail: list + ". Monitored and aired, with no file.", Data: encode(fixData{App: appSonarr, ArrID: id, Episodes: ids, OldName: list}),
		})
	}
	return out
}

func (s *snapshot) settings() []storage.Finding {
	var out []storage.Finding
	for name, st := range map[string]arrState{"radarr": s.radarr, "sonarr": s.sonarr} {
		if st.settings.RecycleBin == "" {
			out = append(out, storage.Finding{Subject: name + ":recycle", Title: name + ": no recycle bin", Severity: check, Detail: "Files it deletes are gone for good."})
		}
	}
	if st := s.radarr.settings; st.CreateEmptyMovieFolders && !st.DeleteEmptyFolders {
		out = append(out, storage.Finding{
			Subject: emptyFoldersSubject, Title: "radarr: leaves empty folders", Severity: check,
			Detail: "It creates a folder when a movie is added and doesn't remove empty ones. That may be where empty folders come from.",
		})
	}
	for _, lib := range s.libraries {
		if lib.local {
			out = append(out, storage.Finding{
				Subject: localPrefix + lib.section.Key, Title: "plex: " + lib.section.Title + " prefers local metadata", Severity: check,
				Data:   encode(fixData{Section: lib.section.Key, Library: lib.section.Title}),
				Detail: "Tags inside files win over Plex's data, which is how release names become titles. Off by default.",
			})
		}
	}
	return out
}

const renameBatch = 80

func (d *Doctor) naming(ctx context.Context, s *snapshot) ([]storage.Finding, error) {
	var renames []arr.Rename
	var ids []int64
	for _, m := range s.movies {
		if m.HasFile {
			ids = append(ids, m.ID)
		}
	}
	for chunk := range slices.Chunk(ids, renameBatch) {
		r, err := d.Radarr.RenamePreview(ctx, chunk)
		if err != nil {
			return nil, err
		}
		renames = append(renames, r...)
	}
	for _, sr := range s.series {
		if sr.Statistics.EpisodeFileCount == 0 {
			continue
		}
		r, err := d.Sonarr.RenamePreview(ctx, sr.ID)
		if err != nil {
			return nil, err
		}
		renames = append(renames, r...)
	}
	out := make([]storage.Finding, 0, len(renames))
	for _, r := range renames {
		data := fixData{App: appRadarr, ArrID: r.MovieID, FileID: r.MovieFileID, OldName: path.Base(r.ExistingPath), NewName: path.Base(r.NewPath)}
		if r.SeriesID != 0 {
			data.App, data.ArrID, data.FileID = appSonarr, r.SeriesID, r.EpisodeFileID
		}
		out = append(out, storage.Finding{Subject: fmt.Sprintf("%s:%d", data.App, data.FileID), Title: data.OldName, Severity: note, Detail: "Would become " + data.NewName, Data: encode(data)})
	}
	return out, nil
}

func (d *Doctor) staleRequests(ctx context.Context, s *snapshot) ([]storage.Finding, error) {
	movies, series := map[int64]bool{}, map[int64]bool{}
	seriesTmdb := map[int64]bool{}
	for _, m := range s.movies {
		movies[m.TmdbID] = true
	}
	for _, sr := range s.series {
		series[sr.TvdbID] = true
		seriesTmdb[sr.TmdbID] = true
	}
	reqs, err := d.DB.UnmatchedRequests(ctx)
	if err != nil {
		return nil, err
	}
	var out []storage.Finding
	for _, r := range reqs {
		if (r.Type == "movie" && movies[r.TmdbID]) || (r.Type == "tv" && (series[r.TvdbID.Int64] || seriesTmdb[r.TmdbID])) {
			continue
		}
		detail := "Requested in Seerr"
		if r.Requesters != "" {
			detail += " by " + strings.ReplaceAll(r.Requesters, ",", ", ")
		}
		if r.RequestedAt != 0 {
			detail += " on " + time.Unix(r.RequestedAt, 0).UTC().Format("2006-01-02")
		}
		detail += ", and nothing has it."
		title, err := d.Seerr.Title(ctx, r.Type, r.TmdbID)
		// Seerr answers 500 for an ID TMDb dropped. Anything that isn't an answer
		// means Seerr couldn't be asked, so the check saves nothing this run.
		if se := (*httpjson.StatusError)(nil); err != nil && !errors.As(err, &se) {
			return nil, err
		}
		if err != nil || title == "" {
			// Seerr stores only the ID, so there's no name left to show.
			title = fmt.Sprintf("unknown %s (TMDB %d)", r.Type, r.TmdbID)
			detail += " Seerr can't name it, so TMDb probably no longer has this ID."
		}
		out = append(out, storage.Finding{Subject: fmt.Sprint(r.MediaID), Title: title, Severity: note, Detail: detail, Data: encode(fixData{MediaID: r.MediaID})})
	}
	return out, nil
}

// watchDrift flags the two play histories drifting apart by more than a
// little, which they do normally because they count a view differently.
func (d *Doctor) watchDrift(ctx context.Context) ([]storage.Finding, error) {
	p, t, err := d.DB.WatchCounts(ctx)
	if err != nil || p == 0 || t == 0 {
		return nil, err
	}
	if diff := float64(max(p, t)-min(p, t)) / float64(max(p, t)); diff > 0.05 {
		return []storage.Finding{{
			Subject: "plays", Title: "play counts differ by more than 5%", Severity: check,
			Detail: fmt.Sprintf("Since Tautulli began: Plex has %d views, Tautulli has %d watched.", p, t),
		}}, nil
	}
	return nil, nil
}
