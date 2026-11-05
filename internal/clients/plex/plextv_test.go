package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPinFlow(t *testing.T) {
	var approved atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/pins", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Client-Identifier") != "cid" {
			t.Error("missing client id")
		}
		_, _ = w.Write([]byte(`{"id":42,"code":"abcd"}`))
	})
	mux.HandleFunc("GET /api/v2/pins/42", func(w http.ResponseWriter, r *http.Request) {
		if approved.Load() {
			_, _ = w.Write([]byte(`{"id":42,"authToken":"tok"}`))
		} else {
			_, _ = w.Write([]byte(`{"id":42,"authToken":null}`))
		}
	})
	mux.HandleFunc("GET /api/v2/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":1234,"username":"bob"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tv := &TV{BaseURL: srv.URL, AppURL: "https://app.example/auth", ClientID: "cid", Product: "p"}
	ctx := context.Background()
	pin, err := tv.CreatePin(ctx)
	if err != nil || pin.ID != 42 || pin.Code != "abcd" {
		t.Fatalf("pin %+v %v", pin, err)
	}
	if tok, err := tv.PinToken(ctx, pin); err != nil || tok != "" {
		t.Fatalf("unapproved: %q %v", tok, err)
	}
	approved.Store(true)
	tok, err := tv.PinToken(ctx, pin)
	if err != nil || tok != "tok" {
		t.Fatalf("approved: %q %v", tok, err)
	}
	acct, err := tv.Account(ctx, tok)
	if err != nil || acct.ID != 1234 {
		t.Fatalf("account %+v %v", acct, err)
	}

	u := tv.AuthURL(pin, "https://me/cb")
	frag, ok := strings.CutPrefix(u, "https://app.example/auth#?")
	v, _ := url.ParseQuery(frag)
	if !ok || v.Get("code") != "abcd" || v.Get("clientID") != "cid" || v.Get("forwardUrl") != "https://me/cb" {
		t.Fatalf("auth url %s", u)
	}
}
