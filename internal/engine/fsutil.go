package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// syncDir fsyncs a directory so that entries created, renamed or removed in
// it survive a power cut.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// mkdirDurable creates path if missing and, when it did create it, fsyncs
// the parent so the new directory entry is durable.
func mkdirDurable(path string) error {
	err := os.Mkdir(path, 0o750)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// ctxReader fails reads once ctx is done, so an abandoned upload stops
// streaming instead of filling the disk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
