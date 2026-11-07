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
	items, err := db.LibraryItems(ctx, bob, "1", LibraryFilter{})
	if err != nil || len(items) != 1 || items[0].RatingKey != "10" || items[0].Requesters != "ann, bob" || items[0].LastWatched != 500 {
		t.Fatalf("bob's items %+v %v", items, err)
	}
	if items, _ := db.LibraryItems(ctx, bob, "2", LibraryFilter{}); len(items) != 0 {
		t.Fatalf("bob sees %d items of a library he requested nothing in", len(items))
	}

	admin := User{SeerrID: 9, IsAdmin: true}
	secs, _ = db.VisibleSections(ctx, admin)
	items, _ = db.LibraryItems(ctx, admin, "1", LibraryFilter{})
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
			items, err := db.LibraryItems(ctx, c.u, "1", LibraryFilter{Search: c.search, Sort: c.sort})
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

func TestLibraryFiltersAndStatus(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	day := int64(86400)
	now := time.Now().Unix()
	movies := []Media{
		{RatingKey: "1", SectionKey: "1", Type: "movie", Title: "a old, requester watched", AddedAt: now - 100*day},
		{RatingKey: "2", SectionKey: "1", Type: "movie", Title: "b old, someone else watched lately", AddedAt: now - 100*day},
		{RatingKey: "3", SectionKey: "1", Type: "movie", Title: "c old, never watched", AddedAt: now - 100*day},
		{RatingKey: "4", SectionKey: "1", Type: "movie", Title: "d new", AddedAt: now - day},
	}
	secs := []Section{{Key: "1", Title: "Movies", Type: "movie"}}
	if err := db.ReplaceLibrary(ctx, secs, movies); err != nil {
		t.Fatal(err)
	}
	u := User{SeerrID: 2, Name: "bob", PlexID: sql.NullInt64{Int64: 500, Valid: true}}
	if err := db.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	rk := func(k string) sql.NullString { return sql.NullString{String: k, Valid: true} }
	if err := db.ReplaceRequests(ctx, []Request{
		{SeerrRequestID: 1, SeerrUserID: 2, RatingKey: rk("1")},
		{SeerrRequestID: 2, SeerrUserID: 2, RatingKey: rk("2")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPlays(ctx, []Play{
		{ReferenceID: 1, PlexUserID: 500, RatingKey: "1", ShowRatingKey: "1", Watched: true, Stopped: now - 50*day},
		{ReferenceID: 2, PlexUserID: 999, RatingKey: "2", ShowRatingKey: "2", Watched: true, Stopped: now - day},
		{ReferenceID: 3, PlexUserID: 500, RatingKey: "2", ShowRatingKey: "2", Stopped: now - 2*day}, // started, not finished
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetItemStatus(ctx, "3", StatusKeep, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetItemStatus(ctx, "4", StatusSpace, 1); err != nil {
		t.Fatal(err)
	}
	admin := User{IsAdmin: true}
	for name, c := range map[string]struct {
		f    LibraryFilter
		want string
	}{
		"added":                {LibraryFilter{AddedDays: 30}, "1 2 3"},
		"unwatched":            {LibraryFilter{UnwatchedDays: 30}, "1 3 4"},
		"requester watched":    {LibraryFilter{RequesterWatched: true}, "1"},
		"status":               {LibraryFilter{Status: StatusKeep}, "3"},
		"normal":               {LibraryFilter{Status: StatusNormal}, "1 2"},
		"unknown status":       {LibraryFilter{Status: "x"}, "1 2 3 4"},
		"filters combine":      {LibraryFilter{AddedDays: 30, UnwatchedDays: 30}, "1 3"},
		"all of them combined": {LibraryFilter{AddedDays: 30, UnwatchedDays: 30, RequesterWatched: true, Status: StatusNormal}, "1"},
	} {
		t.Run(name, func(t *testing.T) {
			items, err := db.LibraryItems(ctx, admin, "1", c.f)
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

	// A sync rebuilds media; the decision stays.
	if err := db.ReplaceLibrary(ctx, secs, movies); err != nil {
		t.Fatal(err)
	}
	items, _ := db.LibraryItems(ctx, admin, "1", LibraryFilter{})
	if items[2].Status != StatusKeep || items[0].Status != StatusNormal {
		t.Fatalf("statuses after a sync: %q %q", items[2].Status, items[0].Status)
	}
	if err := db.SetItemStatus(ctx, "3", StatusEphemeral, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetItemStatus(ctx, "4", StatusNormal, 1); err != nil {
		t.Fatal(err)
	}
	items, _ = db.LibraryItems(ctx, admin, "1", LibraryFilter{})
	if items[2].Status != StatusEphemeral || items[3].Status != StatusNormal {
		t.Fatalf("changed statuses: %q %q", items[2].Status, items[3].Status)
	}
}

func TestPlexAccountLinks(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	secs := []Section{{Key: "1", Title: "Movies", Type: "movie"}}
	movies := []Media{{RatingKey: "1", SectionKey: "1", Type: "movie", Title: "a"}}
	if err := db.ReplaceLibrary(ctx, secs, movies); err != nil {
		t.Fatal(err)
	}
	for _, u := range []User{
		{SeerrID: 2, Name: "local"},
		{SeerrID: 3, Name: "linked", PlexID: sql.NullInt64{Int64: 700, Valid: true}},
	} {
		if err := db.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rk := sql.NullString{String: "1", Valid: true}
	if err := db.ReplaceRequests(ctx, []Request{{SeerrRequestID: 1, SeerrUserID: 2, RatingKey: rk}}); err != nil {
		t.Fatal(err)
	}
	plays := []Play{
		{ReferenceID: 1, PlexUserID: 500, UserName: "old name", RatingKey: "1", ShowRatingKey: "1", Watched: true, Stopped: 100},
		{ReferenceID: 2, PlexUserID: 500, UserName: "new name", RatingKey: "1", ShowRatingKey: "1", Stopped: 200},
		{ReferenceID: 3, PlexUserID: 700, UserName: "linked", RatingKey: "1", ShowRatingKey: "1", Stopped: 200},
	}
	if err := db.ReplacePlays(ctx, plays); err != nil {
		t.Fatal(err)
	}
	admin := User{IsAdmin: true}
	watched := func() int {
		t.Helper()
		items, err := db.LibraryItems(ctx, admin, "1", LibraryFilter{RequesterWatched: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	unlinked := func() int {
		t.Helper()
		counts, _, err := db.SyncReport(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		return counts["Tautulli user not linked to a Seerr user"]
	}
	if watched() != 0 || unlinked() != 1 {
		t.Fatalf("before link: watched %d, unlinked %d", watched(), unlinked())
	}
	as, err := db.PlexAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].PlexUserID != 500 || as[0].Name != "new name" || as[0].Plays != 2 || as[0].LastPlayed != 200 || as[0].SeerrID != 0 {
		t.Fatalf("accounts: %+v", as)
	}

	for name, ids := range map[string][2]int64{"unknown user": {500, 99}, "unknown account": {999, 2}} {
		if found, err := db.SetPlexAccount(ctx, ids[0], ids[1], 1); err != nil || found {
			t.Fatalf("%s: found %v, err %v", name, found, err)
		}
	}
	if found, err := db.SetPlexAccount(ctx, 500, 2, 1); err != nil || !found {
		t.Fatalf("link: found %v, err %v", found, err)
	}
	if watched() != 1 || unlinked() != 0 {
		t.Fatalf("after link: watched %d, unlinked %d", watched(), unlinked())
	}

	// Syncs rewrite users and plays; the link stays.
	if err := db.UpsertUser(ctx, User{SeerrID: 2, Name: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePlays(ctx, plays); err != nil {
		t.Fatal(err)
	}
	if as, _ = db.PlexAccounts(ctx); len(as) != 1 || as[0].SeerrID != 2 || watched() != 1 {
		t.Fatalf("after sync: %+v, watched %d", as, watched())
	}

	if found, err := db.SetPlexAccount(ctx, 500, 0, 1); err != nil || !found {
		t.Fatalf("unlink: found %v, err %v", found, err)
	}
	if watched() != 0 || unlinked() != 1 {
		t.Fatalf("after unlink: watched %d, unlinked %d", watched(), unlinked())
	}
}

// Years of accounts: several Plex accounts, one person.
func TestManyPlexAccountsOneUser(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	secs := []Section{{Key: "1", Title: "Movies", Type: "movie"}}
	movies := []Media{
		{RatingKey: "1", SectionKey: "1", Type: "movie", Title: "a"},
		{RatingKey: "2", SectionKey: "1", Type: "movie", Title: "b"},
		{RatingKey: "3", SectionKey: "1", Type: "movie", Title: "c"},
	}
	if err := db.ReplaceLibrary(ctx, secs, movies); err != nil {
		t.Fatal(err)
	}
	// This user has a Plex ID in Seerr too; old accounts add to it.
	if err := db.UpsertUser(ctx, User{SeerrID: 2, Name: "x", PlexID: sql.NullInt64{Int64: 700, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	rk := func(k string) sql.NullString { return sql.NullString{String: k, Valid: true} }
	if err := db.ReplaceRequests(ctx, []Request{
		{SeerrRequestID: 1, SeerrUserID: 2, RatingKey: rk("1")},
		{SeerrRequestID: 2, SeerrUserID: 2, RatingKey: rk("2")},
		{SeerrRequestID: 3, SeerrUserID: 2, RatingKey: rk("3")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePlays(ctx, []Play{
		{ReferenceID: 1, PlexUserID: 700, RatingKey: "1", ShowRatingKey: "1", Watched: true},
		{ReferenceID: 2, PlexUserID: 501, RatingKey: "2", ShowRatingKey: "2", Watched: true},
		{ReferenceID: 3, PlexUserID: 502, RatingKey: "3", ShowRatingKey: "3", Watched: true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{501, 502} {
		if found, err := db.SetPlexAccount(ctx, id, 2, 1); err != nil || !found {
			t.Fatal(id, found, err)
		}
	}
	items, err := db.LibraryItems(ctx, User{IsAdmin: true}, "1", LibraryFilter{RequesterWatched: true})
	if err != nil || len(items) != 3 {
		t.Fatalf("%+v %v", items, err)
	}
	if as, _ := db.PlexAccounts(ctx); len(as) != 2 || as[0].SeerrID != 2 || as[1].SeerrID != 2 {
		t.Fatalf("%+v", as)
	}
}

func TestDeleteWindowTimes(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 5, h, m, 0, 0, time.UTC) }
	day := DeleteWindow{Start: "03:00", End: "05:00"}
	overnight := DeleteWindow{Start: "23:00", End: "02:00"}
	for name, c := range map[string]struct {
		w    DeleteWindow
		t    time.Time
		in   bool
		next time.Time
	}{
		"off":              {DeleteWindow{}, at(12, 0), true, at(12, 0)},
		"at the start":     {day, at(3, 0), true, at(3, 0)},
		"inside":           {day, at(4, 59), true, at(4, 59)},
		"at the end":       {day, at(5, 0), false, at(3, 0).AddDate(0, 0, 1)},
		"before":           {day, at(2, 59), false, at(3, 0)},
		"overnight late":   {overnight, at(23, 30), true, at(23, 30)},
		"overnight early":  {overnight, at(1, 0), true, at(1, 0)},
		"overnight closed": {overnight, at(12, 0), false, at(23, 0)},
		"overnight at end": {overnight, at(2, 0), false, at(23, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.w.Contains(c.t); got != c.in {
				t.Fatalf("Contains: %v", got)
			}
			if got := c.w.Next(c.t); !got.Equal(c.next) {
				t.Fatalf("Next: %v", got)
			}
		})
	}
	for _, w := range []DeleteWindow{{Start: "03:00"}, {End: "03:00"}, {Start: "03:00", End: "03:00"}, {Start: "x", End: "03:00"}, {Start: "25:00", End: "03:00"}} {
		if w.Validate() == nil {
			t.Fatalf("%+v passed", w)
		}
		if w.Contains(at(3, 0)) {
			t.Fatalf("%+v must contain nothing", w)
		}
	}
	db := openTest(t)
	ctx := context.Background()
	if w, err := db.DeleteWindow(ctx); err != nil || w != (DeleteWindow{}) {
		t.Fatal(w, err)
	}
	if err := db.SetDeleteWindow(ctx, DeleteWindow{Start: "03:00"}); err == nil {
		t.Fatal("saved a bad window")
	}
	if err := db.SetDeleteWindow(ctx, day); err != nil {
		t.Fatal(err)
	}
	if w, err := db.DeleteWindow(ctx); err != nil || w != day {
		t.Fatal(w, err)
	}
}
