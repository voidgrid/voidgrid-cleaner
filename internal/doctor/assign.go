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

// ErrNotAssignable means the show can't be requested without making something
// new: Sonarr doesn't have it, or has nothing of it on disk or monitored.
var ErrNotAssignable = errors.New("not assignable")

// assignSeasons picks the seasons a request may cover: those with files in
// Sonarr or monitored there, so approval changes nothing that isn't already
// wanted or present.
func assignSeasons(sr arr.Series) []int {
	var out []int
	for _, se := range sr.Seasons {
		if se.Monitored || se.Statistics.EpisodeFileCount > 0 {
			out = append(out, se.SeasonNumber)
		}
	}
	return out
}

// AssignRequester files and approves a Seerr request, for the user, on a
// movie or show nobody requested. It only ever lines Seerr up with what Radarr
// or Sonarr already has: the item must be there, a movie must have its file or
// be monitored, and a show's request names only seasons with files or
// monitored. It adds nothing to Radarr or Sonarr and never makes a request for
// something that isn't there.
//
// The request is saved and the finding resolved before the approval, so a
// failed approval leaves a pending request in Seerr and says so.
func (d *Doctor) AssignRequester(ctx context.Context, f storage.Finding, userID int64) error {
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
	var req seerr.Request
	mediaType := "tv"
	if typ == "movie" {
		mediaType = "movie"
		if req, err = d.requestMovie(ctx, tmdb, userID); err != nil {
			return err
		}
	} else {
		if req, tmdb, err = d.requestShow(ctx, tmdb, tvdb, userID); err != nil {
			return err
		}
	}
	rec := storage.Request{
		SeerrRequestID: req.ID, SeerrMediaID: req.Media.ID, SeerrUserID: userID, MediaType: mediaType,
		TmdbID: tmdb, Status: req.Status, CreatedAt: time.Now().Unix(), RatingKey: sql.NullString{String: f.Subject, Valid: true},
	}
	if req.Media.TvdbID != nil {
		rec.TvdbID = sql.NullInt64{Int64: *req.Media.TvdbID, Valid: true}
	}
	if err := d.DB.AddRequest(ctx, rec); err != nil {
		return err
	}
	if _, err := d.DB.SetFindingState(ctx, f.ID, "resolved"); err != nil {
		return err
	}
	if req.Status == seerr.StatusApproved {
		return nil // the user's own permissions approved it
	}
	if err := d.Seerr.ApproveRequest(ctx, req.ID); err != nil {
		return fmt.Errorf("the request was made but Seerr wouldn't approve it, so it waits there: %w", err)
	}
	rec.Status = seerr.StatusApproved
	return d.DB.AddRequest(ctx, rec)
}

func (d *Doctor) requestMovie(ctx context.Context, tmdb, userID int64) (seerr.Request, error) {
	movies, err := d.Radarr.Movies(ctx)
	if err != nil {
		return seerr.Request{}, fmt.Errorf("reading Radarr: %w", err)
	}
	i := slices.IndexFunc(movies, func(m arr.Movie) bool { return tmdb != 0 && m.TmdbID == tmdb })
	if i < 0 {
		return seerr.Request{}, fmt.Errorf("%w: Radarr doesn't have this movie", ErrNotAssignable)
	}
	if !movies[i].HasFile && !movies[i].Monitored {
		return seerr.Request{}, fmt.Errorf("%w: Radarr has it neither on disk nor monitored", ErrNotAssignable)
	}
	req, err := d.Seerr.RequestMovie(ctx, tmdb, userID)
	if err != nil {
		return req, fmt.Errorf("the request was refused by Seerr: %w", err)
	}
	return req, nil
}

// requestShow also returns the TMDB ID it used, which Sonarr may know when Plex doesn't.
func (d *Doctor) requestShow(ctx context.Context, tmdb, tvdb, userID int64) (seerr.Request, int64, error) {
	series, err := d.Sonarr.Series(ctx)
	if err != nil {
		return seerr.Request{}, 0, fmt.Errorf("reading Sonarr: %w", err)
	}
	i := slices.IndexFunc(series, func(sr arr.Series) bool {
		return (tvdb != 0 && sr.TvdbID == tvdb) || (tmdb != 0 && sr.TmdbID == tmdb)
	})
	if i < 0 {
		return seerr.Request{}, 0, fmt.Errorf("%w: Sonarr doesn't have this show", ErrNotAssignable)
	}
	if tmdb == 0 {
		tmdb = series[i].TmdbID
	}
	seasons := assignSeasons(series[i])
	if tmdb == 0 || len(seasons) == 0 {
		return seerr.Request{}, 0, fmt.Errorf("%w: Sonarr has no TMDB ID for it, or no season with files or monitored", ErrNotAssignable)
	}
	req, err := d.Seerr.RequestShow(ctx, tmdb, userID, seasons)
	if err != nil {
		return req, 0, fmt.Errorf("the request was refused by Seerr: %w", err)
	}
	return req, tmdb, nil
}
