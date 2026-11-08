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
	_ "time/tzdata" // the image has no zoneinfo, and TZ sets the delete window's clock

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/discord"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/tautulli"
	"github.com/voidgrid/voidgrid-cleaner/internal/config"
	"github.com/voidgrid/voidgrid-cleaner/internal/deleter"
	"github.com/voidgrid/voidgrid-cleaner/internal/doctor"
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
	tv := &plex.TV{BaseURL: "https://plex.tv", AppURL: "https://app.plex.tv/auth", ClientID: clientID, Product: "voidgrid-cleaner", HTTP: hc}
	syncer := &sync.Syncer{
		Owner: func(ctx context.Context) (int64, error) {
			a, err := tv.Account(ctx, cfg.PlexToken)
			return a.ID, err
		},
		DB:       db,
		Seerr:    &seerr.Client{BaseURL: cfg.SeerrURL, APIKey: cfg.SeerrAPIKey, HTTP: slow},
		Plex:     &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: slow},
		Radarr:   arr.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey, slow),
		Sonarr:   arr.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey, slow),
		Tautulli: &tautulli.Client{BaseURL: cfg.TautulliURL, APIKey: cfg.TautulliAPIKey, HTTP: slow},
	}
	worker := &deleter.Worker{
		DB:       db,
		Radarr:   arr.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey, slow),
		Sonarr:   arr.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey, slow),
		Plex:     &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: slow},
		Seerr:    sc,
		Tautulli: &tautulli.Client{BaseURL: cfg.TautulliURL, APIKey: cfg.TautulliAPIKey, HTTP: hc},
		DryRun:   !cfg.DeleteLive,
		Now:      time.Now,
	}
	if cfg.DiscordBotToken != "" {
		bot := &discord.Client{BaseURL: "https://discord.com/api/v10", Token: cfg.DiscordBotToken, Channel: cfg.DiscordChannel, AdminChannel: cfg.DiscordAdminChannel, HTTP: hc}
		worker.AdminChannel = cfg.DiscordAdminChannel != ""
		worker.Notify = bot
		// The server owner is who admin notices tag until someone picks another.
		if owner, err := bot.GuildOwner(ctx); err != nil {
			log.Printf("discord: server owner: %v", err)
		} else if err := db.DefaultAdminDiscord(ctx, owner); err != nil {
			log.Printf("discord: server owner: %v", err)
		}
	}
	s := &web.Server{
		DryRun:         worker.DryRun,
		DiscordKey:     cfg.DiscordPublicKey,
		DiscordChannel: cfg.DiscordChannel,
		DB:             db,
		Seerr:          sc,
		PlexTV:         tv,
		PinWait:        3 * time.Second,
		// Posters fetch with the short timeout: a page waits on them.
		Posters:     &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: hc},
		PosterCache: posters,
		Episodes:    &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: hc},
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
	go s.WarmLoop(stop, time.Hour)
	s.SyncNow = func() { go syncer.Run(stop) } // a no-op if one is already running
	checkup := &doctor.Doctor{
		DB:      db,
		Radarr:  arr.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey, slow),
		Sonarr:  arr.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey, slow),
		Plex:    &plex.Server{BaseURL: cfg.PlexURL, Token: cfg.PlexToken, HTTP: slow},
		Seerr:   sc,
		FixWait: 3 * time.Second,
		DryRun:  worker.DryRun,
	}
	s.Previews = checkup
	s.Downloads = checkup
	s.Space = checkup
	worker.Disks = checkup
	go worker.Loop(stop, time.Minute)
	s.DiscordAdminChannel = cfg.DiscordAdminChannel
	if worker.AdminChannel {
		if bot, ok := worker.Notify.(*discord.Client); ok {
			checkup.Alerts = bot
		}
	}
	go checkup.WatchDownloads(stop, time.Minute)
	go checkup.FixLoop(stop, 2*time.Second)
	syncer.After = func(ctx context.Context) { checkup.Run(ctx) }
	s.DoctorNow = func() { go checkup.Run(stop) }
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
