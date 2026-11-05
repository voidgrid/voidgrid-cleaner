package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Fake Seerr: user 1 "admin" (plexId 100, admin), user 2 "bob" (plexId 200).
// Local login: admin@x / pw -> 1. Fake plex.tv: pin 9 approves as plexID,
// set per test.
func newTestApp(t *testing.T, plexID int64) (app *httptest.Server, client *http.Client) {
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
		DB:     db,
		Seerr:  &seerr.Client{BaseURL: seerrSrv.URL, APIKey: "k"},
		PlexTV: &plex.TV{BaseURL: plexSrv.URL, AppURL: "https://app.example/auth", ClientID: "cid", Product: "p"},
	}
	app = httptest.NewTLSServer(s.Router())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	client = app.Client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return app, client
}

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
	app, c := newTestApp(t, 0)
	resp := get(t, c, app.URL+"/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLocalLoginAndLogout(t *testing.T) {
	app, c := newTestApp(t, 0)
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
	app, c := newTestApp(t, 0)
	for i := 0; i < maxFails; i++ {
		postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"bad"}})
	}
	// Blocked even with the right password.
	if resp := postForm(t, c, app.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestPlexLogin(t *testing.T) {
	app, c := newTestApp(t, 200)
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
	app, c := newTestApp(t, 999)
	get(t, c, app.URL+"/login/plex")
	if resp := get(t, c, app.URL+"/login/plex/callback"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestPlexCallbackWithoutPinCookie(t *testing.T) {
	app, c := newTestApp(t, 200)
	if resp := get(t, c, app.URL+"/login/plex/callback"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	app, c := newTestApp(t, 0)
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
