package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateAppliesAllStatementsOnce(t *testing.T) {
	db := openTest(t)
	// Every table in 0001 exists, so the multi-statement file ran whole.
	for _, table := range []string{"settings", "users", "sessions"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s: n=%d err=%v", table, n, err)
		}
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestSettingKeepsFirstValue(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, _ := db.Setting(ctx, "k", func() string { return "first" })
	b, err := db.Setting(ctx, "k", func() string { return "second" })
	if err != nil || a != "first" || b != "first" {
		t.Fatalf("got %q %q %v", a, b, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.UpsertUser(ctx, User{SeerrID: 7, Name: "alice", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET auto_approve = 1 WHERE seerr_user_id = 7`); err != nil {
		t.Fatal(err)
	}
	// A refresh from Seerr must not reset the app's own auto-approve flag.
	if err := db.UpsertUser(ctx, User{SeerrID: 7, Name: "alice2"}); err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(ctx, 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.SessionUser(ctx, tok)
	if err != nil || u.Name != "alice2" || u.IsAdmin || !u.AutoApprove {
		t.Fatalf("got %+v %v", u, err)
	}
	if _, err := db.SessionUser(ctx, "bogus"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("bogus token: %v", err)
	}
	expired, _ := db.CreateSession(ctx, 7, -time.Second)
	if _, err := db.SessionUser(ctx, expired); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired token: %v", err)
	}
	if err := db.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionUser(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestLibraryOwnership(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.ReplaceLibrary(ctx,
		[]Section{{Key: "1", Title: "Movies", Type: "movie"}, {Key: "2", Title: "TV", Type: "show"}},
		[]Media{
			{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "b"},
			{RatingKey: "11", SectionKey: "1", Type: "movie", Title: "A"},
			{RatingKey: "20", SectionKey: "2", Type: "show", Title: "s"},
		}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []User{{SeerrID: 1, Name: "ann"}, {SeerrID: 2, Name: "bob"}} {
		if err := db.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rk := sql.NullString{String: "10", Valid: true}
	if err := db.ReplaceRequests(ctx, []Request{
		{SeerrRequestID: 1, SeerrUserID: 2, RatingKey: rk},
		{SeerrRequestID: 2, SeerrUserID: 1, RatingKey: rk},
		{SeerrRequestID: 3, SeerrUserID: 2}, // not in Plex
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPlays(ctx, []Play{{ReferenceID: 1, RatingKey: "10", ShowRatingKey: "10", Stopped: 500}}); err != nil {
		t.Fatal(err)
	}

	bob := User{SeerrID: 2}
	secs, err := db.VisibleSections(ctx, bob)
	if err != nil || len(secs) != 1 || secs[0].Key != "1" {
		t.Fatalf("bob's sections %+v %v", secs, err)
	}
	items, err := db.LibraryItems(ctx, bob, "1", "", "")
	if err != nil || len(items) != 1 || items[0].RatingKey != "10" || items[0].Requesters != "ann, bob" || items[0].LastWatched != 500 {
		t.Fatalf("bob's items %+v %v", items, err)
	}
	if items, _ := db.LibraryItems(ctx, bob, "2", "", ""); len(items) != 0 {
		t.Fatalf("bob sees %d items of a library he requested nothing in", len(items))
	}

	admin := User{SeerrID: 9, IsAdmin: true}
	secs, _ = db.VisibleSections(ctx, admin)
	items, _ = db.LibraryItems(ctx, admin, "1", "", "")
	if len(secs) != 2 || len(items) != 2 || items[0].Title != "A" || items[1].Requesters != "ann, bob" {
		t.Fatalf("admin %+v %+v", secs, items)
	}
}

func TestIgnoredFlagSurvivesReplace(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	movies := []Media{{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "m"}}
	if err := db.ReplaceLibrary(ctx, []Section{{Key: "1", Title: "Old", Type: "movie"}, {Key: "2", Title: "Gone", Type: "show"}}, movies); err != nil {
		t.Fatal(err)
	}
	if found, _ := db.SetSectionIgnored(ctx, "nope", true); found {
		t.Fatal("unknown library reported found")
	}
	if found, err := db.SetSectionIgnored(ctx, "1", true); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	// A sync that read the library before it was ignored still can't bring it back.
	if err := db.ReplaceLibrary(ctx, []Section{{Key: "1", Title: "New", Type: "movie"}}, movies); err != nil {
		t.Fatal(err)
	}
	secs, err := db.Sections(ctx)
	if err != nil || len(secs) != 1 || secs[0].Title != "New" || !secs[0].Ignored {
		t.Fatalf("sections %+v %v", secs, err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM media`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d items of an ignored library", n)
	}
	admin := User{IsAdmin: true}
	if vis, _ := db.VisibleSections(ctx, admin); len(vis) != 0 {
		t.Fatalf("ignored library still visible: %+v", vis)
	}
}

func TestLibrarySearchAndSort(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.ReplaceLibrary(ctx, []Section{{Key: "1", Title: "Movies", Type: "movie"}}, []Media{
		{RatingKey: "1", SectionKey: "1", Type: "movie", Title: "alien", AddedAt: 300},
		{RatingKey: "2", SectionKey: "1", Type: "movie", Title: "Aliens", AddedAt: 100},
		{RatingKey: "3", SectionKey: "1", Type: "movie", Title: "100% Wolf", AddedAt: 200},
		{RatingKey: "4", SectionKey: "1", Type: "movie", Title: "Zoolander", AddedAt: 400},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUser(ctx, User{SeerrID: 2, Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceRequests(ctx, []Request{{SeerrRequestID: 1, SeerrUserID: 2, RatingKey: sql.NullString{String: "2", Valid: true}}}); err != nil {
		t.Fatal(err)
	}
	admin := User{IsAdmin: true}
	for _, c := range []struct {
		name, search, sort string
		u                  User
		want               string
	}{
		{"title", "", "", admin, "3 1 2 4"},
		{"unknown sort is title", "", "x; DROP TABLE media", admin, "3 1 2 4"},
		{"newest", "", "newest", admin, "4 1 3 2"},
		{"oldest", "", "oldest", admin, "2 3 1 4"},
		{"any case, any position", "LIEN", "", admin, "1 2"},
		{"search and sort", "alien", "oldest", admin, "2 1"},
		{"percent is literal", "%", "", admin, "3"},
		{"underscore is literal", "_", "", admin, ""},
		{"non-admin stays limited to their items", "alien", "", User{SeerrID: 2}, "2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			items, err := db.LibraryItems(ctx, c.u, "1", c.search, c.sort)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, it := range items {
				got = append(got, it.RatingKey)
			}
			if s := strings.Join(got, " "); s != c.want {
				t.Fatalf("got %q, want %q", s, c.want)
			}
		})
	}
}
