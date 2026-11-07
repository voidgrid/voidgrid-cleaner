// Package sync copies users, the library, requests and watch history from
// Seerr, Plex, Radarr, Sonarr and Tautulli into the database.
//
// Each source is all-or-nothing: if any call for it fails, its tables keep
// the last good copy and the error is recorded for the sync page.
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// The calls the sync needs, so tests can fake them.
type (
	SeerrAPI interface {
		Users(context.Context) ([]seerr.User, error)
		Requests(context.Context) ([]seerr.Request, error)
	}
	PlexAPI interface {
		Sections(context.Context) ([]plex.Section, error)
		Items(context.Context, string) ([]plex.Item, error)
		ShowPath(context.Context, string) (string, error)
		ViewHistory(context.Context) ([]plex.View, error)
		Accounts(context.Context) ([]plex.ServerAccount, error)
	}
	RadarrAPI interface {
		Movies(context.Context) ([]arr.Movie, error)
	}
	SonarrAPI interface {
		Series(context.Context) ([]arr.Series, error)
	}
	TautulliAPI interface {
		History(context.Context, time.Time) ([]tautulli.Play, error)
	}
)

// Source names, as shown on the sync page.
const (
	SrcUsers       = "seerr users"
	SrcLibrary     = "plex library + radarr/sonarr"
	SrcRequests    = "seerr requests"
	SrcHistory     = "tautulli history"
	SrcHistoryFull = "tautulli history (full)"
	SrcPlexHistory = "plex history"
)

// fullHistoryEvery is how often history is re-read whole instead of from
// the newest play, so plays deleted or merged in Tautulli drop out.
const fullHistoryEvery = 24 * time.Hour

// Syncer runs syncs, one at a time.
type Syncer struct {
	DB       *storage.DB
	Seerr    SeerrAPI
	Plex     PlexAPI
	Radarr   RadarrAPI
	Sonarr   SonarrAPI
	Tautulli TautulliAPI
	// Owner returns the server owner's plex.tv ID: Plex's history calls them
	// account 1, and everything else here uses the plex.tv ID.
	Owner func(context.Context) (int64, error)
	// After runs when a sync finishes, if set.
	After func(context.Context)

	mu sync.Mutex
}

// Loop syncs now and then every interval until ctx ends.
func (s *Syncer) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Run(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run syncs every source once. It returns false without doing anything if
// a sync is already running.
func (s *Syncer) Run(ctx context.Context) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	histName, histFn := SrcHistory, s.history
	if last, err := s.DB.LastSyncOK(ctx, SrcHistoryFull); err != nil || time.Since(last) >= fullHistoryEvery {
		histName, histFn = SrcHistoryFull, s.fullHistory
	}
	// Library before requests: requests are matched to Plex items.
	for _, step := range []struct {
		name string
		fn   func(context.Context) (int, error)
	}{
		{SrcUsers, s.users},
		{SrcLibrary, s.library},
		{SrcRequests, s.requests},
		{histName, histFn},
		{SrcPlexHistory, s.plexHistory},
	} {
		n, err := step.fn(ctx)
		if err != nil {
			log.Printf("sync: %s: %v", step.name, err)
		}
		if rerr := s.DB.RecordSync(ctx, step.name, n, err); rerr != nil {
			log.Printf("sync: record %s: %v", step.name, rerr)
		}
	}
	if s.After != nil {
		s.After(ctx)
	}
	return true
}

func (s *Syncer) users(ctx context.Context) (int, error) {
	us, err := s.Seerr.Users(ctx)
	if err != nil {
		return 0, err
	}
	for _, su := range us {
		u := storage.User{SeerrID: su.ID, Name: su.Name(), IsAdmin: su.IsAdmin()}
		if su.PlexID != nil {
			u.PlexID = sql.NullInt64{Int64: *su.PlexID, Valid: true}
		}
		if err := s.DB.UpsertUser(ctx, u); err != nil {
			return 0, err
		}
	}
	return len(us), nil
}

