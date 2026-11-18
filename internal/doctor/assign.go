package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// ErrNotAssignable means the item can't be given a requester without making
// something new: Radarr or Sonarr doesn't have it, it has nothing of it on disk
// or monitored, or Seerr can't take the request.
var ErrNotAssignable = errors.New("not assignable")

// RequestRecorder writes a request straight into Seerr, with none of what its
// request API sets off (see seerr.DB).
type RequestRecorder interface {
	RecordRequest(ctx context.Context, mediaType string, tmdbID, userID int64, seasons []seerr.Season) (seerr.Request, error)
}

// assignSeasons picks the seasons a request may cover: those with files in
// Sonarr or monitored there.
func assignSeasons(sr arr.Series) []seerr.Season {
	var out []seerr.Season
	for _, se := range sr.Seasons {
		if se.Monitored || se.Statistics.EpisodeFileCount > 0 {
			out = append(out, seerr.Season{Number: se.SeasonNumber, Available: se.Statistics.EpisodeFileCount > 0})
		}
	}
	return out
}

// AssignRequester records a Seerr request, for the user, on a movie or show
// nobody requested. It only lines Seerr up with what Radarr or Sonarr already
// has: the item must be there, a movie must have its file or be monitored, and a
// show's request names only seasons with files or monitored.
//
// The request goes into Seerr's database directly, so Seerr never acts on it:
// nothing is sent to Radarr or Sonarr, no episode is re-monitored, no search
// runs and no notification goes out. Seerr has no record to hang a request on
// for a title its scans haven't seen, and one that already holds a request is
// left alone; both are refused.
func (d *Doctor) AssignRequester(ctx context.Context, f storage.Finding, userID int64) error {
	if d.SeerrDB == nil {
		return fmt.Errorf("%w: VGC_SEERR_DB isn't set, so requests can't be recorded in Seerr", ErrNotAssignable)
	}
	// Read it again: it may have been assigned or dismissed since the caller looked.
	f, found, err := d.DB.Finding(ctx, f.ID)
	if err != nil {
		return err
	}
	if !found || f.Kind != KindNoRequester || f.State != "open" {
		return fmt.Errorf("%w: not an open no-requester finding", ErrNotAssignable)
	}
	typ, tmdb, tvdb, found, err := d.DB.MediaIDs(ctx, f.Subject)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: it is no longer in the library", ErrNotAssignable)
	}
	mediaType := "tv"
	var seasons []seerr.Season
	if typ == "movie" {
		mediaType = "movie"
		if err := d.checkMovie(ctx, tmdb); err != nil {
			return err
		}
	} else if tmdb, seasons, err = d.showSeasons(ctx, tmdb, tvdb); err != nil {
		return err
	}
	req, err := d.SeerrDB.RecordRequest(ctx, mediaType, tmdb, userID, seasons)
	if errors.Is(err, seerr.ErrNoMedia) || errors.Is(err, seerr.ErrHasRequests) {
		return fmt.Errorf("%w: %v", ErrNotAssignable, err)
	}
	if err != nil {
		return err
	}
	rec := storage.Request{
		SeerrRequestID: req.ID, SeerrMediaID: req.Media.ID, SeerrUserID: userID, MediaType: mediaType,
		TmdbID: tmdb, Status: req.Status, CreatedAt: time.Now().Unix(), RatingKey: sql.NullString{String: f.Subject, Valid: true},
	}
	if req.Media.TvdbID != nil {
		rec.TvdbID = sql.NullInt64{Int64: *req.Media.TvdbID, Valid: true}
	}
	// Seerr has the request now, so the app's copy and the finding follow; a failure here
	// only delays them to the next sync and doctor run.
	if err := d.DB.AddRequest(ctx, rec); err != nil {
		return err
	}
	_, err = d.DB.SetFindingState(ctx, f.ID, "resolved")
	return err
}

// checkMovie checks Radarr has the movie on disk or monitored.
func (d *Doctor) checkMovie(ctx context.Context, tmdb int64) error {
	movies, err := d.Radarr.Movies(ctx)
	if err != nil {
		return fmt.Errorf("reading Radarr: %w", err)
	}
	i := slices.IndexFunc(movies, func(m arr.Movie) bool { return tmdb != 0 && m.TmdbID == tmdb })
	if i < 0 {
		return fmt.Errorf("%w: Radarr doesn't have this movie", ErrNotAssignable)
	}
	if !movies[i].HasFile && !movies[i].Monitored {
		return fmt.Errorf("%w: Radarr has it neither on disk nor monitored", ErrNotAssignable)
	}
	return nil
}

// showSeasons finds the show in Sonarr and returns the TMDB ID to use, which
// Sonarr may know when Plex doesn't, and the seasons a request may cover.
func (d *Doctor) showSeasons(ctx context.Context, tmdb, tvdb int64) (int64, []seerr.Season, error) {
	series, err := d.Sonarr.Series(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("reading Sonarr: %w", err)
	}
	i := slices.IndexFunc(series, func(sr arr.Series) bool {
		return (tvdb != 0 && sr.TvdbID == tvdb) || (tmdb != 0 && sr.TmdbID == tmdb)
	})
	if i < 0 {
		return 0, nil, fmt.Errorf("%w: Sonarr doesn't have this show", ErrNotAssignable)
	}
	if tmdb == 0 {
		tmdb = series[i].TmdbID
	}
	seasons := assignSeasons(series[i])
	if tmdb == 0 || len(seasons) == 0 {
		return 0, nil, fmt.Errorf("%w: Sonarr has no TMDB ID for it, or no season with files or monitored", ErrNotAssignable)
	}
	return tmdb, seasons, nil
}
