// Package notifiarr sends Discord messages through Notifiarr's passthrough
// integration.
package notifiarr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// Client posts to one Discord channel.
type Client struct {
	BaseURL string // https://notifiarr.com
	APIKey  string
	Channel int64
	HTTP    *http.Client
}

// Notify posts a message. It returns nil only if Notifiarr confirms it.
func (c *Client) Notify(ctx context.Context, title, text string) error {
	body := map[string]any{
		"notification": map[string]any{"update": false, "name": "voidgrid-cleaner"},
		"discord": map[string]any{
			"text": map[string]any{"title": title, "description": text},
			"ids":  map[string]any{"channel": c.Channel},
		},
	}
	var out struct {
		Result string `json:"result"`
	}
	// Notifiarr takes the key in the URL path, so it's scrubbed from errors.
	err := httpjson.Do(ctx, c.HTTP, "notifiarr", http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/api/v1/notification/passthrough/"+c.APIKey, nil, body, &out)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), c.APIKey, "***"))
	}
	if out.Result != "success" {
		return fmt.Errorf("notifiarr: result %q", out.Result)
	}
	return nil
}
