package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// A backup is a tar stream:
//
//	manifest.json         format version, time, counts
//	meta.db               bbolt snapshot from one read transaction
//	objects/ab/cd/<id>    every blob that snapshot references
//
// While a backup runs, blob deletions (after overwrites and deletes) and
// the janitor are paused, so no blob the snapshot needs can vanish before
// it is copied. Deferred deletions run when the last backup finishes.

var _ store.Backuper = (*Engine)(nil)

type backupManifest struct {
	Format  int       `json:"format"`
	Created time.Time `json:"created"`
	Blobs   int       `json:"blobs"`
}

var blobEntry = regexp.MustCompile(`^objects/([0-9a-f]{2})/([0-9a-f]{2})/([0-9a-f]{32})$`)

func (e *Engine) beginBackup() {
	e.mu.Lock()
	e.backups++
	e.mu.Unlock()
}

func (e *Engine) endBackup() {
	e.mu.Lock()
	e.backups--
	var deferred []string
	if e.backups == 0 {
		deferred, e.deferredRemovals = e.deferredRemovals, nil
	}
	e.mu.Unlock()
	for _, id := range deferred {
		e.removeBlob(id)
	}
}

// Backup implements store.Backuper.
func (e *Engine) Backup(ctx context.Context, w io.Writer) error {
	e.beginBackup()
	defer e.endBackup()

	tw := tar.NewWriter(w)
	var ids []string
	err := e.db.View(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bktBlobs).ForEach(func(k, _ []byte) error {
			ids = append(ids, string(k))
			return nil
		}); err != nil {
			return err
		}
		m, err := json.Marshal(backupManifest{Format: FormatVersion, Created: e.now(), Blobs: len(ids)})
		if err != nil {
			return err
		}
		if err := writeTarFile(tw, "manifest.json", int64(len(m)), func(w io.Writer) error {
			_, err := w.Write(m)
			return err
		}); err != nil {
			return err
		}
		// tx.WriteTo writes the database exactly as this transaction sees it.
		return writeTarFile(tw, "meta.db", tx.Size(), func(w io.Writer) error {
			_, err := tx.WriteTo(w)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("backup metadata: %w", err)
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.backupBlob(tw, id); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish backup: %w", err)
	}
	e.log.Info("backup written", "blobs", len(ids))
	return nil
}

func (e *Engine) backupBlob(tw *tar.Writer, id string) error {
	f, err := os.Open(e.blobPath(id))
	if err != nil {
		return fmt.Errorf("backup blob %s: %w", id, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return writeTarFile(tw, filepath.ToSlash(filepath.Join("objects", id[:2], id[2:4], id)), st.Size(), func(w io.Writer) error {
		_, err := io.Copy(w, f)
		return err
	})
}

func writeTarFile(tw *tar.Writer, name string, size int64, body func(io.Writer) error) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Size: size, Mode: 0o600, ModTime: time.Now(), Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	return body(tw)
}

// RestoreStats reports what Restore verified.
type RestoreStats struct {
	Buckets int
	Objects int
	Bytes   int64
}

// Restore unpacks a backup into dir, which must not exist or be empty, and
// then reads every object back, checking it against its stored SHA-256.
// A backup that restores without error is known to be complete and intact.
func Restore(ctx context.Context, r io.Reader, dir string, opts Options) (st RestoreStats, err error) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		return st, fmt.Errorf("restore target %s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	// The target was empty or missing, so everything in it is ours: on
	// failure, clear it so a retry isn't blocked by half-restored files.
	existed := err == nil
	defer func() {
		if err == nil {
			return
		}
		if !existed {
			os.RemoveAll(dir)
			return
		}
		leftovers, _ := os.ReadDir(dir)
		for _, l := range leftovers {
			os.RemoveAll(filepath.Join(dir, l.Name()))
		}
	}()
	for _, d := range []string{dir, filepath.Join(dir, "objects"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return st, err
		}
	}

	var sawManifest, sawMeta bool
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("read backup: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return st, fmt.Errorf("backup entry %q is not a regular file", hdr.Name)
		}
		switch m := blobEntry.FindStringSubmatch(hdr.Name); {
		case hdr.Name == "manifest.json":
			var man backupManifest
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&man); err != nil {
				return st, fmt.Errorf("backup manifest: %w", err)
			}
			if man.Format > FormatVersion {
				return st, fmt.Errorf("%w: backup has data format %d, this binary supports up to %d",
					ErrFormatTooNew, man.Format, FormatVersion)
			}
			sawManifest = true
		case hdr.Name == "meta.db":
			if err := writeFileDurable(filepath.Join(dir, "meta.db"), tr); err != nil {
				return st, fmt.Errorf("read backup entry meta.db: %w", err)
			}
			sawMeta = true
		case m != nil:
			if m[1] != m[3][:2] || m[2] != m[3][2:4] {
				return st, fmt.Errorf("backup entry %q is in the wrong shard", hdr.Name)
			}
			shard := filepath.Join(dir, "objects", m[1], m[2])
			if err := os.MkdirAll(shard, 0o750); err != nil {
				return st, err
			}
			if err := writeFileDurable(filepath.Join(shard, m[3]), tr); err != nil {
				return st, fmt.Errorf("read backup entry %s: %w", hdr.Name, err)
			}
		default:
			// Only known names are accepted: nothing like "../x" can escape dir.
			return st, fmt.Errorf("unexpected backup entry %q", hdr.Name)
		}
	}
	if !sawManifest || !sawMeta {
		return st, errors.New("backup is incomplete: manifest.json or meta.db missing")
	}
	if err := syncDir(dir); err != nil {
		return st, err
	}

	// Open runs the format check and janitor; then read every object back.
	e, err := Open(dir, opts)
	if err != nil {
		return st, fmt.Errorf("open restored data: %w", err)
	}
	defer e.Close()
	return e.verifyAll(ctx)
}

// verifyAll reads every object in full; checksum or size mismatches surface
// as store.ErrDataCorrupt from the verifying reader.
func (e *Engine) verifyAll(ctx context.Context) (RestoreStats, error) {
	var st RestoreStats
	buckets, err := e.ListBuckets(ctx)
	if err != nil {
		return st, err
	}
	var bad []string
	for _, b := range buckets {
		st.Buckets++
		cursor := ""
		for {
			res, err := e.List(ctx, b.Name, store.ListOptions{Cursor: cursor, Limit: store.MaxListLimit})
			if err != nil {
				return st, err
			}
			for _, o := range res.Objects {
				rc, _, err := e.Get(ctx, b.Name, o.Key, nil)
				if err == nil {
					var n int64
					n, err = io.Copy(io.Discard, rc)
					rc.Close()
					st.Bytes += n
				}
				if err != nil {
					bad = append(bad, fmt.Sprintf("%s/%s: %v", b.Name, o.Key, err))
					continue
				}
				st.Objects++
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
	}
	if len(bad) > 0 {
		return st, fmt.Errorf("%w: %d object(s) failed verification, first: %s", store.ErrDataCorrupt, len(bad), bad[0])
	}
	return st, nil
}

func writeFileDurable(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
