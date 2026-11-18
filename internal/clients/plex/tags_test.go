package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeTags serves Plex's label and collection calls and records every call as "METHOD uri",
// with the query as Plex receives it.
func fakeTags(t *testing.T) (*Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			t.Errorf("no token on %s", r.URL)
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /library/sections/1/label":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[{"fastKey":"/x","key":"62868","title":"Momma"},{"key":"7","title":"watch-together"}]}}`))
		case "GET /library/sections/1/all":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"10"},{"ratingKey":"11"}]}}`))
		case "GET /library/sections/1/collections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"ratingKey":"500","title":"Alien Collection","subtype":"movie","childCount":4},
				{"ratingKey":"501","title":"Top","subtype":"movie","childCount":10,"smart":true},
				{"ratingKey":"502","title":"Old Smart","subtype":"movie","childCount":3,"smart":"1"}]}}`))
		case "GET /library/collections/500/children":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"10"},{"ratingKey":"12"}]}}`))
		case "GET /identity":
			_, _ = w.Write([]byte(`{"MediaContainer":{"machineIdentifier":"abc123"}}`))
		case "POST /library/collections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"900"}]}}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestLabelsAndTheirItems(t *testing.T) {
	s, calls := fakeTags(t)
	ctx := context.Background()
	ls, err := s.Labels(ctx, "1")
	if err != nil || len(ls) != 2 || ls[0] != (Label{Key: "62868", Title: "Momma"}) {
		t.Fatalf("%+v %v", ls, err)
	}
	keys, err := s.LabelItems(ctx, "1", "62868")
	if err != nil || !reflect.DeepEqual(keys, []string{"10", "11"}) {
		t.Fatalf("%v %v", keys, err)
	}
	if got := calls(); got[1] != "GET /library/sections/1/all?label=62868" {
		t.Fatalf("%v", got)
	}
}

// Plex reads the label edit from its query string: an add per label.tag.tag, a list to remove, locked.
func TestEditLabels(t *testing.T) {
	s, calls := fakeTags(t)
	ctx := context.Background()
	if err := s.EditLabels(ctx, "1", TypeMovie, "10", []string{"watch-together", "Two Words"}, []string{"old", "Kometa"}); err != nil {
		t.Fatal(err)
	}
	want := "PUT /library/sections/1/all?type=1&id=10&label[0].tag.tag=watch-together&label[1].tag.tag=Two%20Words&label[].tag.tag-=old,Kometa&label.locked=1"
	if got := calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %v\nwant %s", got, want)
	}
	if err := s.EditLabels(ctx, "2", TypeShow, "20", nil, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if got := calls()[1]; got != "PUT /library/sections/2/all?type=2&id=20&label[].tag.tag-=x&label.locked=1" {
		t.Fatalf("remove only: %s", got)
	}
	for _, bad := range [][2][]string{{{"a,b"}, nil}, {nil, {"a,b"}}} {
		if err := s.EditLabels(ctx, "1", TypeMovie, "10", bad[0], bad[1]); err == nil {
			t.Fatalf("a comma in %v must be refused", bad)
		}
	}
	if len(calls()) != 2 {
		t.Fatalf("a refused edit reached Plex: %v", calls())
	}
}

func TestCollections(t *testing.T) {
	s, calls := fakeTags(t)
	ctx := context.Background()
	cs, err := s.Collections(ctx, "1")
	if err != nil || len(cs) != 3 || cs[0].Title != "Alien Collection" || cs[0].Count != 4 || cs[0].Smart || !cs[1].Smart || !cs[2].Smart {
		t.Fatalf("%+v %v", cs, err)
	}
	keys, err := s.CollectionItems(ctx, "500")
	if err != nil || !reflect.DeepEqual(keys, []string{"10", "12"}) {
		t.Fatalf("%v %v", keys, err)
	}
	if err := s.DeleteCollection(ctx, "500"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFromCollection(ctx, "500", "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToCollection(ctx, "500", "11"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateCollection(ctx, "1", TypeMovie, "My List", "11")
	if err != nil || key != "900" {
		t.Fatalf("%q %v", key, err)
	}
	got := calls()
	want := []string{
		"GET /library/sections/1/collections",
		"GET /library/collections/500/children",
		"DELETE /library/collections/500",
		"DELETE /library/collections/500/items/10",
		"GET /identity",
		"PUT /library/collections/500/items?uri=server%3A%2F%2Fabc123%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F11",
		"POST /library/collections?type=1&title=My%20List&smart=0&sectionId=1&uri=server%3A%2F%2Fabc123%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F11",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
