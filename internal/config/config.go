// Package config reads runtime configuration from environment variables.
// Every service URL and key comes from here (keys typically injected by a
// secrets manager); none are stored in the database.
package config

import (
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
	// Notifications are optional only while deletion is a dry run.
	NotifiarrAPIKey  string
	NotifiarrChannel int64
	DeleteLive       bool
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
	c.NotifiarrAPIKey = os.Getenv("VGC_NOTIFIARR_API_KEY")
	if ch := os.Getenv("VGC_NOTIFIARR_CHANNEL"); ch != "" {
		var err error
		if c.NotifiarrChannel, err = strconv.ParseInt(ch, 10, 64); err != nil {
			return c, errors.New("config: VGC_NOTIFIARR_CHANNEL must be a Discord channel ID")
		}
	}
	if (c.NotifiarrAPIKey == "") != (c.NotifiarrChannel == 0) {
		return c, errors.New("config: VGC_NOTIFIARR_API_KEY and VGC_NOTIFIARR_CHANNEL go together")
	}
	c.DeleteLive = os.Getenv("VGC_DELETE_LIVE") == "true"
	if c.DeleteLive && c.NotifiarrAPIKey == "" {
		return c, errors.New("config: VGC_DELETE_LIVE needs VGC_NOTIFIARR_API_KEY and VGC_NOTIFIARR_CHANNEL")
	}
	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
