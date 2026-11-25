package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Put implements store.Store. Steps, in order:
//
//  1. stream body into tmp/ while hashing
//  2. fsync the temp file
//  3. rename into objects/, fsync the directory
//  4. commit metadata (and the blob index) in one bbolt transaction
//  5. remove the overwritten blob, if any
//
// Until step 4 commits, the new blob is an orphan the janitor may reclaim
// after a crash; after it commits, the old blob is. Readers see the old
// object until the commit and the new one after it.
func (e *Engine) Put(ctx context.Context, bucket, key string, r io.Reader, meta store.Meta) (store.ObjectInfo, error) {
	if err := validateObjectArgs(bucket, key); err != nil {
		return store.ObjectInfo{}, err
	}
	if err := store.ValidateMeta(meta); err != nil {
		return store.ObjectInfo{}, err
	}
	// Fail fast before streaming gigabytes into a bucket that isn't there.
	// The commit re-checks, in case the bucket is deleted meanwhile.
	if err := e.db.View(func(tx *bbolt.Tx) error {
		_, err := objectsOf(tx, bucket)
		return err
	}); err != nil {
		return store.ObjectInfo{}, err
	}

	// 1. Stream into tmp/. The name is registered before the file exists;
	// with os.CreateTemp the janitor could list and delete the file in the
	// gap between creation and registration.
	tmpName := "put-" + newBlobID()
	tmpPath := filepath.Join(e.tmpDir, tmpName)
	e.track(tmpName)
	defer e.untrack(tmpName)
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return store.ObjectInfo{}, fmt.Errorf("create temp file: %w", err)
	}

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx, r})
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return store.ObjectInfo{}, fmt.Errorf("write %s/%s: %w", bucket, key, err)
	}
	if e.crashAt("put:written") {
		tmp.Close()
		return store.ObjectInfo{}, errSimulatedCrash
	}

	// 2. Make the bytes durable.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return store.ObjectInfo{}, fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return store.ObjectInfo{}, fmt.Errorf("close temp file: %w", err)
	}
	if e.crashAt("put:synced") {
		return store.ObjectInfo{}, errSimulatedCrash
	}

	// 3. Move into objects/ under a fresh random ID. Track the ID before the
	// rename so the janitor never sees the file as an orphan.
	id := newBlobID()
	e.track(id)
	defer e.untrack(id)
	final := e.blobPath(id)
	if err := e.ensureShard(id); err != nil {
		os.Remove(tmpPath)
		return store.ObjectInfo{}, fmt.Errorf("create shard dir: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return store.ObjectInfo{}, fmt.Errorf("move blob into place: %w", err)
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		os.Remove(final)
		return store.ObjectInfo{}, fmt.Errorf("fsync shard dir: %w", err)
	}
	if e.crashAt("put:renamed") {
		return store.ObjectInfo{}, errSimulatedCrash
	}

	// 4. Commit metadata. bbolt fsyncs on commit.
	contentType := meta.ContentType
	if contentType == "" {
		contentType = store.DefaultContentType
	}
	rec := objectRecord{
		Blob:        id,
		Size:        size,
		ContentType: contentType,
		ETag:        hex.EncodeToString(h.Sum(nil)),
		Modified:    e.now(),
		User:        maps.Clone(meta.User),
	}
	val, err := json.Marshal(rec)
	if err != nil {
		os.Remove(final)
		return store.ObjectInfo{}, fmt.Errorf("encode object record: %w", err)
	}
	var oldBlob string
	err = e.db.Update(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, bucket)
		if err != nil {
			return err
		}
		if prev := objs.Get([]byte(key)); prev != nil {
			old, err := decodeObject(prev)
			if err != nil {
				return err
			}
			oldBlob = old.Blob
		}
		if err := objs.Put([]byte(key), val); err != nil {
			return err
		}
		blobs := tx.Bucket(bktBlobs)
		if err := blobs.Put([]byte(id), nil); err != nil {
			return err
		}
		if oldBlob != "" {
			return blobs.Delete([]byte(oldBlob))
		}
		return nil
	})
	if err != nil {
		os.Remove(final)
		return store.ObjectInfo{}, fmt.Errorf("commit %s/%s: %w", bucket, key, err)
	}
	if e.crashAt("put:committed") {
		return store.ObjectInfo{}, errSimulatedCrash
	}

	// 5. The old version is now unreferenced.
	if oldBlob != "" {
		e.removeBlob(oldBlob)
	}
	return rec.info(bucket, key), nil
}

