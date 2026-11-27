package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Problem kinds reported by Verify.
const (
	ProblemMetadata  = "metadata"  // meta.db structure or a record is damaged
	ProblemMissing   = "missing"   // the blob file is gone
	ProblemSize      = "size"      // the blob file has the wrong length
	ProblemChecksum  = "checksum"  // the bytes no longer match their SHA-256
	ProblemUnindexed = "unindexed" // the blob is not in the blob index, so the janitor would delete it
	ProblemIO        = "io"        // the blob file could not be read
)

// VerifyProblem is one thing wrong with the data.
type VerifyProblem struct {
	Kind   string
	Bucket string // empty for problems with meta.db itself
	Key    string
	Blob   string
	Detail string
	// InTrash: the object was deleted and is waiting in trash.
	InTrash bool
}

func (p VerifyProblem) String() string {
	where := "meta.db"
	if p.Bucket != "" {
		where = p.Bucket + "/" + p.Key
		if p.InTrash {
			where += " (in trash)"
		}
	}
	s := fmt.Sprintf("%-9s %s: %s", p.Kind, where, p.Detail)
	if p.Blob != "" {
		s += " (blob " + p.Blob + ")"
	}
	return s
}

// VerifyStats counts what Verify checked.
type VerifyStats struct {
	Buckets  int
	Objects  int
	Bytes    int64
	Problems int
	// OrphanBlobs are blob files no object uses: leftovers the janitor
	// removes at the next start. Not a problem; counted only when every
	// bucket is verified.
	OrphanBlobs int
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	Bucket string // verify only this bucket; empty = everything
	// Problem is called for each problem as it is found.
	Problem func(VerifyProblem)
	// Progress, if set, is called about every ProgressEvery with the
	// running totals.
	Progress      func(VerifyStats)
	ProgressEvery time.Duration
}

// verifyBatch is how many object records are read per metadata
// transaction before their blob files are checked.
const verifyBatch = 500

