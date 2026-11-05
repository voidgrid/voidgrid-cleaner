package plex

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakePMS serves response shapes from the developer.plex.tv/pms examples
// and records every call as "METHOD path?query".
func fakePMS(t *testing.T) (*Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /library/sections/all", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":3,"Directory":[
			{"key":"1","type":"movie","title":"Movies"},
			{"key":"2","type":"show","title":"TV Shows"},
			{"key":"3","type":"artist","title":"Music"}]}}`))
	})
	mux.HandleFunc("GET /library/sections/1/all", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("includeGuids") != "1" {
			t.Error("includeGuids not set")
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":1,"Metadata":[
			{"ratingKey":"1049","guid":"plex://movie/5d776824f54112001f5bbdd7","type":"movie","title":"Zoolander","year":2001,
			 "thumb":"/library/metadata/1049/thumb/1434341184","addedAt":1408525217,
			 "Guid":[{"id":"imdb://tt0196229"},{"id":"tmdb://9398"}]}]}}`))
	})
	mux.HandleFunc("/photo/:/transcode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("url") != "/library/metadata/1049/thumb/1434341184" || r.URL.Query().Get("width") != "200" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("JPEG"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {}) // refresh, emptyTrash
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Server{BaseURL: srv.URL, Token: "tok"}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestSectionsAndItems(t *testing.T) {
	s, _ := fakePMS(t)
	ctx := context.Background()
	secs, err := s.Sections(ctx)
	if err != nil || len(secs) != 3 || secs[1].Key != "2" || secs[1].Type != "show" {
		t.Fatalf("sections %+v %v", secs, err)
	}
	items, err := s.Items(ctx, "1")
	if err != nil || len(items) != 1 {
		t.Fatalf("items %+v %v", items, err)
	}
	it := items[0]
	if it.RatingKey != "1049" || it.AddedAt != 1408525217 || it.ExternalID("tmdb") != "9398" || it.ExternalID("tvdb") != "" || it.PlexGUID != "plex://movie/5d776824f54112001f5bbdd7" {
		t.Fatalf("item %+v", it)
	}
}

func TestRefreshAndEmptyTrashMethods(t *testing.T) {
	s, calls := fakePMS(t)
	ctx := context.Background()
	if err := s.RefreshSection(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.EmptyTrash(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0] != "POST /library/sections/1/refresh" || got[1] != "PUT /library/sections/1/emptyTrash" {
		t.Fatalf("calls %v", got)
	}
}

func TestPoster(t *testing.T) {
	s, _ := fakePMS(t)
	b, err := s.Poster(context.Background(), "/library/metadata/1049/thumb/1434341184", 200, 300)
	if err != nil || !bytes.Equal(b, []byte("JPEG")) {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := s.Poster(context.Background(), "/nope", 200, 300); err == nil {
		t.Fatal("bad thumb should fail")
	}
}

func TestBadTokenIsAnError(t *testing.T) {
	s, _ := fakePMS(t)
	s.Token = "wrong"
	if _, err := s.Sections(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
