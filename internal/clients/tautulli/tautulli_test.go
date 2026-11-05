package tautulli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeTautulli serves get_history in pages (rows shaped like the wiki
// example: ints as numbers, "" for a movie's empty indexes) and get_activity
// (keys as strings, as in the wiki example).
func fakeTautulli(t *testing.T, rows int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("X-Api-Key") != "key" || q.Get("apikey") != "" {
			_, _ = w.Write([]byte(`{"response":{"result":"error","message":"Invalid apikey","data":{}}}`))
			return
		}
		switch q.Get("cmd") {
		case "get_history":
			if q.Get("after") != "2026-10-01" || q.Get("grouping") != "1" {
				t.Errorf("after = %q", q.Get("after"))
			}
			start, _ := strconv.Atoi(q.Get("start"))
			length, _ := strconv.Atoi(q.Get("length"))
			var out []string
			for i := start; i < rows && i < start+length; i++ {
				if i%2 == 0 {
					out = append(out, fmt.Sprintf(`{"reference_id":%d,"user_id":8008135,"user":"dany","media_type":"episode","rating_key":%d,
						"parent_rating_key":544,"grandparent_rating_key":351,"media_index":17,"parent_media_index":7,
						"percent_complete":84,"watched_status":1,"stopped":1462688370}`, 9000+i, 4000+i))
				} else {
					out = append(out, fmt.Sprintf(`{"reference_id":"%d","user_id":8008135,"media_type":"movie","rating_key":%d,
						"parent_rating_key":"","grandparent_rating_key":"","media_index":"","parent_media_index":"",
						"percent_complete":40,"watched_status":0.5,"stopped":1462688370}`, 9000+i, 4000+i))
				}
			}
			if start == 0 && rows > 0 {
				out = append(out, `{"media_type":"track","rating_key":1}`) // music is dropped
				// A session still playing, as Tautulli appends it: null IDs. Dropped.
				out = append(out, `{"reference_id":null,"row_id":null,"id":null,"media_type":"episode","rating_key":7001,
					"user_id":555,"group_count":null,"state":"playing","stopped":1462688400}`)
			}
			_, _ = w.Write([]byte(`{"response":{"result":"success","message":null,"data":{"recordsFiltered":` +
				strconv.Itoa(rows) + `,"data":[` + strings.Join(out, ",") + `]}}}`))
		case "get_activity":
			_, _ = w.Write([]byte(`{"response":{"result":"success","message":null,"data":{"stream_count":"1","sessions":[
				{"grandparent_rating_key":"1219","media_type":"episode","rating_key":"153037","state":"playing","user_id":133788}]}}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "key"}
}

func TestHistoryPagesAndTypes(t *testing.T) {
	c := fakeTautulli(t, historyPage+5)
	after := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	plays, err := c.History(context.Background(), after)
	if err != nil || len(plays) != historyPage+5 {
		t.Fatalf("len=%d err=%v", len(plays), err)
	}
	ep, mv := plays[0], plays[1]
	if ep.MediaType != "episode" || ep.GrandparentRatingKey != 351 || ep.ParentMediaIndex != 7 || ep.MediaIndex != 17 ||
		ep.UserID != 8008135 || ep.WatchedStatus != 1 || ep.PercentComplete != 84 || ep.ReferenceID != 9000 {
		t.Fatalf("episode %+v", ep)
	}
	if mv.MediaType != "movie" || mv.GrandparentRatingKey != 0 || mv.WatchedStatus != 0.5 || mv.ReferenceID != 9001 {
		t.Fatalf("movie %+v", mv)
	}
}

func TestActivity(t *testing.T) {
	s, err := fakeTautulli(t, 0).Activity(context.Background())
	if err != nil || len(s) != 1 || s[0].RatingKey != 153037 || s[0].GrandparentRatingKey != 1219 || s[0].UserID != 133788 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestErrorResult(t *testing.T) {
	c := fakeTautulli(t, 0)
	c.APIKey = "wrong"
	if _, err := c.Activity(context.Background()); err == nil || !strings.Contains(err.Error(), "Invalid apikey") {
		t.Fatalf("got %v", err)
	}
}
