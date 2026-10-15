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
	case "cleanup":
		if err := db.Ping(startupCtx); err != nil {
			slog.Error("schema not ready; run migrate first")
			os.Exit(1)
		}
		if err := db.Cleanup(startupCtx, time.Now().UTC()); err != nil {
			slog.Error("cleanup failed", "error", err)
			os.Exit(1)
		}
		slog.Info("database cleanup complete")
		return
	case "serve":
	default:
		slog.Error("usage: trace-backend [serve|migrate|cleanup]")
		os.Exit(1)
	}
	if err := db.Ping(startupCtx); err != nil {
		slog.Error("schema not ready; run migrate first")
		os.Exit(1)
	}
	if err := db.Cleanup(startupCtx, time.Now().UTC()); err != nil {
		slog.Error("startup cleanup failed", "error", err)
		os.Exit(1)
	}
	cancelStartup()

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           api.New(cfg, db),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go cleanupExpired(ctx, db)
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

func cleanupExpired(ctx context.Context, db *store.Store) {
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
		case now := <-ticker.C:
			cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := db.Ping(cleanupCtx); err != nil {
				slog.Error("schema not ready; skipping cleanup", "error", err)
				cancel()
				continue
			}
			if err := db.Cleanup(cleanupCtx, now.UTC()); err != nil && ctx.Err() == nil {
				slog.Error("cleanup expired data", "error", err)
			}
			cancel()
		}
	}
}
