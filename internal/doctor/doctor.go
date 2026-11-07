// Package doctor finds where Plex, Radarr, Sonarr, Seerr and the disk disagree.
//
// A run reads every source once, then pure checks turn that into findings.
// A source that fails skips only the checks that need it, and their old
// findings stay, so an outage never reads as "all clear". Nothing here changes
// anything outside the database.
package doctor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Finding kinds. Labels below are what the screen shows.
const (
	KindPathNotMounted  = "path_not_mounted"
	KindPathNotVerified = "path_not_verified"
	KindPathMismatch    = "path_mismatch"
	KindWrongMatch      = "wrong_match"
	KindQuestionable    = "questionable_match"
	KindReleaseTitle    = "release_title"
	KindUntracked       = "untracked_folder"
	KindNotAccepted     = "file_not_accepted"
	KindNaming          = "naming"
	KindEmptyFolder     = "empty_folder"
	KindStrayFile       = "stray_file"
	KindStaleRequest    = "stale_request"
	KindHealth          = "arr_health"
	KindMissing         = "missing"
	KindSettings        = "settings_drift"
	KindBlocklist       = "blocklist"
	KindWatchDrift      = "watch_drift"
)

// Labels names each kind, in the order the screen lists them.
var Labels = []struct{ Kind, Label string }{
	{KindPathNotMounted, "path not mounted"},
	{KindPathNotVerified, "path not verified"},
	{KindPathMismatch, "path mismatch"},
	{KindWrongMatch, "wrong plex match"},
	{KindQuestionable, "questionable plex match"},
	{KindReleaseTitle, "release name as title"},
	{KindNotAccepted, "file not accepted"},
	{KindUntracked, "untracked folder"},
	{KindHealth, "radarr and sonarr health"},
	{KindStaleRequest, "stale seerr request"},
	{KindMissing, "missing"},
	{KindNaming, "naming"},
	{KindStrayFile, "stray file"},
	{KindEmptyFolder, "empty folder"},
	{KindSettings, "settings"},
	{KindBlocklist, "blocklist"},
	{KindWatchDrift, "plex and tautulli disagree"},
}

const (
	problem = "problem"
	check   = "check"
	note    = "note"
)

// The services a run reads.
type (
	ArrAPI interface {
		RootFolders(context.Context) ([]arr.RootFolder, error)
		DiskSpace(context.Context) ([]arr.DiskSpace, error)
		Health(context.Context) ([]arr.Health, error)
		BlocklistSize(context.Context) (int, error)
		MediaManagement(context.Context) (arr.MediaManagement, error)
		commander
	}
	RadarrAPI interface {
		ArrAPI
		Movies(context.Context) ([]arr.Movie, error)
		RenamePreview(context.Context, []int64) ([]arr.Rename, error)
		Movie(context.Context, int64) (arr.Movie, error)
		LookupMovies(context.Context, string) ([]map[string]any, error)
		AddMovie(context.Context, map[string]any) (int64, error)
		QualityDefinitions(context.Context) ([]map[string]any, error)
		ImportFiles(context.Context, int64, string) ([]arr.ImportFile, error)
		MediaManagementRaw(context.Context) (map[string]any, error)
		SetMediaManagement(context.Context, map[string]any) error
		UnmonitorMovie(context.Context, int64) error
		MonitorMovie(context.Context, int64) error
		RemoveMovie(context.Context, int64) error
	}
	SonarrAPI interface {
		ArrAPI
		Series(context.Context) ([]arr.Series, error)
		RenamePreview(context.Context, int64) ([]arr.Rename, error)
		EpisodeFiles(context.Context, int64) ([]arr.EpisodeFile, error)
		MissingEpisodes(context.Context) ([]arr.MissingEpisode, error)
		OneSeries(context.Context, int64) (arr.Series, error)
		UnmonitorSeries(context.Context, int64) error
		UnmonitorEpisodes(context.Context, []int64) error
		RemoveSeries(context.Context, int64) error
		LookupSeries(context.Context, string) ([]map[string]any, error)
		AddSeries(context.Context, map[string]any) (int64, error)
	}
	PlexAPI interface {
		Sections(context.Context) ([]plex.Section, error)
		Items(context.Context, string) ([]plex.Item, error)
		ShowPath(context.Context, string) (string, error)
		PreferLocalMetadata(context.Context, string) (bool, error)
		Item(context.Context, string) (plex.Item, error)
		Matches(ctx context.Context, ratingKey, title string, year int, agent string) ([]plex.Match, error)
		Match(ctx context.Context, ratingKey, guid, name string) error
		SetPreferLocalMetadata(ctx context.Context, sectionKey string, on bool) error
		RefreshMetadata(context.Context, string) error
		RefreshItem(context.Context, string) error
	}
	SeerrAPI interface {
		Title(context.Context, string, int64) (string, error)
		DeleteMedia(context.Context, int64) error
	}
)

