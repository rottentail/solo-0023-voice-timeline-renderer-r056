package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

func main() {
	address := flag.String("addr", ":8093", "HTTP listen address")
	dataDir := flag.String("data", "./var/voice-timeline", "persistent data directory")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		slog.Error("cannot create data directory", "error", err)
		os.Exit(1)
	}
	repo, err := storage.NewRepository(*dataDir)
	if err != nil {
		slog.Error("cannot initialize repository", "error", err)
		os.Exit(1)
	}

	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Recompute any legacy mid-downmix source measurements in place. The
	// backfill is incremental, resumable, and atomic: it never deletes media,
	// does not require re-registration, and yields the lock between batches so
	// in-flight production traffic is not blocked.
	repaired, err := repo.BackfillSourceMeasurements(runContext)
	if err != nil {
		slog.Error("source measurement backfill stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("source measurement backfill finished", "repaired", repaired)
	serviceLayer := service.New(repo, service.DefaultConfig())
	api := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-runContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()

	slog.Info("voice timeline service listening", "addr", *address, "data", *dataDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP service stopped", "error", err)
		os.Exit(1)
	}
}
