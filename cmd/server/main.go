package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/photoasset"
	"github.com/Mino829/trace_Backend/internal/push"
	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	db, err := store.Open(startupCtx, cfg.DatabaseURL, cfg.DatabaseMaxConnections)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "migrate":
		if err := db.Migrate(startupCtx); err != nil {
			slog.Error("migration failed", "error", err)
			os.Exit(1)
		}
		slog.Info("database migration complete")
		return
	case "serve", "cleanup":
	default:
		slog.Error("usage: trace-backend [serve|migrate|cleanup]")
		os.Exit(1)
	}
	if err := db.Ping(startupCtx); err != nil {
		slog.Error("schema not ready; run migrate first")
		os.Exit(1)
	}
	if cfg.DisplayEnabled {
		db.EnableDisplay()
	}
	var photos *photo.Service
	if cfg.PhotoBucket != "" {
		assets, err := photoasset.NewGCS(startupCtx, cfg.PhotoBucket, cfg.PhotoSignerEmail)
		if err != nil {
			slog.Error("photo storage initialization failed; check private bucket policy and runtime permissions")
			os.Exit(1)
		}
		defer assets.Close()
		photos = photo.New(db, assets)
	} else {
		required, err := db.PhotoStorageRequired(startupCtx)
		if err != nil || required {
			slog.Error("photo storage configuration is required while retained assets or upload fences exist")
			os.Exit(1)
		}
	}
	var reminders *push.Service
	if cfg.PushEnabled {
		reminders, err = push.New(cfg)
		if err != nil {
			slog.Error("invalid push configuration")
			os.Exit(1)
		}
	} else if err := db.ClearPush(startupCtx); err != nil {
		slog.Error("failed to erase subscriptions while push is disabled")
		os.Exit(1)
	}
	if photos != nil || cfg.DisplayEnabled {
		graph, err := route.Embedded()
		if err != nil {
			slog.Error("invalid embedded CampusGraph", "error", err)
			os.Exit(1)
		}
		if err := db.ConfigureRoutes(graph, cfg.CoordinateVersion, cfg.CampusGraphVersion, cfg.ThreeDMapVersion); err != nil {
			slog.Error("display/photo routing requires compatible graph, coordinate and 3D map versions", "error", err)
			os.Exit(1)
		}
	}
	if err := cleanupData(startupCtx, db, photos, time.Now().UTC()); err != nil {
		slog.Error("startup cleanup failed; persisted deletion intents are retained")
		os.Exit(1)
	}
	if mode == "cleanup" {
		slog.Info("database and photo cleanup complete")
		return
	}
	cancelStartup()

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           api.New(cfg, db, photos, reminders),
		ReadHeaderTimeout: 5 * time.Second,
		// The WebSocket route supplies its own 30s heartbeat/read deadline and
		// 50m lifetime. HTTP requests remain bounded by API middleware contexts.
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	go cleanupExpired(ctx, db, photos, cfg.DisplayEnabled)
	if reminders != nil {
		go dispatchReminders(ctx, db, reminders)
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("TRACE backend listening", "address", cfg.Address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
	<-shutdownDone
}

func cleanupExpired(ctx context.Context, db *store.Store, photos *photo.Service, displayEnabled bool) {
	ticker := time.NewTicker(30 * time.Second)
	positionTicker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer positionTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-positionTicker.C:
			db.PurgePositions(now.UTC())
			if displayEnabled {
				displayCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := db.RefreshDisplay(displayCtx, now.UTC()); err != nil && ctx.Err() == nil {
					slog.Error("display expiry cleanup failed")
				}
				cancel()
			}
		case now := <-ticker.C:
			cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := db.Ping(cleanupCtx); err != nil {
				slog.Error("schema not ready; skipping cleanup", "error", err)
				cancel()
				continue
			}
			if err := cleanupData(cleanupCtx, db, photos, now.UTC()); err != nil && ctx.Err() == nil {
				slog.Error("cleanup expired data failed; persisted deletion intents are retained")
			}
			cancel()
		}
	}
}

func cleanupData(ctx context.Context, db *store.Store, photos *photo.Service, now time.Time) error {
	if err := db.Cleanup(ctx, now); err != nil {
		return err
	}
	if photos == nil {
		return nil
	}
	if err := photos.Purge(ctx, now); err != nil {
		return err
	}
	return db.Cleanup(ctx, now)
}

func dispatchReminders(ctx context.Context, db *store.Store, reminders *push.Service) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			workCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := reminders.Dispatch(workCtx, db, now.UTC()); err != nil && ctx.Err() == nil {
				slog.Error("push dispatch failed")
			}
			cancel()
		}
	}
}
