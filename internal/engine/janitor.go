package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// JanitorStats reports what one janitor pass removed.
type JanitorStats struct {
	TmpRemoved   int
	BlobsRemoved int
}

// RunJanitor deletes files a crash or failed cleanup left behind:
//
//   - files in tmp/ not owned by a running Put
//   - blob files in objects/ whose ID is not in the blob index
//
// It is safe to run while the engine serves requests.
func (e *Engine) RunJanitor(ctx context.Context) (JanitorStats, error) {
	var st JanitorStats

	tmps, err := os.ReadDir(e.tmpDir)
	if err != nil {
		return st, fmt.Errorf("read tmp dir: %w", err)
	}
	for _, ent := range tmps {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if e.tracked(ent.Name()) {
			continue
		}
		removed, err := removeIfExists(filepath.Join(e.tmpDir, ent.Name()))
		if err != nil {
			return st, err
		}
		if removed {
			st.TmpRemoved++
		}
	}

	// objects/ab/cd/<id>
	outer, err := os.ReadDir(e.objDir)
	if err != nil {
		return st, fmt.Errorf("read objects dir: %w", err)
	}
	for _, a := range outer {
		if !a.IsDir() {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(e.objDir, a.Name()))
		if err != nil {
			return st, err
		}
		for _, b := range inner {
			if !b.IsDir() {
				continue
			}
			shard := filepath.Join(e.objDir, a.Name(), b.Name())
			files, err := os.ReadDir(shard)
			if err != nil {
				return st, err
			}
			for _, f := range files {
				if err := ctx.Err(); err != nil {
					return st, err
				}
				if f.IsDir() {
					continue
				}
				orphan, err := e.isOrphan(f.Name())
				if err != nil {
					return st, err
				}
				if !orphan {
					continue
				}
				removed, err := removeIfExists(filepath.Join(shard, f.Name()))
				if err != nil {
					return st, err
				}
				if removed {
					st.BlobsRemoved++
				}
			}
		}
	}
	return st, nil
}

// isOrphan reports whether blob id is neither owned by a running Put nor
// committed. Order matters: a Put tracks the ID before the file appears and
// untracks it only after the commit. So if the file exists and the ID is not
// tracked, any commit has already happened, and the index read that follows
// sees it.
func (e *Engine) isOrphan(id string) (bool, error) {
	if e.tracked(id) || e.backupRunning() {
		return false, nil
	}
	var known bool
	err := e.db.View(func(tx *bbolt.Tx) error {
		known = tx.Bucket(bktBlobs).Get([]byte(id)) != nil
		return nil
	})
	return !known, err
}

func removeIfExists(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("janitor remove %s: %w", path, err)
	}
	return true, nil
}

// JanitorLoop runs RunJanitor every interval until ctx is cancelled.
func (e *Engine) JanitorLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := e.RunJanitor(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				e.log.Error("janitor failed", "err", err)
				continue
			}
			if st.TmpRemoved+st.BlobsRemoved > 0 {
				e.log.Info("janitor cleaned up", "tmp_files", st.TmpRemoved, "orphan_blobs", st.BlobsRemoved)
			}
		}
	}
}

// backupRunning reports whether a backup is copying blobs right now; the
// janitor must not delete then, since a blob unreferenced a moment ago may
// still be in the backup's snapshot.
func (e *Engine) backupRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.backups > 0
}