// ensureShard creates objects/ab and objects/ab/cd durably.
func (e *Engine) ensureShard(id string) error {
	outer := filepath.Join(e.objDir, id[:2])
	if err := mkdirDurable(outer); err != nil {
		return err
	}
	return mkdirDurable(filepath.Join(outer, id[2:4]))
}

// Get implements store.Store.
func (e *Engine) Get(ctx context.Context, bucket, key string, rng *store.Range) (io.ReadCloser, store.ObjectInfo, error) {
	if err := validateObjectArgs(bucket, key); err != nil {
		return nil, store.ObjectInfo{}, err
	}
	// The blob can vanish between reading metadata and opening the file if a
	// concurrent overwrite or delete commits in that gap. Looking up again
	// then finds the new version (or ErrNoSuchKey). Unix keeps an already
	// open file readable after unlink, so once opened, the read is safe.
	const attempts = 3
	for range attempts {
		rec, err := e.lookup(bucket, key)
		if err != nil {
			return nil, store.ObjectInfo{}, err
		}
		info := rec.info(bucket, key)
		f, err := os.Open(e.blobPath(rec.Blob))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, store.ObjectInfo{}, fmt.Errorf("open blob: %w", err)
		}
		if rng == nil {
			return &verifyingReader{f: f, h: sha256.New(), want: rec.ETag, size: rec.Size}, info, nil
		}
		off, n, err := rng.Resolve(rec.Size)
		if err != nil {
			f.Close()
			return nil, info, err
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			f.Close()
			return nil, store.ObjectInfo{}, fmt.Errorf("seek blob: %w", err)
		}
		return limitedFile{Reader: io.LimitReader(f, n), f: f}, info, nil
	}
	return nil, store.ObjectInfo{}, fmt.Errorf("%w: %s/%s: metadata present but blob file missing", store.ErrDataCorrupt, bucket, key)
}

// Head implements store.Store.
func (e *Engine) Head(ctx context.Context, bucket, key string) (store.ObjectInfo, error) {
	if err := validateObjectArgs(bucket, key); err != nil {
		return store.ObjectInfo{}, err
	}
	rec, err := e.lookup(bucket, key)
	if err != nil {
		return store.ObjectInfo{}, err
	}
	return rec.info(bucket, key), nil
}

// Delete implements store.Store. The metadata row goes first; the blob file
// is removed after the commit (or by the janitor after a crash).
func (e *Engine) Delete(ctx context.Context, bucket, key string) error {
	if err := validateObjectArgs(bucket, key); err != nil {
		return err
	}
	var blob string
	err := e.db.Update(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, bucket)
		if err != nil {
			return err
		}
		v := objs.Get([]byte(key))
		if v == nil {
			return nil // idempotent
		}
		rec, err := decodeObject(v)
		if err != nil {
			return err
		}
		blob = rec.Blob
		if err := objs.Delete([]byte(key)); err != nil {
			return err
		}
		return tx.Bucket(bktBlobs).Delete([]byte(blob))
	})
	if err != nil {
		return fmt.Errorf("delete %s/%s: %w", bucket, key, err)
	}
	if blob == "" {
		return nil
	}
	if e.crashAt("delete:committed") {
		return errSimulatedCrash
	}
	e.removeBlob(blob)
	return nil
}

func (e *Engine) lookup(bucket, key string) (objectRecord, error) {
	var rec objectRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, bucket)
		if err != nil {
			return err
		}
		v := objs.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("%w: %s/%s", store.ErrNoSuchKey, bucket, key)
		}
		rec, err = decodeObject(v)
		return err
	})
	return rec, err
}

func validateObjectArgs(bucket, key string) error {
	if err := store.ValidateBucketName(bucket); err != nil {
		return err
	}
	return store.ValidateKey(key)
}

// verifyingReader hashes a full-object read and, at EOF, reports
// store.ErrDataCorrupt instead of io.EOF if the bytes on disk no longer
// match the size and SHA-256 recorded at write time (bit rot, truncation,
// tampering).
type verifyingReader struct {
	f    *os.File
	h    hash.Hash
	want string
	size int64
	read int64
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.f.Read(p)
	v.h.Write(p[:n])
	v.read += int64(n)
	if errors.Is(err, io.EOF) {
		if v.read != v.size {
			return n, fmt.Errorf("%w: read %d bytes, expected %d", store.ErrDataCorrupt, v.read, v.size)
		}
		if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
			return n, fmt.Errorf("%w: sha256 %s, expected %s", store.ErrDataCorrupt, got, v.want)
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.f.Close() }

type limitedFile struct {
	io.Reader
	f *os.File
}

func (l limitedFile) Close() error { return l.f.Close() }
