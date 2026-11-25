// Package engine is the on-disk implementation of store.Store and
// store.Buckets: a bbolt metadata index plus object bytes in plain files.
//
// Layout under the data dir:
//
//	meta.db              bbolt: "buckets", "objects/<bucket>/<key>", "blobs/<id>",
//	                     "keys/<token hash>", "settings",
//	                     "links/<token>", "linkidx/<bucket>\x00<key>\x00<id>" -> token
//	objects/ab/cd/<id>   object bytes, random 32-hex-char IDs
//	tmp/                 uploads in progress
//
// # Crash safety
//
// A Put streams into tmp/, fsyncs, renames into objects/, fsyncs the
// directory, and only then commits metadata in one bbolt transaction. A blob
// file whose ID is not in the "blobs" index is an orphan: either an upload
// that never committed or an old version whose delete did not finish. The
// janitor deletes orphans at Open and on a timer. A crash at any step
// therefore leaves the old object or the new one, never a partial file.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

var (
	bktBuckets = []byte("buckets") // name -> bucketRecord
	bktObjects = []byte("objects") // nested bucket per BlobFish bucket: key -> objectRecord
	bktBlobs   = []byte("blobs")   // blob ID -> empty; every committed blob file
)

var (
	_ store.Store   = (*Engine)(nil)
	_ store.Buckets = (*Engine)(nil)
)

// errSimulatedCrash is returned only when a test crash hook fires.
var errSimulatedCrash = errors.New("simulated crash")

// Options configures Open.
type Options struct {
	Logger *slog.Logger
}

// Engine stores objects in a data directory. It is safe for concurrent use.
// Only one Engine (one process) may have a data directory open at a time;
// bbolt's file lock enforces this.
type Engine struct {
	dir, objDir, tmpDir string
	db                  *bbolt.DB
	log                 *slog.Logger
	now                 func() time.Time
	presignSecret       []byte

	// inflight holds names of tmp files and blob IDs that a running Put owns
	// but has not committed yet. The janitor skips them.
	mu       sync.Mutex
	inflight map[string]struct{}

	// crash, when set by a test, is called at each named step of a write.
	// Returning true makes the write stop dead, skipping all cleanup, as if
	// the process had been killed at that point.
	crash func(point string) bool
}

type bucketRecord struct {
	Created time.Time `json:"created"`
	Public  bool      `json:"public,omitempty"`
}

type objectRecord struct {
	Blob        string            `json:"blob"`
	Size        int64             `json:"size"`
	ContentType string            `json:"ct"`
	ETag        string            `json:"etag"`
	Modified    time.Time         `json:"mod"`
	User        map[string]string `json:"meta,omitempty"`
}

func (r objectRecord) info(bucket, key string) store.ObjectInfo {
	return store.ObjectInfo{
		Bucket:      bucket,
		Key:         key,
		Size:        r.Size,
		ContentType: r.ContentType,
		ETag:        r.ETag,
		Modified:    r.Modified,
		User:        r.User,
	}
}

// Open opens (creating if needed) the data directory at dir and removes any
// orphans a previous crash left behind before returning.
func Open(dir string, opts Options) (*Engine, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		dir:      dir,
		objDir:   filepath.Join(dir, "objects"),
		tmpDir:   filepath.Join(dir, "tmp"),
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
		inflight: make(map[string]struct{}),
	}
	for _, d := range []string{dir, e.objDir, e.tmpDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	db, err := bbolt.Open(filepath.Join(dir, "meta.db"), 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open meta.db (is another blobfish using %s?): %w", dir, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bktBuckets, bktObjects, bktBlobs, bktKeys, bktSettings, bktLinks, bktLinkIdx} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		settings := tx.Bucket(bktSettings)
		secret := settings.Get(setPresign)
		if secret == nil {
			secret = make([]byte, 32)
			rand.Read(secret)
			if err := settings.Put(setPresign, secret); err != nil {
				return err
			}
		}
		e.presignSecret = bytes.Clone(secret) // bbolt memory is only valid inside the tx
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init meta.db: %w", err)
	}
	e.db = db

	stats, err := e.RunJanitor(context.Background())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("startup janitor: %w", err)
	}
	if stats.TmpRemoved+stats.BlobsRemoved > 0 {
		log.Info("removed leftovers from an earlier run", "tmp_files", stats.TmpRemoved, "orphan_blobs", stats.BlobsRemoved)
	}
	return e, nil
}

// Close releases the metadata database. In-flight calls must have returned.
func (e *Engine) Close() error {
	return e.db.Close()
}

func (e *Engine) crashAt(point string) bool {
	return e.crash != nil && e.crash(point)
}

func (e *Engine) track(name string) {
	e.mu.Lock()
	e.inflight[name] = struct{}{}
	e.mu.Unlock()
}

func (e *Engine) untrack(name string) {
	e.mu.Lock()
	delete(e.inflight, name)
	e.mu.Unlock()
}

func (e *Engine) tracked(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.inflight[name]
	return ok
}

func newBlobID() string {
	var b [16]byte
	rand.Read(b[:]) // never fails since Go 1.24
	return hex.EncodeToString(b[:])
}

// blobPath shards by the first four hex chars: objects/ab/cd/abcd....
func (e *Engine) blobPath(id string) string {
	return filepath.Join(e.objDir, id[:2], id[2:4], id)
}

// removeBlob deletes a blob file that is no longer referenced. Failure is
// logged, not returned: the metadata change already committed, and the
// janitor will retry.
func (e *Engine) removeBlob(id string) {
	if err := os.Remove(e.blobPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.log.Warn("could not remove unreferenced blob; janitor will retry", "blob", id, "err", err)
	}
}

func objectsOf(tx *bbolt.Tx, bucket string) (*bbolt.Bucket, error) {
	b := tx.Bucket(bktObjects).Bucket([]byte(bucket))
	if b == nil {
		return nil, fmt.Errorf("%w: %s", store.ErrNoSuchBucket, bucket)
	}
	return b, nil
}

func decodeObject(v []byte) (objectRecord, error) {
	var r objectRecord
	if err := json.Unmarshal(v, &r); err != nil {
		return r, fmt.Errorf("decode object record: %w", err)
	}
	return r, nil
}
