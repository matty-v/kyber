// Command kyber-archive-store is Kyber's built-in store for agent disk
// archives (MAT-87/MAT-88). The chart runs it as a one-replica Deployment (Recreate)
// with its own volume on the installation's default StorageClass, so disk
// export works on every installation target without object storage. It
// holds sealed bytes only and serves them to the control plane over a
// token-authenticated HTTP API.
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

	"github.com/matty-v/kyber/pkg/archivestore"
)

func main() {
	dir := envOr("KYBER_ARCHIVE_STORE_DIR", "/data")
	addr := envOr("KYBER_ARCHIVE_STORE_ADDR", ":8090")
	token := os.Getenv("KYBER_ARCHIVE_STORE_TOKEN")
	if token == "" {
		slog.Error("KYBER_ARCHIVE_STORE_TOKEN is required")
		os.Exit(1)
	}
	store, err := archivestore.NewFilesystemStore(dir)
	if err != nil {
		slog.Error("opening store", "error", err)
		os.Exit(1)
	}
	// Uploads interrupted by the previous process are never complete.
	if err := store.SweepPartials(); err != nil {
		slog.Warn("sweeping partial uploads", "error", err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           &archivestore.Server{Store: store, Token: token},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("archive store listening", "addr", addr, "dir", dir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("archive store stopped", "error", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
