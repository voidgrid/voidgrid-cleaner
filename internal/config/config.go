// Package config reads runtime configuration from environment variables.
// Every service URL and key comes from here (keys typically injected by a
// secrets manager); none are stored in the database.
package config

import (
	"fmt"
	"os"
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
	// IgnoreLibraries are Plex library names the app never touches.
	IgnoreLibraries []string
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
	for _, name := range strings.Split(os.Getenv("VGC_PLEX_IGNORE_LIBRARIES"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.IgnoreLibraries = append(c.IgnoreLibraries, name)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("config: required but not set: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
