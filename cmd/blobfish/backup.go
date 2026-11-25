package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nitishkdas/blobfish/internal/config"
	"github.com/nitishkdas/blobfish/internal/engine"
)

// runBackup: `blobfish backup -o FILE` with the server stopped. With the
// server running, use GET /v1/backup instead (the data dir is locked).
func runBackup(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("blobfish backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", getenv("BLOBFISH_CONFIG"), "path to JSON config file (env BLOBFISH_CONFIG)")
	out := fs.String("o", "", "write the backup to this file (required)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: blobfish backup -o FILE [-config FILE]")
		fmt.Fprintln(stderr, "Writes a consistent snapshot of the data dir. The server must be stopped;")
		fmt.Fprintln(stderr, "while it runs, download GET /v1/backup with an admin key instead.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		fs.Usage()
		return errors.New("backup: -o is required")
	}
	cfg, err := config.Load(*configPath, getenv)
	if err != nil {
		return err
	}
	eng, err := engine.Open(cfg.DataDir, engine.Options{})
	if err != nil {
		return fmt.Errorf("%w\nIf the server is running, back up through it instead:\n"+
			"  curl -H 'Authorization: Bearer <admin key>' http://<server>/v1/backup -o backup.tar", err)
	}
	defer eng.Close()

	// Write to a .partial file and rename only once complete, so a failed
	// run never leaves something that looks like a finished backup.
	partial := *out + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := eng.Backup(ctx, f); err != nil {
		f.Close()
		os.Remove(partial)
		return fmt.Errorf("backup: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, *out); err != nil {
		return err
	}
	st, _ := os.Stat(*out)
	fmt.Fprintf(stdout, "Backup written to %s (%d bytes). It contains API key hashes and share link tokens: keep it private.\n",
		*out, st.Size())
	return nil
}

// runRestore: `blobfish restore -from FILE -data-dir DIR` unpacks a backup
// into an empty dir and verifies every object.
func runRestore(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("blobfish restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "backup file to restore (required)")
	dir := fs.String("data-dir", "", "new, empty data dir to restore into (required)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: blobfish restore -from FILE -data-dir DIR")
		fmt.Fprintln(stderr, "Restores into an empty DIR and checks every object's SHA-256. Use it to")
		fmt.Fprintln(stderr, "verify a backup too. Then point the server's data_dir at DIR.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *dir == "" {
		fs.Usage()
		return errors.New("restore: -from and -data-dir are required")
	}
	f, err := os.Open(*from)
	if err != nil {
		return err
	}
	defer f.Close()
	abs, _ := filepath.Abs(*dir)
	st, err := engine.Restore(ctx, f, abs, engine.Options{})
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Fprintf(stdout, "Restored and verified %d objects (%d bytes) in %d buckets into %s.\n",
		st.Objects, st.Bytes, st.Buckets, abs)
	return nil
}
