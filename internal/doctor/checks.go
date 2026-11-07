package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
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
			Subject: it.RatingKey, Title: it.Title, Severity: problem,
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

func (s *snapshot) questionableMatches() []storage.Finding {
	var out []storage.Finding
	add := func(it plex.Item, severity, why string) {
		out = append(out, storage.Finding{Subject: it.RatingKey, Title: fmt.Sprintf("%s (%d)", it.Title, it.Year), Severity: severity, Detail: why})
	}
	s.forEachItem(func(lib library, it plex.Item, c counterpart, ok bool) {
		switch {
		case len(it.Guids) == 0 || strings.HasPrefix(it.PlexGUID, "local://"):
			add(it, problem, "Plex has no match for this.")
		case !ok:
		case idsDisagree(lib, it, c): // a wrong match, reported as one
		case !releaseName.MatchString(it.Title) && titleMatch(it, c) < minTitleMatch:
			add(it, check, fmt.Sprintf("%s calls this %q.", arrName(lib), c.title))
		case c.year != 0 && it.Year != 0 && (it.Year-c.year > 1 || c.year-it.Year > 1):
			add(it, check, fmt.Sprintf("Plex says %d; %s says %d.", it.Year, arrName(lib), c.year))
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
					Subject: it.RatingKey, Title: it.Title, Severity: check,
					Detail: "This title looks like a release name, likely a tag inside the file.",
				})
			}
		}
	}
	return out
}

func (s *snapshot) notAccepted() []storage.Finding {
	plexFiles := map[string]string{}
	for _, lib := range s.libraries {
		for _, it := range lib.items {
			for _, f := range it.Files() {
				plexFiles[path.Dir(f)] = f
			}
		}
	}
	var out []storage.Finding
	for _, m := range s.movies {
		if f, ok := plexFiles[m.Path]; ok && !m.HasFile {
			out = append(out, storage.Finding{
				Subject: m.Path, Title: m.Title, Severity: problem,
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
	if len(entries) == 0 {
		return "empty"
	}
	return "files"
}

func (s *snapshot) inManaged(p string) bool {
	return slices.ContainsFunc(s.managedRoots(), func(root string) bool { return strings.HasPrefix(p, root+"/") })
}

func (s *snapshot) unmapped() []string {
	var out []string
	for _, st := range []arrState{s.radarr, s.sonarr} {
		for _, r := range st.roots {
			for _, u := range r.UnmappedFolders {
				out = append(out, u.Path)
			}
		}
	}
	return out
}

func (s *snapshot) untracked() []storage.Finding {
	var out []storage.Finding
	for _, p := range s.unmapped() {
		if s.inManaged(p) && folderKind(p) == "empty" {
			continue // reported as an empty folder
		}
		out = append(out, storage.Finding{Subject: p, Title: path.Base(p), Severity: problem, Detail: p + " isn't tracked by Radarr or Sonarr."})
	}
	return out
}

func (s *snapshot) emptyFolders() []storage.Finding {
	var out []storage.Finding
	for _, p := range s.unmapped() {
		if s.inManaged(p) && folderKind(p) == "empty" {
			out = append(out, storage.Finding{Subject: p, Title: path.Base(p), Severity: note, Detail: p})
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
		_ = filepath.WalkDir(sr.Path, func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && isVideoFile(p) && !tracked[p] {
				out = append(out, storage.Finding{Subject: p, Title: e.Name(), Severity: check, Detail: "In " + sr.Title + "'s folder, and Sonarr doesn't track it."})
			}
			return nil
		})
	}
	for _, m := range s.movies {
		if !s.inManaged(m.Path) {
			continue
		}
		entries, err := os.ReadDir(m.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isVideoFile(e.Name()) || (m.MovieFile != nil && e.Name() == m.MovieFile.RelativePath) {
				continue
			}
			p := path.Join(m.Path, e.Name())
			out = append(out, storage.Finding{Subject: p, Title: e.Name(), Severity: check, Detail: "In " + m.Title + "'s folder, and Radarr doesn't track it."})
		}
	}
	return out, nil
}

func (s *snapshot) health() []storage.Finding {
	var out []storage.Finding
	for name, st := range map[string]arrState{"radarr": s.radarr, "sonarr": s.sonarr} {
		for _, h := range st.health {
			severity := note
			if h.Type == "error" {
				severity = problem
			}
			out = append(out, storage.Finding{Subject: name + ":" + h.Source + ":" + h.Message, Title: name + ": " + h.Source, Severity: severity, Detail: h.Message})
		}
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
		out = append(out, storage.Finding{Subject: fmt.Sprintf("movie:%d", m.TmdbID), Title: fmt.Sprintf("%s (%d)", m.Title, m.Year), Severity: note, Detail: detail})
	}
	for _, sr := range s.series {
		if sr.Statistics.EpisodeFileCount == 0 {
			out = append(out, storage.Finding{Subject: fmt.Sprintf("series:%d", sr.TvdbID), Title: sr.Title, Severity: note, Detail: "In Sonarr with no files."})
		}
	}
	if n := s.sonarr.missing; n > 0 {
		out = append(out, storage.Finding{Subject: "episodes", Title: fmt.Sprintf("%d missing episodes", n), Severity: note, Detail: "Monitored and aired, with no file. Series with no files at all are listed on their own."})
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
			Subject: "radarr:empty", Title: "radarr: leaves empty folders", Severity: check,
			Detail: "It creates a folder when a movie is added and doesn't remove empty ones. That may be where empty folders come from.",
		})
	}
	for _, lib := range s.libraries {
		if lib.local {
			out = append(out, storage.Finding{
				Subject: "plex:local:" + lib.section.Key, Title: "plex: " + lib.section.Title + " prefers local metadata", Severity: check,
				Detail: "Tags inside files win over Plex's data, which is how release names become titles. Off by default.",
			})
		}
	}
	return out
}

func (s *snapshot) blocklist() []storage.Finding {
	var out []storage.Finding
	for name, st := range map[string]arrState{"radarr": s.radarr, "sonarr": s.sonarr} {
		if st.block > 0 {
			out = append(out, storage.Finding{Subject: name, Title: fmt.Sprintf("%s: %d blocked releases", name, st.block), Severity: note, Detail: "Blocked releases are never grabbed again."})
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
		out = append(out, storage.Finding{Subject: r.ExistingPath, Title: path.Base(r.ExistingPath), Severity: note, Detail: "Would become " + path.Base(r.NewPath)})
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
		title, err := d.Seerr.Title(ctx, r.Type, r.TmdbID)
		if err != nil || title == "" {
			title = fmt.Sprintf("%s %d", r.Type, r.TmdbID) // TMDb may have dropped it
		}
		out = append(out, storage.Finding{Subject: fmt.Sprint(r.MediaID), Title: title, Severity: note, Detail: "Requested in Seerr, and nothing has it."})
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