// Doctor runs the checks, one run at a time.
type Doctor struct {
	DB     *storage.DB
	Radarr RadarrAPI
	Sonarr SonarrAPI
	Plex   PlexAPI
	Seerr  SeerrAPI
	// FixWait is the gap between checks that Plex took a match.
	FixWait time.Duration
	// DryRun makes a stray file fix report instead of deleting.
	DryRun bool

	mu sync.Mutex
}

// arrState is what one of Radarr or Sonarr reported.
type arrState struct {
	roots    []arr.RootFolder
	disks    []arr.DiskSpace
	health   []arr.Health
	block    int
	settings arr.MediaManagement
}

func readArr(ctx context.Context, a ArrAPI) (s arrState, err error) {
	if s.roots, err = a.RootFolders(ctx); err != nil {
		return s, err
	}
	if s.disks, err = a.DiskSpace(ctx); err != nil {
		return s, err
	}
	if s.health, err = a.Health(ctx); err != nil {
		return s, err
	}
	if s.block, err = a.BlocklistSize(ctx); err != nil {
		return s, err
	}
	s.settings, err = a.MediaManagement(ctx)
	return s, err
}

type library struct {
	section plex.Section
	ignored bool
	items   []plex.Item
	local   bool // Prefer local metadata is on
}

// snapshot is everything a run read. The OK flags say which parts are usable.
type snapshot struct {
	radarrOK, sonarrOK, plexOK bool
	radarr, sonarr             arrState
	movies                     []arr.Movie
	series                     []arr.Series
	missingEpisodes            []arr.MissingEpisode
	libraries                  []library
	showPaths                  map[string]string // rating key to folder, for shows the IDs didn't match
	paths                      []storage.LibraryPath
}

func (d *Doctor) read(ctx context.Context) snapshot {
	var s snapshot
	var err error
	s.radarr, err = readArr(ctx, d.Radarr)
	if err == nil {
		s.movies, err = d.Radarr.Movies(ctx)
	}
	if s.radarrOK = err == nil; err != nil {
		log.Printf("doctor: radarr: %v", err)
	}
	s.sonarr, err = readArr(ctx, d.Sonarr)
	if err == nil {
		s.series, err = d.Sonarr.Series(ctx)
	}
	if err == nil {
		s.missingEpisodes, err = d.Sonarr.MissingEpisodes(ctx)
	}
	if s.sonarrOK = err == nil; err != nil {
		log.Printf("doctor: sonarr: %v", err)
	}
	if err := d.readPlex(ctx, &s); err != nil {
		log.Printf("doctor: plex: %v", err)
	} else {
		s.plexOK = true
	}
	return s
}

func (d *Doctor) readPlex(ctx context.Context, s *snapshot) error {
	secs, err := d.Plex.Sections(ctx)
	if err != nil {
		return err
	}
	stored, err := d.DB.Sections(ctx)
	if err != nil {
		return err
	}
	ignored := map[string]bool{}
	for _, sec := range stored {
		ignored[sec.Key] = sec.Ignored
	}
	tvdb := map[string]bool{}
	for _, sr := range s.series {
		tvdb[fmt.Sprint(sr.TvdbID)] = true
	}
	s.showPaths = map[string]string{}
	for _, sec := range secs {
		lib := library{section: sec, ignored: ignored[sec.Key]}
		if isVideo(sec) && !lib.ignored {
			if lib.items, err = d.Plex.Items(ctx, sec.Key); err != nil {
				return err
			}
			if lib.local, err = d.Plex.PreferLocalMetadata(ctx, sec.Key); err != nil {
				return err
			}
			for _, it := range lib.items {
				if sec.Type == "show" && !tvdb[it.ExternalID("tvdb")] {
					dir, err := d.Plex.ShowPath(ctx, it.RatingKey)
					if err != nil {
						return err
					}
					s.showPaths[it.RatingKey] = dir
				}
			}
		}
		s.libraries = append(s.libraries, lib)
	}
	return nil
}