// Verify reads every object in the data dir at dir and checks it against
// the size and SHA-256 recorded when it was written. It also checks the
// structure of meta.db and that every object's blob is in the blob index.
//
// It never writes: meta.db is opened read-only and the janitor does not
// run. (Open would run it, and with a damaged index the janitor could
// delete the very files Verify should report.) The server must be stopped,
// since it holds meta.db's lock.
//
// The returned error is about Verify being unable to run; damage is
// reported through opts.Problem and counted in VerifyStats.Problems.
func Verify(ctx context.Context, dir string, opts VerifyOptions) (VerifyStats, error) {
	var st VerifyStats
	if opts.Problem == nil {
		opts.Problem = func(VerifyProblem) {}
	}
	report := func(p VerifyProblem) {
		st.Problems++
		opts.Problem(p)
	}
	lastProgress := time.Now()
	progress := func() {
		if opts.Progress != nil && time.Since(lastProgress) >= opts.ProgressEvery {
			lastProgress = time.Now()
			opts.Progress(st)
		}
	}

	path := filepath.Join(dir, "meta.db")
	if _, err := os.Stat(path); err != nil {
		return st, fmt.Errorf("no BlobFish data in %s: %w", dir, err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return st, fmt.Errorf("open meta.db read-only (is the server running? stop it first): %w", err)
	}
	defer db.Close()
	if err := checkVerifiableFormat(db); err != nil {
		return st, err
	}

	// bbolt's own consistency check: page structure, free list, ordering.
	err = db.View(func(tx *bbolt.Tx) error {
		for err := range tx.Check() {
			report(VerifyProblem{Kind: ProblemMetadata, Detail: err.Error()})
		}
		return nil
	})
	if err != nil {
		return st, err
	}

	buckets, err := verifyBucketNames(db, opts.Bucket)
	if err != nil {
		return st, err
	}
	e := &Engine{objDir: filepath.Join(dir, "objects")} // only for blobPath
	for _, bucket := range buckets {
		st.Buckets++
		var liveBytes, liveObjects int64 // to check the bucket's usage counters
		countable := true                // false once a record can't be read
		// Live objects, then the bucket's trash: trashed objects can still
		// be restored, so their bytes matter too.
		for _, inTrash := range []bool{false, true} {
			after := []byte(nil)
			for {
				if err := ctx.Err(); err != nil {
					return st, err
				}
				batch, err := readVerifyBatch(db, bucket, inTrash, after)
				if err != nil {
					return st, err
				}
				for _, it := range batch {
					if err := ctx.Err(); err != nil {
						return st, err
					}
					st.Objects++
					p := VerifyProblem{Bucket: bucket, Key: it.key, Blob: it.rec.Blob, InTrash: inTrash}
					switch {
					case it.decodeErr != nil:
						p.Kind, p.Detail = ProblemMetadata, "unreadable object record: "+it.decodeErr.Error()
						report(p)
						countable = countable && inTrash
						continue
					case !inTrash:
						liveBytes += it.rec.Size
						liveObjects++
					}
					if !it.indexed {
						p.Kind, p.Detail = ProblemUnindexed, "blob is missing from the blob index; starting the server would delete it"
						report(p)
						// Still check the bytes: they may be fine and worth saving.
					}
					n, kind, detail := checkBlob(e.blobPath(it.rec.Blob), it.rec.Size, it.rec.ETag)
					st.Bytes += n
					if kind != "" {
						p.Kind, p.Detail = kind, detail
						report(p)
					}
					progress()
				}
				if len(batch) < verifyBatch {
					break
				}
				after = []byte(batch[len(batch)-1].cursor)
			}
		}
		if p, ok := checkUsage(db, bucket, liveBytes, liveObjects); countable && !ok {
			report(p)
		}
	}

	if opts.Bucket == "" {
		err = db.View(func(tx *bbolt.Tx) error {
			for _, d := range checkUserEmails(tx) {
				report(VerifyProblem{Kind: ProblemMetadata, Detail: d})
			}
			return nil
		})
		if err != nil {
			return st, err
		}
		st.OrphanBlobs, err = countOrphanBlobs(ctx, db, e.objDir)
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

// checkUsage compares a bucket's usage counters (which quotas rely on)
// with what its object records add up to.
func checkUsage(db *bbolt.DB, bucket string, bytes, objects int64) (VerifyProblem, bool) {
	var r bucketRecord
	err := db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bktBuckets).Get([]byte(bucket)), &r)
	})
	switch {
	case err != nil:
		return VerifyProblem{Kind: ProblemMetadata, Detail: fmt.Sprintf("bucket %s: unreadable record: %v", bucket, err)}, false
	case r.UsedBytes != bytes || r.Objects != objects:
		return VerifyProblem{Kind: ProblemMetadata, Detail: fmt.Sprintf(
			"bucket %s: usage counters say %d bytes in %d objects, but its objects add up to %d bytes in %d",
			bucket, r.UsedBytes, r.Objects, bytes, objects)}, false
	}
	return VerifyProblem{}, true
}

// checkVerifiableFormat refuses data this binary cannot read as-is: newer
// data, or older data that only the server's startup migration can upgrade
// (Verify never writes, so it cannot migrate).
func checkVerifiableFormat(db *bbolt.DB) error {
	return db.View(func(tx *bbolt.Tx) error {
		v, err := readFormat(tx)
		if err != nil {
			return err
		}
		switch v = max(v, 1); {
		case v > FormatVersion:
			return fmt.Errorf("%w: data format %d, this binary supports up to %d; install a newer blobfish",
				ErrFormatTooNew, v, FormatVersion)
		case v < FormatVersion:
			return fmt.Errorf("data format %d is older than this binary's %d: start the server once to upgrade it, then verify", v, FormatVersion)
		}
		return nil
	})
}

