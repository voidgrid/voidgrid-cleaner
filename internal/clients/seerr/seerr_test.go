package seerr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// fakeSeerr serves n users (IDs 1..n, plexId = 1000+ID) in pages, and a
// local login that accepts a@x / pw as user 3.
func fakeSeerr(t *testing.T, n int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		take, _ := strconv.Atoi(r.URL.Query().Get("take"))
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		var res []User
		for id := skip + 1; id <= n && id <= skip+take; id++ {
			pid := int64(1000 + id)
			res = append(res, User{ID: int64(id), PlexID: &pid, Username: fmt.Sprint("u", id)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
	})
	mux.HandleFunc("POST /api/v1/auth/local", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			t.Error("local login must not send the API key")
		}
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["email"] != "a@x" || b["password"] != "pw" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(User{ID: 3})
	})
	// One recorded-shape request (fields as in server/entity/MediaRequest.ts and Media.ts).
	mux.HandleFunc("GET /api/v1/request", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "all" || r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("request list query/key: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"pageInfo":{"pages":1,"pageSize":100,"results":1,"page":1},"results":[
			{"id":11,"status":2,"is4k":false,"createdAt":"2026-10-01T12:00:00.000Z",
			 "requestedBy":{"id":2,"displayName":"bob","permissions":32},
			 "media":{"id":5,"mediaType":"tv","tmdbId":1399,"tvdbId":121361,"status":4,"ratingKey":"351","externalServiceId":17},
			 "seasons":[{"id":1,"seasonNumber":1,"status":2},{"id":2,"seasonNumber":2,"status":2}]}]}`))
	})
	mux.HandleFunc("DELETE /api/v1/media/5", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "key"}
}

func TestUserByPlexIDPages(t *testing.T) {
	c := fakeSeerr(t, 250)
	u, found, err := c.UserByPlexID(context.Background(), 1000+234)
	if err != nil || !found || u.ID != 234 {
		t.Fatalf("got %+v %v %v", u, found, err)
	}
	if _, found, err := c.UserByPlexID(context.Background(), 5); err != nil || found {
		t.Fatalf("unknown plex id: found=%v err=%v", found, err)
	}
}

func TestLocalLogin(t *testing.T) {
	c := fakeSeerr(t, 0)
	if id, err := c.LocalLogin(context.Background(), "a@x", "pw"); err != nil || id != 3 {
		t.Fatalf("good login: %d %v", id, err)
	}
	if _, err := c.LocalLogin(context.Background(), "a@x", "nope"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("bad login: %v", err)
	}
}

func TestRequests(t *testing.T) {
	rs, err := fakeSeerr(t, 0).Requests(context.Background())
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %v", rs, err)
	}
	r := rs[0]
	if r.RequestedBy.ID != 2 || r.Media.ID != 5 || r.Media.MediaType != "tv" || *r.Media.RatingKey != "351" ||
		*r.Media.ExternalServiceID != 17 || len(r.Seasons) != 2 || r.Seasons[1].SeasonNumber != 2 || r.CreatedAt.Month() != 10 {
		t.Fatalf("parsed %+v", r)
	}
}

func TestDeleteMedia(t *testing.T) {
	c := fakeSeerr(t, 0)
	if err := c.DeleteMedia(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteMedia(context.Background(), 6); err == nil {
		t.Fatal("unknown media should fail")
	}
}

func TestIsAdmin(t *testing.T) {
	if !(User{Permissions: 2 | 32}).IsAdmin() || !(User{Permissions: 16 | 32}).IsAdmin() || !(User{Permissions: 32 | 128}).IsAdmin() || (User{Permissions: 32}).IsAdmin() {
		t.Fatal("admin bit")
	}
}

func TestAdminIncludesAutoApprove(t *testing.T) {
	for perms, want := range map[int64]bool{2: true, 16: true, 128: true, 256: true, 512: true, 32: false, 0: false, 4194336: false} {
		if got := (User{Permissions: perms}).IsAdmin(); got != want {
			t.Fatalf("%d: got %v", perms, got)
		}
	}
}
