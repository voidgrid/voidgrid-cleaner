package seerr

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// seerrSchema is the part of Seerr's schema the recorder uses, as Seerr creates it.
const seerrSchema = `
CREATE TABLE user (id integer PRIMARY KEY AUTOINCREMENT NOT NULL, email varchar);
CREATE TABLE media (id integer PRIMARY KEY AUTOINCREMENT NOT NULL, mediaType varchar NOT NULL, tmdbId integer NOT NULL, tvdbId integer,
	status integer NOT NULL DEFAULT 1);
CREATE TABLE media_request (id integer PRIMARY KEY AUTOINCREMENT NOT NULL, status integer NOT NULL,
	createdAt datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), updatedAt datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP),
	type varchar NOT NULL, mediaId integer, requestedById integer, modifiedById integer, is4k boolean NOT NULL DEFAULT (0),
	serverId integer, profileId integer, rootFolder varchar, languageProfileId integer, tags text,
	isAutoRequest boolean NOT NULL DEFAULT (0), ignoreQuota boolean NOT NULL DEFAULT (0),
	FOREIGN KEY (mediaId) REFERENCES media (id) ON DELETE CASCADE,
	FOREIGN KEY (requestedById) REFERENCES user (id) ON DELETE CASCADE,
	FOREIGN KEY (modifiedById) REFERENCES user (id) ON DELETE CASCADE);
CREATE TABLE season_request (id integer PRIMARY KEY AUTOINCREMENT NOT NULL, seasonNumber integer NOT NULL,
	status integer NOT NULL DEFAULT 1, createdAt datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP),
	updatedAt datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), requestId integer,
	FOREIGN KEY (requestId) REFERENCES media_request (id) ON DELETE CASCADE);
INSERT INTO user (id, email) VALUES (1, 'a'), (2, 'b');
INSERT INTO media (id, mediaType, tmdbId, tvdbId, status) VALUES (10, 'tv', 100, 1000, 5), (11, 'movie', 200, NULL, 5), (12, 'tv', 300, 3000, 5);
INSERT INTO media_request (status, type, mediaId, requestedById) VALUES (5, 'tv', 12, 1);`

func seerrDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.sqlite3")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(seerrSchema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestRecordRequestShow(t *testing.T) {
	d := seerrDB(t)
	ctx := context.Background()
	r, err := d.RecordRequest(ctx, "tv", 100, 2, []Season{{Number: 4, Available: true}, {Number: 5, Available: true}})
	if err != nil || r.Status != StatusCompleted || r.Media.ID != 10 || *r.Media.TvdbID != 1000 {
		t.Fatalf("%+v %v", r, err)
	}
	var user, status, quota int
	var typ string
	if err := d.db.QueryRowContext(ctx, `SELECT requestedById, status, ignoreQuota, type FROM media_request WHERE id = ?`, r.ID).Scan(&user, &status, &quota, &typ); err != nil ||
		user != 2 || status != StatusCompleted || quota != 1 || typ != "tv" {
		t.Fatalf("stored: %d %d %d %s %v", user, status, quota, typ, err)
	}
	var n, notDone int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*), sum(status != 5) FROM season_request WHERE requestId = ?`, r.ID).Scan(&n, &notDone); err != nil || n != 2 || notDone != 0 {
		t.Fatalf("seasons: %d %d %v", n, notDone, err)
	}
}

// A season that isn't there yet leaves the request approved, as Seerr has it until the files land.
func TestRecordRequestSeasonNotThereYet(t *testing.T) {
	d := seerrDB(t)
	r, err := d.RecordRequest(context.Background(), "tv", 100, 1, []Season{{Number: 1, Available: true}, {Number: 2}})
	if err != nil || r.Status != StatusApproved {
		t.Fatalf("%+v %v", r, err)
	}
	var s1, s2 int
	_ = d.db.QueryRow(`SELECT status FROM season_request WHERE requestId = ? AND seasonNumber = 1`, r.ID).Scan(&s1)
	_ = d.db.QueryRow(`SELECT status FROM season_request WHERE requestId = ? AND seasonNumber = 2`, r.ID).Scan(&s2)
	if s1 != StatusCompleted || s2 != StatusApproved {
		t.Fatalf("seasons %d %d", s1, s2)
	}
}

func TestRecordRequestMovie(t *testing.T) {
	d := seerrDB(t)
	r, err := d.RecordRequest(context.Background(), "movie", 200, 2, nil)
	if err != nil || r.Status != StatusCompleted || r.Media.TvdbID != nil {
		t.Fatalf("%+v %v", r, err)
	}
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM season_request WHERE requestId = ?`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a movie has no seasons: %d %v", n, err)
	}
}

func TestRecordRequestRefusals(t *testing.T) {
	d := seerrDB(t)
	ctx := context.Background()
	if _, err := d.RecordRequest(ctx, "tv", 999, 2, nil); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("no record: %v", err)
	}
	if _, err := d.RecordRequest(ctx, "tv", 300, 2, nil); !errors.Is(err, ErrHasRequests) {
		t.Fatalf("already requested: %v", err)
	}
	if _, err := d.RecordRequest(ctx, "tv", 100, 99, []Season{{Number: 1, Available: true}}); err == nil {
		t.Fatal("a user Seerr doesn't have must be refused")
	}
	// A refusal leaves nothing behind, season rows included.
	var req, seasons int
	_ = d.db.QueryRow(`SELECT count(*) FROM media_request`).Scan(&req)
	_ = d.db.QueryRow(`SELECT count(*) FROM season_request`).Scan(&seasons)
	if req != 1 || seasons != 0 {
		t.Fatalf("left behind: %d requests, %d seasons", req, seasons)
	}
}
