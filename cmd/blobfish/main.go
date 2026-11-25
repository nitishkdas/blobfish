// Command blobfish runs the BlobFish object store server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nitishkdas/blobfish/internal/config"
	"github.com/nitishkdas/blobfish/internal/server"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

const shutdownTimeout = 15 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "blobfish:", err)
		os.Exit(1)
	}
}

// run is main without process-global side effects, so it can be tested.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("blobfish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", getenv("BLOBFISH_CONFIG"), "path to JSON config file (env BLOBFISH_CONFIG)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, "blobfish", version)
		return nil
	}

	cfg, err := config.Load(*configPath, getenv)
	if err != nil {
		return err
	}
	log, err := newLogger(cfg, stderr)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{
		Handler: server.New(server.Options{Logger: log, MaxObjectSize: cfg.MaxObjectSize}),
		// Only the header read is bounded. Bodies can be multi-GB streams on
		// slow links, so there is deliberately no ReadTimeout/WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// No BaseContext tied to ctx: on SIGTERM, in-flight uploads should
		// finish during Shutdown, not be cancelled the moment the signal lands.
	}

	log.Info("blobfish starting", "version", version, "addr", ln.Addr().String(), "data_dir", cfg.DataDir)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("stopped")
	return nil
}

func newLogger(cfg config.Config, w io.Writer) (*slog.Logger, error) {
	lvl, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return slog.New(slog.NewTextHandler(w, opts)), nil
}
