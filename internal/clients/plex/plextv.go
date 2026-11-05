// Package plex talks to plex.tv (sign-in) and the Plex Media Server.
package plex

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// TV is the plex.tv PIN sign-in flow, the same one Seerr uses.
type TV struct {
	BaseURL  string // https://plex.tv
	AppURL   string // https://app.plex.tv/auth
	ClientID string // stable per install
	Product  string
	HTTP     *http.Client
}

// Pin is a pending plex.tv sign-in.
type Pin struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// Account is the plex.tv account that signed in.
type Account struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Title    string `json:"title"`
	Email    string `json:"email"`
}

// CreatePin starts a sign-in.
func (t *TV) CreatePin(ctx context.Context) (Pin, error) {
	var p Pin
	err := t.do(ctx, http.MethodPost, "/api/v2/pins?strong=true", "", &p)
	return p, err
}

// AuthURL is where the browser goes to approve the pin; Plex sends it back
// to forwardURL afterwards.
func (t *TV) AuthURL(p Pin, forwardURL string) string {
	v := url.Values{}
	v.Set("clientID", t.ClientID)
	v.Set("code", p.Code)
	v.Set("forwardUrl", forwardURL)
	v.Set("context[device][product]", t.Product)
	// Plex reads these from the fragment, not the query.
	return t.AppURL + "#?" + v.Encode()
}

// PinToken returns the auth token once the pin is approved, or "" if not yet.
func (t *TV) PinToken(ctx context.Context, p Pin) (string, error) {
	var r struct {
		AuthToken *string `json:"authToken"`
	}
	path := "/api/v2/pins/" + strconv.FormatInt(p.ID, 10) + "?code=" + url.QueryEscape(p.Code)
	if err := t.do(ctx, http.MethodGet, path, "", &r); err != nil {
		return "", err
	}
	if r.AuthToken == nil {
		return "", nil
	}
	return *r.AuthToken, nil
}

// Account returns who a token belongs to.
func (t *TV) Account(ctx context.Context, token string) (Account, error) {
	var a Account
	err := t.do(ctx, http.MethodGet, "/api/v2/user", token, &a)
	return a, err
}

func (t *TV) do(ctx context.Context, method, path, token string, out any) error {
	hdr := http.Header{}
	hdr.Set("X-Plex-Client-Identifier", t.ClientID)
	hdr.Set("X-Plex-Product", t.Product)
	if token != "" {
		hdr.Set("X-Plex-Token", token)
	}
	return httpjson.Do(ctx, t.HTTP, "plex.tv", method, strings.TrimRight(t.BaseURL, "/")+path, hdr, nil, out)
}
