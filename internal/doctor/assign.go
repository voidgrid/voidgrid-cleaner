package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
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
	if req.Status == seerr.StatusApproved || req.Status == seerr.StatusCompleted {
		return nil // the user's own permissions approved it, or the media was already there
	}
	status, err := d.approve(ctx, req.ID)
	if err != nil {
		return fmt.Errorf("the request was made but Seerr wouldn't approve it, so it waits there: %w", err)
	}
	rec.Status = status
	return d.DB.AddRequest(ctx, rec)
}

// approve approves a request and returns the status it ends with. Seerr
// answers 409 for a request that needs no approval, which happens when the
// media is already available and it completed the request itself; that counts
// as approved once Seerr says so.
func (d *Doctor) approve(ctx context.Context, id int64) (int, error) {
	err := d.Seerr.ApproveRequest(ctx, id)
	if err == nil {
		return seerr.StatusApproved, nil
	}
	if se := (*httpjson.StatusError)(nil); !errors.As(err, &se) || se.Code != http.StatusConflict {
		return 0, err
	}
	r, rerr := d.Seerr.RequestByID(ctx, id)
	if rerr != nil {
		return 0, fmt.Errorf("%w (and reading it back failed: %v)", err, rerr)
	}
	if r.Status != seerr.StatusApproved && r.Status != seerr.StatusCompleted {
		return 0, fmt.Errorf("%w (its status is %d)", err, r.Status)
	}
	return r.Status, nil
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
	return req, noRequestMade(req)
}

// noRequestMade is an error when Seerr answered without creating a request. It
// does that for a show whose requested seasons are all already available: the
// reply is a success with no request in it (ID 0), and nothing is stored.
func noRequestMade(req seerr.Request) error {
	if req.ID != 0 {
		return nil
	}
	return fmt.Errorf("%w: Seerr made no request, because every season asked for is already available in Seerr", ErrNotAssignable)
}

// resetSeerrShow deletes Seerr's record of a show so it can be requested again.
// It only ever deletes a record that holds no requests: the delete takes every
// request on it too, and a request this app doesn't know about must survive.
func (d *Doctor) resetSeerrShow(ctx context.Context, tmdb int64) error {
	id, requests, found, err := d.Seerr.MediaRecord(ctx, "tv", tmdb)
	if err != nil {
		return fmt.Errorf("reading Seerr's record of the show: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: Seerr made no request and has no record of the show to reset", ErrNotAssignable)
	}
	if requests > 0 {
		return fmt.Errorf("%w: Seerr already holds %d request(s) on this show, so its record is left alone", ErrNotAssignable, requests)
	}
	if err := d.Seerr.DeleteMedia(ctx, id); err != nil {
		return fmt.Errorf("resetting Seerr's record of the show: %w", err)
	}
	return nil
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
	if req.ID == 0 {
		// Seerr drops every season it already tracks as available, partial, pending or
		// processing, and then makes no request. Its media delete is what resets that,
		// so the show is requested again from a clean record.
		if err := d.resetSeerrShow(ctx, tmdb); err != nil {
			return req, 0, err
		}
		if req, err = d.Seerr.RequestShow(ctx, tmdb, userID, seasons); err != nil {
			return req, 0, fmt.Errorf("the request was refused by Seerr after resetting its record: %w", err)
		}
	}
	return req, tmdb, noRequestMade(req)
}
