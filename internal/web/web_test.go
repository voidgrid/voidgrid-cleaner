package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Fake Seerr: user 1 "admin" (plexId 100, admin), user 2 "bob" (plexId 200).
// Local login: admin@x / pw -> 1. Fake plex.tv: pin 9 approves as plexID,
// set per test.
func newTestApp(t *testing.T, plexID int64) (app *httptest.Server, client *http.Client, srv *Server) {
	t.Helper()
	users := []map[string]any{
		{"id": 1, "displayName": "admin", "plexId": 100, "permissions": 2},
		{"id": 2, "displayName": "bob", "plexId": 200, "permissions": 32},
	}
	sm := http.NewServeMux()
	sm.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": users})
	})
	sm.HandleFunc("GET /api/v1/user/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(users[0])
	})
	sm.HandleFunc("POST /api/v1/auth/local", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["email"] != "admin@x" || b["password"] != "pw" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	seerrSrv := httptest.NewServer(sm)
	t.Cleanup(seerrSrv.Close)

	pm := http.NewServeMux()
	pm.HandleFunc("POST /api/v2/pins", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":9,"code":"cc"}`))
	})
	pm.HandleFunc("GET /api/v2/pins/9", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"authToken":"t"}`))
	})
	pm.HandleFunc("GET /api/v2/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": plexID, "username": "someone"})
	})
	plexSrv := httptest.NewServer(pm)
	t.Cleanup(plexSrv.Close)

	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:      db,
		Seerr:   &seerr.Client{BaseURL: seerrSrv.URL, APIKey: "k"},
		PlexTV:  &plex.TV{BaseURL: plexSrv.URL, AppURL: "https://app.example/auth", ClientID: "cid", Product: "p"},
		SyncNow: func() { syncCalls.Add(1) },
		Posters: &fakePosters{},
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	s.PosterCache = root
	app = httptest.NewTLSServer(s.Router())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	client = app.Client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return app, client, s
}

// fakePosters counts fetches; thumbs starting "/bad" fail.
type fakePosters struct{ calls atomic.Int32 }

func (f *fakePosters) Poster(_ context.Context, thumb string, _, _ int) ([]byte, error) {
	f.calls.Add(1)
	if strings.HasPrefix(thumb, "/bad") {
		return nil, errors.New("plex down")
	}
	return []byte("JPEG " + thumb), nil
}

// seedLibrary: Movies (1) holds 10 requested by bob (2) and 11 requested by
// nobody; TV (2) holds 20 requested by admin (1). Users come from login or
// are inserted here.
func seedLibrary(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []storage.User{{SeerrID: 1, Name: "admin", IsAdmin: true}, {SeerrID: 2, Name: "bob"}} {
		if err := s.DB.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	secs := []storage.Section{{Key: "1", Title: "Movies", Type: "movie"}, {Key: "2", Title: "TV", Type: "show"}}
	ms := []storage.Media{
		{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "Bobs Movie", Thumb: "/t/10"},
		{RatingKey: "11", SectionKey: "1", Type: "movie", Title: "Unrequested", Thumb: "/bad/11"},
		{RatingKey: "20", SectionKey: "2", Type: "show", Title: "Admins Show", Thumb: "/t/20"},
	}
	if err := s.DB.ReplaceLibrary(ctx, secs, ms); err != nil {
		t.Fatal(err)
	}
	rk := func(k string) sql.NullString { return sql.NullString{String: k, Valid: true} }
	if err := s.DB.ReplaceRequests(ctx, []storage.Request{
		{SeerrRequestID: 1, SeerrUserID: 2, MediaType: "movie", RatingKey: rk("10")},
		{SeerrRequestID: 2, SeerrUserID: 1, MediaType: "tv", RatingKey: rk("20")},
	}); err != nil {
		t.Fatal(err)
	}
}

var syncCalls atomic.Int32

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func postForm(t *testing.T, c *http.Client, u string, v url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, v)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func get(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	app, c, _ := newTestApp(t, 0)
	resp := get(t, c, app.URL+"/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLocalLoginAndLogout(t *testing.T) {
	app, c, _ := newTestApp(t, 0)
	if resp := postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"bad"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", resp.StatusCode)
	}
	resp := postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d %s", resp.StatusCode, body(t, resp))
	}
	if b := body(t, get(t, c, app.URL+"/")); !strings.Contains(b, "admin (admin)") {
		t.Fatalf("home page missing user: %s", b)
	}
	postForm(t, c, app.URL+"/logout", nil)
	if resp := get(t, c, app.URL+"/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestLocalLoginRateLimited(t *testing.T) {
	app, c, _ := newTestApp(t, 0)
	for i := 0; i < maxFails; i++ {
		postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"bad"}})
	}
	// Blocked even with the right password.
	if resp := postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestPlexLogin(t *testing.T) {
	app, c, _ := newTestApp(t, 200)
	resp := get(t, c, app.URL+"/login/plex")
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "https://app.example/auth#?") {
		t.Fatalf("start: %d %s", resp.StatusCode, loc)
	}
	frag, _ := url.ParseQuery(strings.TrimPrefix(loc, "https://app.example/auth#?"))
	if fwd := frag.Get("forwardUrl"); !strings.HasPrefix(fwd, "https://") {
		t.Fatalf("forwardUrl not https: %s", fwd)
	}
	resp = get(t, c, app.URL+"/login/plex/callback")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", resp.StatusCode, body(t, resp))
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && !ck.Secure {
			t.Fatal("session cookie not Secure")
		}
	}
	if b := body(t, get(t, c, app.URL+"/")); !strings.Contains(b, "bob") || strings.Contains(b, "(admin)") {
		t.Fatalf("home: %s", b)
	}
}

func TestPlexLoginUnknownUser(t *testing.T) {
	app, c, _ := newTestApp(t, 999)
	get(t, c, app.URL+"/login/plex")
	if resp := get(t, c, app.URL+"/login/plex/callback"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestPlexCallbackWithoutPinCookie(t *testing.T) {
	app, c, _ := newTestApp(t, 200)
	if resp := get(t, c, app.URL+"/login/plex/callback"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	app, c, _ := newTestApp(t, 0)
	req, _ := http.NewRequest(http.MethodPost, app.URL+"/login/local", strings.NewReader("email=admin%40x&password=pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestSyncPageAdminOnly(t *testing.T) {
	app, c, _ := newTestApp(t, 200) // bob, not an admin
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	if resp := get(t, c, app.URL+"/sync"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET /sync: %d", resp.StatusCode)
	}
	before := syncCalls.Load()
	if resp := postForm(t, c, app.URL+"/sync", nil); resp.StatusCode != http.StatusForbidden || syncCalls.Load() != before {
		t.Fatalf("non-admin POST /sync: %d", resp.StatusCode)
	}
	if b := body(t, get(t, c, app.URL+"/")); strings.Contains(b, `href="/sync"`) {
		t.Fatal("non-admin sees the sync link")
	}

	app, c, _ = newTestApp(t, 0)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	if b := body(t, get(t, c, app.URL+"/sync")); !strings.Contains(b, "no sync has run yet") {
		t.Fatalf("admin sync page: %s", b)
	}
	before = syncCalls.Load()
	resp := postForm(t, c, app.URL+"/sync", nil)
	if resp.StatusCode != http.StatusSeeOther || syncCalls.Load() != before+1 {
		t.Fatalf("admin POST /sync: %d calls %d", resp.StatusCode, syncCalls.Load()-before)
	}
}

func TestLibraryTabsAndOwnership(t *testing.T) {
	app, c, s := newTestApp(t, 200) // bob
	seedLibrary(t, s)
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")

	// /library opens bob's first tab; TV holds nothing of his, so no tab.
	if resp := get(t, c, app.URL+"/library"); resp.Header.Get("Location") != "/library/1" {
		t.Fatalf("redirect %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	b := body(t, get(t, c, app.URL+"/library/1"))
	if !strings.Contains(b, "Bobs Movie") || strings.Contains(b, "Unrequested") || strings.Contains(b, `href="/library/2"`) ||
		strings.Contains(b, "managed by") {
		t.Fatalf("bob's library page: %s", b)
	}
	resp := get(t, c, app.URL+"/library/2")
	if b := body(t, resp); resp.StatusCode != http.StatusForbidden || !strings.Contains(b, "sorry Dave, I can&#39;t do that") ||
		!strings.Contains(b, `href="/library/1"`) || strings.Contains(b, "Admins Show") {
		t.Fatalf("bob opening a library with nothing of his: %d %s", resp.StatusCode, b)
	}

	app, c, s = newTestApp(t, 0) // admin
	seedLibrary(t, s)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	b = body(t, get(t, c, app.URL+"/library/1"))
	if !strings.Contains(b, "Bobs Movie") || !strings.Contains(b, "Unrequested") || !strings.Contains(b, `href="/library/2"`) ||
		!strings.Contains(b, "plex only") || !strings.Contains(b, "bob") {
		t.Fatalf("admin's library page: %s", b)
	}
	if !strings.Contains(b, `popovertarget="item-10"`) || !strings.Contains(b, `id="item-10"`) {
		t.Fatalf("card not wired to its details: %s", b)
	}
	if i, j := strings.Index(b, `href="/library/1"`), strings.Index(b, `href="/library/2"`); i < 0 || j < i {
		t.Fatal("tabs not in Plex's order")
	}
}

func TestPosterIsCached(t *testing.T) {
	app, c, s := newTestApp(t, 200)
	seedLibrary(t, s)
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	fp := s.Posters.(*fakePosters)
	for range 2 {
		resp := get(t, c, app.URL+"/poster/10")
		if b := body(t, resp); resp.StatusCode != http.StatusOK || b != "JPEG /t/10" || resp.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("poster %d %q", resp.StatusCode, b)
		}
	}
	if n := fp.calls.Load(); n != 1 {
		t.Fatalf("plex fetched %d times, want 1 (cached)", n)
	}
	if resp := get(t, c, app.URL+"/poster/999"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown item: %d", resp.StatusCode)
	}
	if resp := get(t, c, app.URL+"/poster/11"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("plex failure: %d", resp.StatusCode)
	}
	// A new client on the same TLS transport: no cookie jar, so no session.
	// (app.Client() would return the shared client, jar included.)
	anon := &http.Client{
		Transport:     app.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if resp := get(t, anon, app.URL+"/poster/10"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("poster without a session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLibrariesPage(t *testing.T) {
	app, c, s := newTestApp(t, 200) // bob, not an admin
	seedLibrary(t, s)
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	if resp := get(t, c, app.URL+"/libraries"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET: %d", resp.StatusCode)
	}
	if resp := postForm(t, c, app.URL+"/libraries/1/ignore", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin POST: %d", resp.StatusCode)
	}
	if secs, _ := s.DB.Sections(context.Background()); secs[0].Ignored {
		t.Fatal("non-admin ignored a library")
	}

	app, c, s = newTestApp(t, 0)
	seedLibrary(t, s)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	if resp := postForm(t, c, app.URL+"/libraries/1/ignore", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ignore: %d", resp.StatusCode)
	}
	if b := body(t, get(t, c, app.URL+"/libraries")); !strings.Contains(b, "/libraries/1/unignore") || !strings.Contains(b, "/libraries/2/ignore") {
		t.Fatalf("libraries page: %s", b)
	}
	if resp := get(t, c, app.URL+"/library/1"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ignored tab: %d", resp.StatusCode)
	}
	before := syncCalls.Load()
	if resp := postForm(t, c, app.URL+"/libraries/1/unignore", nil); resp.StatusCode != http.StatusSeeOther || syncCalls.Load() != before+1 {
		t.Fatalf("unignore: %d, syncs %d", resp.StatusCode, syncCalls.Load()-before)
	}
	if resp := postForm(t, c, app.URL+"/libraries/99/ignore", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown library: %d", resp.StatusCode)
	}
}

func TestLibrarySearchForm(t *testing.T) {
	app, c, s := newTestApp(t, 0)
	seedLibrary(t, s)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	b := body(t, get(t, c, app.URL+"/library/1?q=bobs&sort=newest"))
	if !strings.Contains(b, "Bobs Movie") || strings.Contains(b, "Unrequested") || !strings.Contains(b, `value="bobs"`) ||
		!strings.Contains(b, `value="newest" selected`) || !strings.Contains(b, `matching`) {
		t.Fatalf("search page: %s", b)
	}
	if b := body(t, get(t, c, app.URL+`/library/1?q=%3Cscript%3E`)); strings.Contains(b, "<script>") {
		t.Fatal("search text not escaped")
	}
}

func TestSetStatus(t *testing.T) {
	app, c, s := newTestApp(t, 200) // bob, who requested item 10
	seedLibrary(t, s)
	ctx := context.Background()
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	if b := body(t, get(t, c, app.URL+"/library/1")); strings.Contains(b, "/library/item/") {
		t.Fatal("non-admin sees status buttons")
	}
	// Refused even on his own item, and on someone else's, by direct POST.
	for _, key := range []string{"10", "20"} {
		if resp := postForm(t, c, app.URL+"/library/item/"+key+"/keep", nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin POST on %s: %d", key, resp.StatusCode)
		}
	}
	admin := storage.User{IsAdmin: true}
	items, _ := s.DB.LibraryItems(ctx, admin, "1", storage.LibraryFilter{Status: storage.StatusKeep})
	if len(items) != 0 {
		t.Fatalf("non-admin changed a status: %+v", items)
	}

	app, c, s = newTestApp(t, 0)
	seedLibrary(t, s)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	req, _ := http.NewRequest(http.MethodPost, app.URL+"/library/item/10/keep", nil)
	req.Header.Set("Referer", app.URL+"/library/1?q=bobs&sort=newest")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/library/1?q=bobs&sort=newest#card-10" {
		t.Fatalf("set status: %d %q", resp.StatusCode, loc)
	}
	b := body(t, get(t, c, app.URL+"/library/1?status=keep"))
	if !strings.Contains(b, "Bobs Movie") || strings.Contains(b, "Unrequested") || !strings.Contains(b, `class="badge"`) {
		t.Fatalf("keep filter: %s", b)
	}
	if strings.Contains(b, "/library/item/10/ephemeral") {
		t.Fatal("a movie offers ephemeral")
	}
	if b := body(t, get(t, c, app.URL+"/library/2")); !strings.Contains(b, "/library/item/20/ephemeral") {
		t.Fatal("a show doesn't offer ephemeral")
	}
	for path, want := range map[string]int{
		"/library/item/10/ephemeral": http.StatusBadRequest, // a movie
		"/library/item/10/nonsense":  http.StatusBadRequest,
		"/library/item/999/keep":     http.StatusNotFound,
		"/library/item/20/ephemeral": http.StatusSeeOther,
		"/library/item/10/normal":    http.StatusSeeOther,
	} {
		if resp := postForm(t, c, app.URL+path, nil); resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	items, _ = s.DB.LibraryItems(ctx, storage.User{IsAdmin: true}, "1", storage.LibraryFilter{})
	if items[0].Status != storage.StatusNormal {
		t.Fatalf("back to normal: %q", items[0].Status)
	}
}

func TestUsersPage(t *testing.T) {
	seedPlay := func(s *Server) {
		t.Helper()
		seedLibrary(t, s)
		if err := s.DB.ReplacePlays(context.Background(), []storage.Play{
			{ReferenceID: 1, PlexUserID: 500, UserName: "shared tv", RatingKey: "10", ShowRatingKey: "10", Watched: true, Stopped: 100},
		}); err != nil {
			t.Fatal(err)
		}
	}
	app, c, s := newTestApp(t, 200) // bob, not an admin
	seedPlay(s)
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	if resp := get(t, c, app.URL+"/users"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET /users: %d", resp.StatusCode)
	}
	form := url.Values{"seerr_user": {"2"}}
	if resp := postForm(t, c, app.URL+"/users/plex/500", form); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin POST: %d", resp.StatusCode)
	}

	app, c, s = newTestApp(t, 0)
	seedPlay(s)
	postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	if b := body(t, get(t, c, app.URL+"/users")); !strings.Contains(b, "shared tv") || strings.Contains(b, `value="2" selected`) {
		t.Fatalf("users page: %s", b)
	}
	if resp := postForm(t, c, app.URL+"/users/plex/500", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("link: %d", resp.StatusCode)
	}
	if b := body(t, get(t, c, app.URL+"/users")); !strings.Contains(b, `value="2" selected`) {
		t.Fatalf("link not shown: %s", b)
	}
	if b := body(t, get(t, c, app.URL+"/library/1?requester=1")); !strings.Contains(b, "Bobs Movie") {
		t.Fatalf("requester watched: %s", b)
	}
	for path, want := range map[string]int{"/users/plex/999": http.StatusNotFound, "/users/plex/x": http.StatusNotFound} {
		if resp := postForm(t, c, app.URL+path, form); resp.StatusCode != want {
			t.Fatalf("POST %s: %d", path, resp.StatusCode)
		}
	}
	if resp := postForm(t, c, app.URL+"/users/plex/500", url.Values{"seerr_user": {"x"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad user: %d", resp.StatusCode)
	}
	if resp := postForm(t, c, app.URL+"/users/plex/500", url.Values{"seerr_user": {""}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unlink: %d", resp.StatusCode)
	}
	if b := body(t, get(t, c, app.URL+"/library/1?requester=1")); strings.Contains(b, "Bobs Movie") {
		t.Fatalf("still counted after unlink: %s", b)
	}
}
