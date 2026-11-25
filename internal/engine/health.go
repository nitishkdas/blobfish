package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

var _ store.HealthChecker = (*Engine)(nil)

// Health implements store.HealthChecker: it reads the metadata database,
// writes and fsyncs a probe file on the data disk, and measures free space.
func (e *Engine) Health(ctx context.Context) store.HealthReport {
	rep := store.HealthReport{FreeBytes: -1, Reserve: e.minFree}
	rep.Metadata = e.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bktSettings) == nil {
			return errors.New("settings bucket missing")
		}
		return nil
	})
	rep.Disk = e.probeDisk()
	if free, err := e.freeSpace(); err == nil {
		rep.FreeBytes = int64(free)
		rep.LowSpace = e.minFree > 0 && int64(free) <= e.minFree
	}
	return rep
}

// probeDisk proves the data disk takes writes: a read-only remount, a full
// disk or a dying device all fail here before they fail an upload.
func (e *Engine) probeDisk() error {
	name := "health-" + newBlobID()
	e.track(name) // the janitor must not race the probe
	defer e.untrack(name)
	path := filepath.Join(e.tmpDir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString("ok")
	serr := f.Sync()
	cerr := f.Close()
	rerr := os.Remove(path)
	return errors.Join(werr, serr, cerr, rerr)
}