func (s *Syncer) library(ctx context.Context) (int, error) {
	secs, err := s.Plex.Sections(ctx)
	if err != nil {
		return 0, err
	}
	movies, err := s.Radarr.Movies(ctx)
	if err != nil {
		return 0, err
	}
	series, err := s.Sonarr.Series(ctx)
	if err != nil {
		return 0, err
	}
	byTmdb, moviePaths := map[int64]arr.Movie{}, map[string]arr.Movie{}
	for _, m := range movies {
		byTmdb[m.TmdbID] = m
		moviePaths[m.Path] = m
	}
	byTvdb, seriesPaths := map[int64]arr.Series{}, map[string]arr.Series{}
	for _, sr := range series {
		byTvdb[sr.TvdbID] = sr
		seriesPaths[sr.Path] = sr
	}

	stored, err := s.DB.Sections(ctx)
	if err != nil {
		return 0, err
	}
	ignored := map[string]bool{}
	for _, sec := range stored {
		ignored[sec.Key] = sec.Ignored
	}

	var out []storage.Media
	var kept []storage.Section
	for _, sec := range secs {
		if sec.Type != "movie" && sec.Type != "show" {
			continue // only video libraries: music and photos are never handled
		}
		kept = append(kept, storage.Section{Key: sec.Key, Title: sec.Title, Type: sec.Type})
		if ignored[sec.Key] {
			continue
		}
		items, err := s.Plex.Items(ctx, sec.Key)
		if err != nil {
			return 0, err
		}
		for _, it := range items {
			m := storage.Media{
				RatingKey: it.RatingKey, SectionKey: sec.Key, Type: sec.Type, Title: it.Title,
				Year: it.Year, Thumb: it.Thumb, AddedAt: it.AddedAt,
				TmdbID: idOf(it, "tmdb"), TvdbID: idOf(it, "tvdb"),
			}
			if sec.Type == "movie" {
				rm, ok := byTmdb[m.TmdbID.Int64]
				if !ok || !m.TmdbID.Valid {
					// Plex's ID can be wrong, but the file can't. Taking Radarr's
					// ID also keeps the deletion's own lookup on Radarr's path:
					// an item Radarr manages must never be deleted through Plex.
					for _, f := range it.Files() {
						if byFile, found := moviePaths[path.Dir(f)]; found {
							rm, ok = byFile, true
							m.TmdbID = sql.NullInt64{Int64: rm.TmdbID, Valid: true}
						}
					}
				}
				if ok {
					m.ArrID, m.SizeBytes = sql.NullInt64{Int64: rm.ID, Valid: true}, rm.SizeOnDisk
				}
			}
			if sec.Type == "show" {
				ss, ok := byTvdb[m.TvdbID.Int64]
				if !ok || !m.TvdbID.Valid {
					dir, err := s.Plex.ShowPath(ctx, it.RatingKey)
					if err != nil {
						return 0, err
					}
					if ss, ok = seriesPaths[dir]; ok {
						m.TvdbID = sql.NullInt64{Int64: ss.TvdbID, Valid: true}
					}
				}
				if ok {
					m.ArrID, m.SizeBytes = sql.NullInt64{Int64: ss.ID, Valid: true}, ss.Statistics.SizeOnDisk
				}
			}
			out = append(out, m)
		}
	}
	return len(out), s.DB.ReplaceLibrary(ctx, kept, out)
}

func idOf(it plex.Item, agent string) sql.NullInt64 {
	n, err := strconv.ParseInt(it.ExternalID(agent), 10, 64)
	return sql.NullInt64{Int64: n, Valid: err == nil}
}

