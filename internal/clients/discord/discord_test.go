package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	reply, code := `{"id":"1"}`, http.StatusOK
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/channels/55/messages" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bot sekrit" || !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			t.Errorf("headers: %v", r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "sekrit", Channel: "55"}
	ctx := context.Background()
	if err := c.Notify(ctx, "hello", 7); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, want := range []string{`"content":"hello"`, `"custom_id":"keep:7"`, `"parse":[]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("no %s in %s", want, b)
		}
	}
	reply = `{}`
	if err := c.Notify(ctx, "hello", 7); err == nil {
		t.Fatal("a reply without a message counted as delivered")
	}
	code = http.StatusForbidden
	err := c.Notify(ctx, "hello", 7)
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("error must exist and hide the token: %v", err)
	}
}

func TestVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":1}`)
	sign := func(ts string, b []byte) string {
		return hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), b...)))
	}
	old := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	for name, c := range map[string]struct {
		key  ed25519.PublicKey
		sig  string
		ts   string
		body []byte
		want bool
	}{
		"good":         {pub, sign(ts, body), ts, body, true},
		"other key":    {otherPub, sign(ts, body), ts, body, false},
		"changed body": {pub, sign(ts, body), ts, []byte(`{"type":3}`), false},
		"changed time": {pub, sign(ts, body), old, body, false},
		"old":          {pub, sign(old, body), old, body, false},
		"future":       {pub, sign(future, body), future, body, false},
		"not a time":   {pub, sign("x", body), "x", body, false},
		"not hex":      {pub, "zz", ts, body, false},
		"short":        {pub, "abcd", ts, body, false},
		"empty":        {pub, "", "", nil, false},
		"no key":       {nil, sign(ts, body), ts, body, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Verify(c.key, c.sig, c.ts, c.body, now); got != c.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestParseKeep(t *testing.T) {
	if id, ok := ParseKeep("keep:12"); !ok || id != 12 {
		t.Fatal(id, ok)
	}
	for _, s := range []string{"", "keep:", "keep:x", "delete:12", "12"} {
		if _, ok := ParseKeep(s); ok {
			t.Fatalf("accepted %q", s)
		}
	}
}
