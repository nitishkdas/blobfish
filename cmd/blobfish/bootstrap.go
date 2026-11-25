package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nitishkdas/blobfish/internal/auth"
)

// initialKeyFile holds the first admin key, created when a data dir has no
// keys at all. It is not part of backups.
const initialKeyFile = "initial-admin-key"

// bootstrapAdminKey creates the first "*:admin" key when there are none.
// The token goes to a file only this user can read, never to the log:
// under systemd and similar, stderr ends up in the journal and stays there.
// It is also shown when stderr is an interactive terminal.
func bootstrapAdminKey(ctx context.Context, svc *auth.Service, dataDir string, log *slog.Logger, stderr io.Writer) error {
	path := filepath.Join(dataDir, initialKeyFile)
	token, err := svc.EnsureAdminKey(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		if _, err := os.Stat(path); err == nil {
			log.Warn("the initial admin key file is still on disk; delete it once the key is stored safely", "path", path)
		}
		return nil
	}
	if err := writeSecretFile(path, token+"\n"); err != nil {
		return fmt.Errorf("save the new admin key to %s: %w (recover with: blobfish -new-admin-key)", path, err)
	}
	log.Warn("no API keys existed, so an admin key was created and saved to a file only this user can read; "+
		"copy it somewhere safe, then delete the file", "path", path)
	if isTerminal(stderr) {
		fmt.Fprintf(stderr, "\n  Admin key: %s\n  (also saved in %s)\n\n", token, path)
	}
	return nil
}

func writeSecretFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // in case it existed with looser bits
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// isTerminal reports whether w is an interactive terminal (not a pipe,
// file or journal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