func isVideo(sec plex.Section) bool { return sec.Type == "movie" || sec.Type == "show" }

// Run reads everything and saves what it found. It returns false without
// doing anything if a run is already going.
func (d *Doctor) Run(ctx context.Context) bool {
	if !d.mu.TryLock() {
		return false
	}
	defer d.mu.Unlock()
	s := d.read(ctx)
	if err := d.refreshPaths(ctx, &s); err != nil {
		log.Printf("doctor: paths: %v", err)
		return true
	}
	// A check that fails saves nothing: an empty result would resolve its
	// findings, and an error must never read as "all clear".
	tryFind := func(ok bool, kind string, find func() ([]storage.Finding, error)) {
		if !ok {
			return
		}
		fs, err := find()
		if err == nil {
			err = d.DB.SaveFindings(ctx, kind, fs)
		}
		if err != nil {
			log.Printf("doctor: %s: %v", kind, err)
		}
	}
	save := func(ok bool, kind string, find func() []storage.Finding) {
		tryFind(ok, kind, func() ([]storage.Finding, error) { return find(), nil })
	}
	arrs, all := s.radarrOK && s.sonarrOK, s.radarrOK && s.sonarrOK && s.plexOK
	save(all, KindPathNotMounted, s.pathsNotMounted)
	save(all, KindPathNotVerified, s.pathsNotVerified)
	save(all, KindPathMismatch, s.pathMismatches)
	save(all, KindWrongMatch, s.wrongMatches)
	save(all, KindQuestionable, s.questionableMatches)
	save(s.plexOK, KindReleaseTitle, s.releaseTitles)
	save(all, KindNotAccepted, s.notAccepted)
	save(arrs, KindUntracked, s.untracked)
	save(arrs, KindEmptyFolder, s.emptyFolders)
	tryFind(all, KindStrayFile, func() ([]storage.Finding, error) { return d.strayFiles(ctx, &s) })
	save(arrs, KindHealth, s.health)
	save(arrs, KindMissing, s.missing)
	save(all, KindSettings, s.settings)
	save(arrs, KindBlocklist, s.blocklist)
	tryFind(arrs, KindNaming, func() ([]storage.Finding, error) { return d.naming(ctx, &s) })
	tryFind(all, KindStaleRequest, func() ([]storage.Finding, error) { return d.staleRequests(ctx, &s) })
	tryFind(true, KindWatchDrift, func() ([]storage.Finding, error) { return d.watchDrift(ctx) })
	return true
}

// refreshPaths infers every path in use and tests each one. Without all three
// services the list would be partial, so the old one stands.
func (d *Doctor) refreshPaths(ctx context.Context, s *snapshot) error {
	if s.radarrOK && s.sonarrOK && s.plexOK {
		if err := d.DB.SaveLibraryPaths(ctx, s.inferPaths()); err != nil {
			return err
		}
	}
	var err error
	s.paths, err = d.DB.LibraryPaths(ctx)
	return err
}

// systemPaths are mounts inside other containers that aren't media.
var systemPaths = []string{"/config", "/app", "/etc", "/run", "/proc", "/sys", "/dev", "/var", "/usr", "/boot", "/home", "/root"}

func isSystem(p string) bool {
	return p == "/" || slices.ContainsFunc(systemPaths, func(s string) bool { return p == s || strings.HasPrefix(p, s+"/") })
}

