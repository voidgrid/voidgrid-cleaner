// Package config reads runtime configuration from environment variables.
//
// Seerr is configured here rather than on the settings screen because
// sign-in depends on it: nobody can reach the settings screen without it.
package config

import (
	"errors"
	"os"
)

// Config is the runtime configuration.
type Config struct {
	ListenAddr  string
	DBPath      string
	SeerrURL    string
	SeerrAPIKey string
}

// Load reads VGC_* variables, applying defaults.
func Load() (Config, error) {
	c := Config{
		ListenAddr:  getEnv("VGC_LISTEN_ADDR", ":8790"),
		DBPath:      getEnv("VGC_DB_PATH", "/data/voidgrid-cleaner.db"),
		SeerrURL:    os.Getenv("VGC_SEERR_URL"),
		SeerrAPIKey: os.Getenv("VGC_SEERR_API_KEY"),
	}
	if c.SeerrURL == "" || c.SeerrAPIKey == "" {
		return c, errors.New("config: VGC_SEERR_URL and VGC_SEERR_API_KEY are required")
	}
	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
