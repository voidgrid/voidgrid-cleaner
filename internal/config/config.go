// Package config reads runtime configuration from environment variables.
// Every service URL and key comes from here (keys typically injected by a
// secrets manager); none are stored in the database.
package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the runtime configuration.
type Config struct {
	ListenAddr     string
	DBPath         string
	SeerrURL       string
	SeerrAPIKey    string
	PlexURL        string
	PlexToken      string
	RadarrURL      string
	RadarrAPIKey   string
	SonarrURL      string
	SonarrAPIKey   string
	TautulliURL    string
	TautulliAPIKey string
	// Discord is optional only while deletion is a dry run.
	DiscordBotToken  string
	DiscordPublicKey ed25519.PublicKey
	DiscordChannel   string
	// DiscordAdminChannel takes tech alerts; optional.
	DiscordAdminChannel string
	DeleteLive          bool
}

// Load reads VGC_* variables, applying defaults. Every service setting is
// required.
func Load() (Config, error) {
	c := Config{
		ListenAddr: getEnv("VGC_LISTEN_ADDR", ":8790"),
		DBPath:     getEnv("VGC_DB_PATH", "/data/voidgrid-cleaner.db"),
	}
	var missing []string
	for _, v := range []struct {
		name string
		dst  *string
	}{
		{"VGC_SEERR_URL", &c.SeerrURL},
		{"VGC_SEERR_API_KEY", &c.SeerrAPIKey},
		{"VGC_PLEX_URL", &c.PlexURL},
		{"VGC_PLEX_TOKEN", &c.PlexToken},
		{"VGC_RADARR_URL", &c.RadarrURL},
		{"VGC_RADARR_API_KEY", &c.RadarrAPIKey},
		{"VGC_SONARR_URL", &c.SonarrURL},
		{"VGC_SONARR_API_KEY", &c.SonarrAPIKey},
		{"VGC_TAUTULLI_URL", &c.TautulliURL},
		{"VGC_TAUTULLI_API_KEY", &c.TautulliAPIKey},
	} {
		if *v.dst = os.Getenv(v.name); *v.dst == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("config: required but not set: %s", strings.Join(missing, ", "))
	}
	c.DiscordBotToken, c.DiscordChannel = os.Getenv("VGC_DISCORD_BOT_TOKEN"), os.Getenv("VGC_DISCORD_CHANNEL")
	key := os.Getenv("VGC_DISCORD_PUBLIC_KEY")
	if c.DiscordBotToken == "" != (key == "") || key == "" != (c.DiscordChannel == "") {
		return c, errors.New("config: VGC_DISCORD_BOT_TOKEN, VGC_DISCORD_PUBLIC_KEY and VGC_DISCORD_CHANNEL go together")
	}
	if key != "" {
		b, err := hex.DecodeString(key)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return c, errors.New("config: VGC_DISCORD_PUBLIC_KEY must be the application's hex public key")
		}
		c.DiscordPublicKey = b
		if _, err := strconv.ParseUint(c.DiscordChannel, 10, 64); err != nil {
			return c, errors.New("config: VGC_DISCORD_CHANNEL must be a Discord channel ID")
		}
	}
	if c.DiscordAdminChannel = os.Getenv("VGC_DISCORD_ADMIN_CHANNEL"); c.DiscordAdminChannel != "" {
		if c.DiscordBotToken == "" {
			return c, errors.New("config: VGC_DISCORD_ADMIN_CHANNEL needs the other VGC_DISCORD_ settings")
		}
		if _, err := strconv.ParseUint(c.DiscordAdminChannel, 10, 64); err != nil {
			return c, errors.New("config: VGC_DISCORD_ADMIN_CHANNEL must be a Discord channel ID")
		}
	}
	c.DeleteLive = os.Getenv("VGC_DELETE_LIVE") == "true"
	if c.DeleteLive && c.DiscordBotToken == "" {
		return c, errors.New("config: VGC_DELETE_LIVE needs the VGC_DISCORD_ settings")
	}
	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
