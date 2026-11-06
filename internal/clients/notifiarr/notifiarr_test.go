package notifiarr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotify(t *testing.T) {
	reply, code := `{"result":"success"}`, http.StatusOK
	var got struct {
		Notification struct {
			Name string `json:"name"`
		} `json:"notification"`
		Discord struct {
			Text struct {
				Title, Description string
			} `json:"text"`
			IDs struct {
				Channel int64 `json:"channel"`
			} `json:"ids"`
		} `json:"discord"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/notification/passthrough/sekrit" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "sekrit", Channel: 735481457153277994}
	ctx := context.Background()
	if err := c.Notify(ctx, "t", "d"); err != nil {
		t.Fatal(err)
	}
	if got.Notification.Name == "" || got.Discord.IDs.Channel != 735481457153277994 || got.Discord.Text.Title != "t" || got.Discord.Text.Description != "d" {
		t.Fatalf("payload: %+v", got)
	}

	reply = `{"result":"error"}`
	if err := c.Notify(ctx, "t", "d"); err == nil {
		t.Fatal("unconfirmed notification treated as sent")
	}
	code = http.StatusUnauthorized
	err := c.Notify(ctx, "t", "d")
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("error must exist and hide the key: %v", err)
	}
}
