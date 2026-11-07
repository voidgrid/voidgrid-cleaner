// Package discord posts deletion notices with a Keep button and checks the
// signed requests Discord sends when someone presses it.
package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
)

// maxSkew limits replays of a captured request. Discord's docs set no window;
// a replay can only repeat a keep, which is harmless.
const maxSkew = 5 * time.Minute

const keepPrefix = "keep:"

// Client posts to one channel as the bot.
type Client struct {
	BaseURL string // https://discord.com/api/v10
	Token   string
	Channel string
	HTTP    *http.Client
}

// Notify posts text with a Keep button for the deletion. It returns nil only
// once Discord has replied with the created message.
func (c *Client) Notify(ctx context.Context, text string, deletionID int64) error {
	body := map[string]any{
		"content":          text,
		"allowed_mentions": map[string]any{"parse": []string{}}, // a title can't ping anyone
		"components": []any{map[string]any{"type": 1, "components": []any{map[string]any{
			"type": 2, "style": 3, "label": "Keep", "custom_id": keepPrefix + strconv.FormatInt(deletionID, 10),
		}}}},
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bot "+c.Token)
	hdr.Set("User-Agent", "DiscordBot (https://github.com/voidgrid/voidgrid-cleaner, 1)")
	var out struct {
		ID string `json:"id"`
	}
	err := httpjson.Do(ctx, c.HTTP, "discord", http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/channels/"+c.Channel+"/messages", hdr, body, &out)
	if err != nil {
		return err
	}
	if out.ID == "" {
		return errors.New("discord: reply has no message ID")
	}
	return nil
}

// Verify reports whether a request really came from Discord, recently: the
// signature covers the timestamp followed by the raw body.
func Verify(key ed25519.PublicKey, sigHex, timestamp string, body []byte, now time.Time) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize || len(key) != ed25519.PublicKeySize {
		return false
	}
	if !ed25519.Verify(key, append([]byte(timestamp), body...), sig) {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	d := now.Sub(time.Unix(ts, 0))
	return d < maxSkew && d > -maxSkew
}

// ParseKeep returns the deletion a Keep button belongs to.
func ParseKeep(customID string) (int64, bool) {
	s, ok := strings.CutPrefix(customID, keepPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}
