package engine

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Bucket usage and quotas
//
// bucketRecord.UsedBytes/Objects are updated by adjustUsage inside the same
// transaction that adds or removes an object record (Put, Delete, restore
// from trash), so they can never drift, even across crashes. Trash does
// not count: a delete frees its bytes from the quota at once.

// adjustUsage adds dBytes and dObjects to a bucket's usage. When the
// change grows the bucket (dBytes > 0) and enforce is set, it refuses with
// ErrQuotaExceeded if the result would pass the quota. Shrinking is always
// allowed, even over quota (after the quota was lowered).
func adjustUsage(tx *bbolt.Tx, bucket string, dBytes, dObjects int64, enforce bool) error {
	buckets := tx.Bucket(bktBuckets)
	v := buckets.Get([]byte(bucket))
	if v == nil {
		return fmt.Errorf("%w: %s", store.ErrNoSuchBucket, bucket)
	}
	var r bucketRecord
	if err := json.Unmarshal(v, &r); err != nil {
		return fmt.Errorf("decode bucket %s: %w", bucket, err)
	}
	if enforce && dBytes > 0 && r.QuotaBytes > 0 && r.UsedBytes+dBytes > r.QuotaBytes {
		return quotaError(bucket, r, dBytes)
	}
	r.UsedBytes += dBytes
	r.Objects += dObjects
	val, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return buckets.Put([]byte(bucket), val)
}

func quotaError(bucket string, r bucketRecord, need int64) error {
	return fmt.Errorf("%w: %s uses %s of its %s quota; this needs %s more",
		store.ErrQuotaExceeded, bucket, human(r.UsedBytes), human(r.QuotaBytes), human(need))
}

// quotaRoom returns how many more bytes an upload replacing an object of
// oldSize may write before the bucket passes its quota, or -1 for no
// quota. A fast pre-check only: the commit decides.
func quotaRoom(tx *bbolt.Tx, bucket string, oldSize int64) (int64, bucketRecord, error) {
	var r bucketRecord
	v := tx.Bucket(bktBuckets).Get([]byte(bucket))
	if v == nil {
		return 0, r, fmt.Errorf("%w: %s", store.ErrNoSuchBucket, bucket)
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return 0, r, err
	}
	if r.QuotaBytes <= 0 {
		return -1, r, nil
	}
	return max(r.QuotaBytes-r.UsedBytes+oldSize, 0), r, nil
}

// quotaGuard fails a streaming write once it passes room bytes, so an
// upload of unknown size can't fill the disk on its way to being refused.
type quotaGuard struct {
	bucket  string
	rec     bucketRecord
	oldSize int64 // size of the object being replaced, if any
	room    int64 // -1: no quota
	written int64
}

func (g *quotaGuard) Write(p []byte) (int, error) {
	g.written += int64(len(p))
	if g.room >= 0 && g.written > g.room {
		return 0, quotaError(g.bucket, g.rec, g.written-g.oldSize)
	}
	return len(p), nil
}

// recountUsage sets every bucket's usage from its object records. It is
// the format 2 to 3 migration step.
func recountUsage(tx *bbolt.Tx) error {
	buckets := tx.Bucket(bktBuckets)
	type usage struct{ bytes, objects int64 }
	counts := map[string]usage{}
	err := buckets.ForEach(func(name, _ []byte) error {
		objs := tx.Bucket(bktObjects).Bucket(name)
		if objs == nil {
			return nil
		}
		var u usage
		if err := objs.ForEach(func(k, v []byte) error {
			rec, err := decodeObject(v)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", name, k, err)
			}
			u.bytes += rec.Size
			u.objects++
			return nil
		}); err != nil {
			return err
		}
		counts[string(name)] = u
		return nil
	})
	if err != nil {
		return err
	}
	for name, u := range counts {
		var r bucketRecord
		if err := json.Unmarshal(buckets.Get([]byte(name)), &r); err != nil {
			return err
		}
		r.UsedBytes, r.Objects = u.bytes, u.objects
		val, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := buckets.Put([]byte(name), val); err != nil {
			return err
		}
	}
	return nil
}
