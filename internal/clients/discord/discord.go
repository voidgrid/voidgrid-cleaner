// Package discord posts notices with Keep and Add me buttons and checks the
// signed requests Discord sends when someone presses one.
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

const (
	keepPrefix = "keep:"
	joinPrefix = "join:"
)

// Client posts to one channel as the bot.
type Client struct {
	BaseURL string // https://discord.com/api/v10
	Token   string
	Channel string
	// AdminChannel takes tech alerts; "" sends admin notices to Channel.
	AdminChannel string
	HTTP         *http.Client
}

// Notify posts text with a Keep button for the deletion. It returns nil only
// once Discord has replied with the created message.
func (c *Client) Notify(ctx context.Context, text string, deletionID int64) error {
	return c.post(ctx, map[string]any{
		"content":          text,
		"allowed_mentions": noMentions,
		"components":       button(3, "Keep", keepPrefix+strconv.FormatInt(deletionID, 10)),
	})
}

// Announce posts text with an Add me button for the show.
func (c *Client) Announce(ctx context.Context, text, ratingKey string) error {
	return c.post(ctx, map[string]any{
		"content":          text,
		"allowed_mentions": noMentions,
		"components":       button(1, "Add me", joinPrefix+ratingKey),
	})
}

// Alert posts text tagging one user, or nobody if userID is empty. Only that
// user can be pinged, whatever the text says.
func (c *Client) Alert(ctx context.Context, text, userID string) error {
	ch := c.AdminChannel
	if ch == "" {
		ch = c.Channel
	}
	if userID == "" {
		return c.postTo(ctx, ch, map[string]any{"content": text, "allowed_mentions": noMentions})
	}
	return c.postTo(ctx, ch, map[string]any{
		"content":          "<@" + userID + "> " + text,
		"allowed_mentions": map[string]any{"users": []string{userID}},
	})
}

// GuildOwner returns the owner of the server the channel is in.
func (c *Client) GuildOwner(ctx context.Context) (string, error) {
	var ch struct {
		GuildID string `json:"guild_id"`
	}
	if err := c.do(ctx, http.MethodGet, "/channels/"+c.Channel, nil, &ch); err != nil {
		return "", err
	}
	var g struct {
		OwnerID string `json:"owner_id"`
	}
	err := c.do(ctx, http.MethodGet, "/guilds/"+ch.GuildID, nil, &g)
	return g.OwnerID, err
}

// noMentions stops a title or name pinging anyone.
var noMentions = map[string]any{"parse": []string{}}

func button(style int, label, customID string) []any {
	return []any{map[string]any{"type": 1, "components": []any{map[string]any{
		"type": 2, "style": style, "label": label, "custom_id": customID,
	}}}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bot "+c.Token)
	hdr.Set("User-Agent", "DiscordBot (https://github.com/voidgrid/voidgrid-cleaner, 1)")
	return httpjson.Do(ctx, c.HTTP, "discord", method, strings.TrimRight(c.BaseURL, "/")+path, hdr, body, out)
}

// Post sends plain text that can't ping anyone, to the admin channel if
// admin is set, otherwise to the main one.
func (c *Client) Post(ctx context.Context, admin bool, text string) error {
	ch := c.Channel
	if admin {
		if c.AdminChannel == "" {
			return errors.New("discord: no admin channel set")
		}
		ch = c.AdminChannel
	}
	return c.postTo(ctx, ch, map[string]any{"content": text, "allowed_mentions": noMentions})
}

func (c *Client) post(ctx context.Context, body map[string]any) error {
	return c.postTo(ctx, c.Channel, body)
}

func (c *Client) postTo(ctx context.Context, channel string, body map[string]any) error {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/channels/"+channel+"/messages", body, &out); err != nil {
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

// ParseJoin returns the show an Add me button belongs to.
func ParseJoin(customID string) (string, bool) {
	return strings.CutPrefix(customID, joinPrefix)
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
