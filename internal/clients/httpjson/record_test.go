package httpjson

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecorderSeesCallsAndNoSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad") {
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	var got []Call
	ctx := WithRecorder(context.Background(), func(c Call) { got = append(got, c) })
	if err := Do(ctx, srv.Client(), "svc", http.MethodDelete, srv.URL+"/thing/1?deleteFiles=true&apikey=sekrit&X-Plex-Token=sekrit", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := Do(ctx, srv.Client(), "svc", http.MethodPut, srv.URL+"/bad", nil, nil, nil); err == nil {
		t.Fatal("no error from a 418")
	}
	if len(got) != 2 || got[0].String() != "svc DELETE /thing/1?deleteFiles=true: 200" || got[1].Status != http.StatusTeapot {
		t.Fatalf("%+v", got)
	}
	if err := Do(context.Background(), srv.Client(), "svc", http.MethodGet, srv.URL+"/x", nil, nil, nil); err != nil || len(got) != 2 {
		t.Fatal("a call without a recorder was recorded", err)
	}
}
