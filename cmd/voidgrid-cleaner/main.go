// Command voidgrid-cleaner serves the web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/config"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
	"github.com/voidgrid/voidgrid-cleaner/internal/sync"
	"github.com/voidgrid/voidgrid-cleaner/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Printf("voidgrid-cleaner: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	// plex.tv identifies this install by a client ID that must stay stable.
	clientID, err := db.Setting(ctx, "plex_client_id", func() string {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	})
	if err != nil {
		return fmt.Errorf("plex client id: %w", err)
	}

	posterDir := filepath.Join(filepath.Dir(cfg.DBPath), "posters")
	if err := os.MkdirAll(posterDir, 0o750); err != nil {
		return fmt.Errorf("poster cache: %w", err)
	}
	posters, err := os.OpenRoot(posterDir)
	if err != nil {
		return fmt.Errorf("poster cache: %w", err)
	}
	defer func() { _ = posters.Close() }()

	hc := &http.Client{Timeout: 30 * time.Second}
	// Whole-library and whole-history reads can take longer than a page load.
	slow := &http.Client{Timeout: 5 * time.Minute}
	sc := &seerr.Client{BaseURL: cfg.SeerrURL, APIKey: cfg.SeerrAPIKey, HTTP: hc}
	syncer := &sync.Syncer{
		DB:       db,
		Seerr:    &seerr.Client{BaseURL: cfg.SeerrURL, APIKey: cfg.SeerrAPIKey, HTTP: slow},
		Plex:     &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: slow},
		Radarr:   arr.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey, slow),
		Sonarr:   arr.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey, slow),
		Tautulli: &tautulli.Client{BaseURL: cfg.TautulliURL, APIKey: cfg.TautulliAPIKey, HTTP: slow},
	}
	s := &web.Server{
		DB:      db,
		Seerr:   sc,
		PlexTV:  &plex.TV{BaseURL: "https://plex.tv", AppURL: "https://app.plex.tv/auth", ClientID: clientID, Product: "voidgrid-cleaner", HTTP: hc},
		PinWait: 3 * time.Second,
		// Posters fetch with the short timeout: a page waits on them.
		Posters:     &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: hc},
		PosterCache: posters,
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// PID 1 in the container: on SIGTERM finish requests and close the DB
	// so the WAL is checkpointed.
	stop, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// shortcut: fixed hourly interval, make it a setting if anyone needs another.
	go syncer.Loop(stop, time.Hour)
	s.SyncNow = func() { go syncer.Run(stop) } // a no-op if one is already running
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("voidgrid-cleaner listening on %s", cfg.ListenAddr)
	select {
	case err := <-errc:
		return err
	case <-stop.Done():
		sctx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		return srv.Shutdown(sctx)
	}
}