func (s *snapshot) inferPaths() []storage.LibraryPath {
	users := map[string]map[string]bool{}
	media := map[string]bool{}
	use := func(p, who string) {
		p = strings.TrimRight(p, "/")
		if p == "" || isSystem(p) {
			return
		}
		if users[p] == nil {
			users[p] = map[string]bool{}
		}
		users[p][who] = true
	}
	for _, lib := range s.libraries {
		for _, l := range lib.section.Location {
			use(l.Path, "plex")
			if isVideo(lib.section) {
				media[strings.TrimRight(l.Path, "/")] = true
			}
		}
	}
	for _, a := range []struct {
		name  string
		state arrState
	}{{"radarr", s.radarr}, {"sonarr", s.sonarr}} {
		for _, r := range a.state.roots {
			use(r.Path, a.name)
			media[strings.TrimRight(r.Path, "/")] = true
		}
		for _, disk := range a.state.disks {
			use(disk.Path, a.name)
		}
	}
	samples := s.sampleFiles()
	out := make([]storage.LibraryPath, 0, len(users))
	for p, who := range users {
		names := make([]string, 0, len(who))
		for n := range who {
			names = append(names, n)
		}
		sort.Strings(names)
		lp := storage.LibraryPath{Path: p, UsedBy: strings.Join(names, ", "), Media: media[p]}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			lp.Visible = true
			lp.Writable = syscall.Access(p, 2) == nil
			lp.Verified = hasFile(p, samples)
		}
		out = append(out, lp)
	}
	return out
}

// sampleFiles lists files other services say exist.
func (s *snapshot) sampleFiles() []string {
	var out []string
	for _, m := range s.movies {
		if m.MovieFile != nil {
			out = append(out, path.Join(m.Path, m.MovieFile.RelativePath))
		}
	}
	for _, lib := range s.libraries {
		for _, it := range lib.items {
			out = append(out, it.Files()...)
		}
	}
	for _, sr := range s.series {
		if sr.Statistics.EpisodeFileCount > 0 {
			out = append(out, sr.Path) // Sonarr's file list costs a call per series; the folder proves the mount
		}
	}
	return out
}

// hasFile proves a path is really the mount other services see: something
// they say is under it must exist here. An empty mountpoint proves nothing.
func hasFile(root string, known []string) bool {
	tried := 0
	for _, f := range known {
		if !strings.HasPrefix(f, root+"/") {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			return true
		}
		if tried++; tried == 20 {
			return false
		}
	}
	return false
}

func hasKnownFile(root string, known []string) bool {
	return slices.ContainsFunc(known, func(f string) bool { return strings.HasPrefix(f, root+"/") })
}

func (s *snapshot) pathsNotMounted() []storage.Finding {
	var out []storage.Finding
	for _, p := range s.paths {
		if !p.Visible {
			out = append(out, storage.Finding{
				Subject: p.Path, Title: p.Path, Severity: problem,
				Detail: p.UsedBy + " uses this path and this app can't see it. Mount it, or dismiss this if it isn't needed here.",
			})
		}
	}
	return out
}

func (s *snapshot) pathsNotVerified() []storage.Finding {
	known := s.sampleFiles()
	var out []storage.Finding
	for _, p := range s.paths {
		if p.Visible && !p.Verified && hasKnownFile(p.Path, known) {
			out = append(out, storage.Finding{
				Subject: p.Path, Title: p.Path, Severity: problem,
				Detail: "The folder is here, but files " + p.UsedBy + " list aren't. It may be an empty mountpoint. Disk checks skip it.",
			})
		}
	}
	return out
}

func (s *snapshot) pathMismatches() []storage.Finding {
	roots := map[string]string{}
	for _, r := range s.radarr.roots {
		roots[strings.TrimRight(r.Path, "/")] = "radarr"
	}
	for _, r := range s.sonarr.roots {
		roots[strings.TrimRight(r.Path, "/")] = "sonarr"
	}
	var out []storage.Finding
	plexPaths := map[string]bool{}
	for _, lib := range s.libraries {
		for _, l := range lib.section.Location {
			p := strings.TrimRight(l.Path, "/")
			plexPaths[p] = true
			if isVideo(lib.section) && !lib.ignored && roots[p] == "" {
				out = append(out, storage.Finding{
					Subject: p, Title: p, Severity: note,
					Detail: "Plex library " + lib.section.Title + " uses this, and neither Radarr nor Sonarr has it as a root folder.",
				})
			}
		}
	}
	for p, who := range roots {
		if !plexPaths[p] {
			out = append(out, storage.Finding{Subject: p, Title: p, Severity: problem, Detail: who + " stores media here, and no Plex library reads it."})
		}
	}
	return out
}

// managedRoots lists paths the doctor may look inside.
func (s *snapshot) managedRoots() []string {
	var out []string
	for _, p := range s.paths {
		if p.Managed && p.Verified {
			out = append(out, p.Path)
		}
	}
	return out
}
