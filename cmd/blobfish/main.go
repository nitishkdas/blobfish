// Command blobfish runs the BlobFish object store server.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/config"
	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/server"
	"github.com/nitishkdas/blobfish/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

const (
	shutdownTimeout = 15 * time.Second
	janitorInterval = time.Hour
	// minQuotaBytes is the smallest bucket quota the API accepts: anything
	// less is a typo, not a useful limit.
	minQuotaBytes = 10 << 20
)

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
	if len(args) > 0 {
		switch args[0] {
		case "backup":
			return runBackup(ctx, args[1:], getenv, stdout, stderr)
		case "restore":
			return runRestore(ctx, args[1:], getenv, stdout, stderr)
		case "verify":
			return runVerify(ctx, args[1:], getenv, stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("blobfish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", getenv("BLOBFISH_CONFIG"), "path to JSON config file (env BLOBFISH_CONFIG)")
	showVersion := fs.Bool("version", false, "print version and exit")
	newAdminKey := fs.Bool("new-admin-key", false, "create a new *:admin API key, print it and exit (server must be stopped)")
	resetPassword := fs.String("reset-password", "", "set a new random password for this user, print it and exit; ends their sessions (server must be stopped)")
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

	// Open runs the janitor once, so leftovers from a crash are gone before
	// the first request.
	eng, err := engine.Open(cfg.DataDir, engine.Options{
		Logger: log, MinFreeBytes: cfg.MinFreeSpace, TrashRetention: cfg.TrashRetention(),
	})
	if err != nil {
		return err
	}
	defer eng.Close()

	authSvc := auth.NewService(eng, eng.PresignSecret()).UseUsers(eng)
	if *newAdminKey {
		token, rec, err := authSvc.CreateKey(ctx, "recovery-admin", []string{"*:admin"})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created admin key %s. Store it now; it is not shown again:\n%s\n", rec.ID, token)
		return nil
	}
	if *resetPassword != "" {
		name, err := auth.NormalizeUserName(*resetPassword)
		if err != nil {
			return err
		}
		pw := randomPassword()
		if err := authSvc.SetPassword(ctx, name, pw, auth.Principal{}); err != nil {
			return fmt.Errorf("reset password for %s: %w", name, err)
		}
		fmt.Fprintf(stdout, "New password for %s (their sessions have ended). Give it to them; they can change it after signing in:\n%s\n", name, pw)
		return nil
	}
	if err := bootstrapAdminKey(ctx, authSvc, cfg.DataDir, log, stderr); err != nil {
		return err
	}

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	var janitor sync.WaitGroup
	janitor.Go(func() { eng.JanitorLoop(janitorCtx, janitorInterval) })
	janitor.Go(func() { webhook.New(eng, log, version).Run(janitorCtx) })
	janitor.Go(func() { purgeSessionsLoop(janitorCtx, authSvc, log) })
	// Stop the janitor and webhook dispatcher before the deferred Close runs.
	defer janitor.Wait()
	defer stopJanitor()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ln = server.LimitListener(ln, cfg.MaxConnections)

	trusted, err := server.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: server.New(server.Options{
			Logger:         log,
			MaxObjectSize:  cfg.MaxObjectSize,
			IdleTimeout:    time.Duration(cfg.TransferIdleTimeout),
			TrustedProxies: trusted,
			Store:          eng,
			Buckets:        eng,
			Auth:           authSvc,
			Links:          eng,
			Trash:          eng,
			UploadLinks:    eng,
			Webhooks:       eng,
			Backup:         eng,
			Health:         eng,
			Version:        version,
			DataFormat:     engine.FormatVersion,
			PublicURL:      cfg.PublicURL,
			MinQuotaBytes:  minQuotaBytes,
		}),
		// Headers get a fixed limit. Bodies can be multi-GB streams on slow
		// links, so instead of ReadTimeout/WriteTimeout (which bound the whole
		// request) the server drops transfers only when they stop making
		// progress (server.Options.IdleTimeout).
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10, // ample for X-Meta-* (capped at 2 KB of values)
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

// purgeSessionsLoop deletes ended sign-in sessions every janitorInterval.
// They already stop working when they end; this only frees the space.
func purgeSessionsLoop(ctx context.Context, svc *auth.Service, log *slog.Logger) {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		if n, err := svc.PurgeExpiredSessions(ctx); err != nil {
			log.Error("purge expired sessions", "err", err)
		} else if n > 0 {
			log.Info("purged expired sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// randomPassword returns 128 random bits from crypto/rand as base32
// (letters and 2-7, no look-alike 0/1/8), grouped to read out or type.
func randomPassword() string {
	t := strings.ToLower(rand.Text()) // 26 characters
	return t[:6] + "-" + t[6:12] + "-" + t[12:18] + "-" + t[18:]
}
