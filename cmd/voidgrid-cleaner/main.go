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
	"syscall"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/config"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
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

	hc := &http.Client{Timeout: 30 * time.Second}
	s := &web.Server{
		DB:      db,
		Seerr:   &seerr.Client{BaseURL: cfg.SeerrURL, APIKey: cfg.SeerrAPIKey, HTTP: hc},
		PlexTV:  &plex.TV{BaseURL: "https://plex.tv", AppURL: "https://app.plex.tv/auth", ClientID: clientID, Product: "voidgrid-cleaner", HTTP: hc},
		PinWait: 3 * time.Second,
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