func (s *Syncer) requests(ctx context.Context) (int, error) {
	rs, err := s.Seerr.Requests(ctx)
	if err != nil {
		return 0, err
	}
	// Match by Plex rating key, else by TMDB (movies) or TVDB (shows) ID.
	keys, movies, shows := map[string]bool{}, map[int64]string{}, map[int64]string{}
	rows, err := s.DB.QueryContext(ctx, `SELECT rating_key, type, tmdb_id, tvdb_id FROM media`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var key, typ string
		var tmdb, tvdb sql.NullInt64
		if err := rows.Scan(&key, &typ, &tmdb, &tvdb); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys[key] = true
		if typ == "movie" && tmdb.Valid {
			movies[tmdb.Int64] = key
		}
		if typ == "show" && tvdb.Valid {
			shows[tvdb.Int64] = key
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	out := make([]storage.Request, 0, len(rs))
	for _, r := range rs {
		sr := storage.Request{
			SeerrRequestID: r.ID, SeerrMediaID: r.Media.ID, SeerrUserID: r.RequestedBy.ID,
			MediaType: r.Media.MediaType, TmdbID: r.Media.TmdbID, Status: r.Status, CreatedAt: r.CreatedAt.Unix(),
		}
		if r.Media.TvdbID != nil {
			sr.TvdbID = sql.NullInt64{Int64: *r.Media.TvdbID, Valid: true}
		}
		key := ""
		switch {
		case r.Media.RatingKey != nil && keys[*r.Media.RatingKey]:
			key = *r.Media.RatingKey
		case r.Media.MediaType == "movie":
			key = movies[r.Media.TmdbID]
		case r.Media.MediaType == "tv" && sr.TvdbID.Valid:
			key = shows[sr.TvdbID.Int64]
		}
		sr.RatingKey = sql.NullString{String: key, Valid: key != ""}
		out = append(out, sr)
	}
	return len(out), s.DB.ReplaceRequests(ctx, out)
}

func (s *Syncer) history(ctx context.Context) (int, error) {
	last, err := s.DB.LastPlayStopped(ctx)
	if err != nil {
		return 0, err
	}
	// Re-read from the day before the newest stored play: Tautulli filters by
	// whole days, and a resumed play can grow after it was first stored.
	after := time.Unix(0, 0)
	if !last.IsZero() {
		after = last.AddDate(0, 0, -1)
	}
	out, err := s.readHistory(ctx, after)
	if err != nil {
		return 0, err
	}
	return len(out), s.DB.UpsertPlays(ctx, out)
}

// fullHistory re-reads every play and replaces the stored ones.
func (s *Syncer) fullHistory(ctx context.Context) (int, error) {
	out, err := s.readHistory(ctx, time.Unix(0, 0))
	if err != nil {
		return 0, err
	}
	return len(out), s.DB.ReplacePlays(ctx, out)
}

// plexHistory copies Plex's own views, which go back further than Tautulli's
// and carry on if Tautulli is down.
func (s *Syncer) plexHistory(ctx context.Context) (int, error) {
	views, err := s.Plex.ViewHistory(ctx)
	if err != nil {
		return 0, err
	}
	accounts, err := s.Plex.Accounts(ctx)
	if err != nil {
		return 0, err
	}
	owner, err := s.Owner(ctx)
	if err != nil {
		return 0, err
	}
	names := map[int64]string{}
	for _, a := range accounts {
		names[a.ID] = a.Name
	}
	out := make([]storage.Play, 0, len(views))
	for _, v := range views {
		id, err := strconv.ParseInt(path.Base(v.HistoryKey), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("history key %q: %w", v.HistoryKey, err)
		}
		show := v.RatingKey
		if v.Type == "episode" {
			show = path.Base(v.GrandparentKey)
		}
		// shortcut: views of removed or re-added items have no keys, so they're skipped; matching by title risks the wrong item, so only if that history matters.
		if (v.Type != "movie" && v.Type != "episode") || v.RatingKey == "" || show == "" || show == "." {
			continue
		}
		user := v.AccountID
		if user == 1 {
			user = owner
		}
		out = append(out, storage.Play{
			ReferenceID: -id, PlexUserID: user, UserName: names[v.AccountID], MediaType: v.Type, RatingKey: v.RatingKey, ShowRatingKey: show,
			Season: v.ParentIndex, Episode: v.Index, Percent: 100, Watched: true, Stopped: v.ViewedAt,
		})
	}
	return len(out), s.DB.ReplacePlexViews(ctx, out)
}

func (s *Syncer) readHistory(ctx context.Context, after time.Time) ([]storage.Play, error) {
	ps, err := s.Tautulli.History(ctx, after)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Play, 0, len(ps))
	for _, p := range ps {
		if p.ReferenceID == 0 {
			return nil, fmt.Errorf("play of rating key %d has no reference_id", p.RatingKey)
		}
		show := p.RatingKey
		if p.MediaType == "episode" {
			show = p.GrandparentRatingKey
		}
		out = append(out, storage.Play{
			ReferenceID: int64(p.ReferenceID), PlexUserID: int64(p.UserID), UserName: p.User, MediaType: p.MediaType,
			RatingKey: itoa(p.RatingKey), ShowRatingKey: itoa(show), Season: int(p.ParentMediaIndex), Episode: int(p.MediaIndex),
			Percent: int(p.PercentComplete), Watched: p.WatchedStatus == 1, Stopped: int64(p.Stopped),
		})
	}
	return out, nil
}

func itoa(n tautulli.Int) string { return strconv.FormatInt(int64(n), 10) }