func verifyBucketNames(db *bbolt.DB, only string) ([]string, error) {
	var names []string
	err := db.View(func(tx *bbolt.Tx) error {
		bk := tx.Bucket(bktBuckets)
		if bk == nil {
			return nil
		}
		if only != "" {
			if bk.Get([]byte(only)) == nil {
				return fmt.Errorf("%w: %s", store.ErrNoSuchBucket, only)
			}
			names = []string{only}
			return nil
		}
		return bk.ForEach(func(k, _ []byte) error {
			names = append(names, string(k))
			return nil
		})
	})
	return names, err
}

type verifyItem struct {
	cursor    string // the bbolt key: object key, or trash ID
	key       string
	rec       objectRecord
	decodeErr error
	indexed   bool
}

// readVerifyBatch returns up to verifyBatch records after the bbolt key
// after, from the bucket's objects or its trash, each with whether its blob
// is in the index. Blob files are checked outside the transaction, so no
// read transaction stays open for long.
func readVerifyBatch(db *bbolt.DB, bucket string, inTrash bool, after []byte) ([]verifyItem, error) {
	var out []verifyItem
	err := db.View(func(tx *bbolt.Tx) error {
		var objs *bbolt.Bucket
		if inTrash {
			if tb := tx.Bucket(bktTrash); tb != nil {
				objs = tb.Bucket([]byte(bucket))
			}
		} else if o := tx.Bucket(bktObjects); o != nil {
			objs = o.Bucket([]byte(bucket))
		}
		if objs == nil {
			return nil // no objects (or no trash) yet
		}
		blobs := tx.Bucket(bktBlobs)
		c := objs.Cursor()
		k, v := c.First()
		if after != nil {
			k, v = c.Seek(after)
			if k != nil && bytes.Equal(k, after) {
				k, v = c.Next()
			}
		}
		for ; k != nil && len(out) < verifyBatch; k, v = c.Next() {
			it := verifyItem{cursor: string(k), key: string(k)}
			if inTrash {
				var tr trashRecord
				it.decodeErr = json.Unmarshal(v, &tr)
				it.key, it.rec = tr.Key, tr.Object
				if it.key == "" {
					it.key = "trash id " + string(k)
				}
			} else {
				it.rec, it.decodeErr = decodeObject(v)
			}
			if it.decodeErr == nil && blobs != nil {
				it.indexed = blobs.Get([]byte(it.rec.Blob)) != nil
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// checkBlob reads one blob file from disk and compares it with the size
// and SHA-256 recorded at write time. It returns the bytes read and, if
// something is wrong, a problem kind and detail.
func checkBlob(path string, size int64, etag string) (n int64, kind, detail string) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ProblemMissing, "blob file does not exist"
	}
	if err != nil {
		return 0, ProblemIO, err.Error()
	}
	defer f.Close()
	// Read from the disk, not from a cached copy of what was written.
	dropCache(f)
	h := sha256.New()
	n, err = io.Copy(h, f)
	dropCache(f) // and don't fill the cache with a whole-disk scan
	switch {
	case err != nil:
		return n, ProblemIO, err.Error()
	case n != size:
		return n, ProblemSize, fmt.Sprintf("file is %d bytes, expected %d", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != etag {
		return n, ProblemChecksum, fmt.Sprintf("sha256 is %s, expected %s", got, etag)
	}
	return n, "", ""
}

// countOrphanBlobs counts blob files whose ID is not in the index.
func countOrphanBlobs(ctx context.Context, db *bbolt.DB, objDir string) (int, error) {
	n := 0
	err := db.View(func(tx *bbolt.Tx) error {
		blobs := tx.Bucket(bktBlobs)
		return filepath.WalkDir(objDir, func(_ string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !d.IsDir() && (blobs == nil || blobs.Get([]byte(d.Name())) == nil) {
				n++
			}
			return nil
		})
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil // no objects dir yet
	}
	return n, err
}
