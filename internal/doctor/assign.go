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

// AssignEnabled reports whether the no-requester section and assign are on, which
// they are only when Seerr's database is mounted (VGC_SEERR_DB).
func (d *Doctor) AssignEnabled() bool { return d.SeerrDB != nil }

// AssignRequester gives a movie or show nobody requested a requester in Seerr,
// for the user. It only lines Seerr up with what Radarr or Sonarr already has:
// a movie must be on disk or monitored in Radarr, and a show's request names only
// seasons with files or monitored in Sonarr.
//
// A movie goes through Seerr's request API: for a movie already on disk Seerr
// completes the request itself and Radarr is never touched. A show can't: Seerr
// approves it for an auto-approve user, and approving re-monitors the unmonitored
// episodes in it, which brings back what ephemeral deleted, and starts a search.
// So a show's request is written into Seerr's database directly, where Seerr
// never acts on it (see seerr.DB). Seerr has no record to hang that on for a
// title its scans haven't seen, and one that already holds a request is left
// alone; both are refused.
func (d *Doctor) AssignRequester(ctx context.Context, f storage.Finding, userID int64) error {
	if d.SeerrDB == nil {
		return fmt.Errorf("%w: assign is off, VGC_SEERR_DB isn't set", ErrNotAssignable)
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
	var req seerr.Request
	mediaType := "movie"
	if typ == "movie" {
		req, err = d.requestMovie(ctx, tmdb, userID)
	} else {
		mediaType = "tv"
		tmdb, req, err = d.recordShow(ctx, tmdb, tvdb, userID)
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
	if _, err := d.DB.SetFindingState(ctx, f.ID, "resolved"); err != nil {
		return err
	}
	if mediaType == "tv" || req.Status == seerr.StatusApproved || req.Status == seerr.StatusCompleted {
		return nil // recorded in Seerr's database, or approved by the user's own permissions or already there
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

// requestMovie checks Radarr has the movie on disk or monitored, then asks Seerr for the request.
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
	if req.ID == 0 {
		return req, fmt.Errorf("%w: Seerr made no request", ErrNotAssignable)
	}
	return req, nil
}

// recordShow checks Sonarr has the show, then writes the request into Seerr's database.
// It returns the TMDB ID it used, which Sonarr may know when Plex doesn't.
func (d *Doctor) recordShow(ctx context.Context, tmdb, tvdb, userID int64) (int64, seerr.Request, error) {
	tmdb, seasons, err := d.showSeasons(ctx, tmdb, tvdb)
	if err != nil {
		return 0, seerr.Request{}, err
	}
	req, err := d.SeerrDB.RecordRequest(ctx, "tv", tmdb, userID, seasons)
	if errors.Is(err, seerr.ErrNoMedia) || errors.Is(err, seerr.ErrHasRequests) {
		return 0, req, fmt.Errorf("%w: %v", ErrNotAssignable, err)
	}
	return tmdb, req, err
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
