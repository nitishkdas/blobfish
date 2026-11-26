package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// CreateBucket implements store.Buckets.
func (e *Engine) CreateBucket(ctx context.Context, name string) (store.BucketInfo, error) {
	if err := store.ValidateBucketName(name); err != nil {
		return store.BucketInfo{}, err
	}
	info := store.BucketInfo{Name: name, Created: e.now()}
	val, err := json.Marshal(bucketRecord{Created: info.Created})
	if err != nil {
		return store.BucketInfo{}, err
	}
	err = e.db.Update(func(tx *bbolt.Tx) error {
		buckets := tx.Bucket(bktBuckets)
		if buckets.Get([]byte(name)) != nil {
			return fmt.Errorf("%w: %s", store.ErrBucketExists, name)
		}
		if err := buckets.Put([]byte(name), val); err != nil {
			return err
		}
		_, err := tx.Bucket(bktObjects).CreateBucket([]byte(name))
		return err
	})
	if err != nil {
		return store.BucketInfo{}, fmt.Errorf("create bucket: %w", err)
	}
	return info, nil
}

// DeleteBucket implements store.Buckets. Only empty buckets can be deleted,
// so no blob files need cleaning up.
func (e *Engine) DeleteBucket(ctx context.Context, name string) error {
	if err := store.ValidateBucketName(name); err != nil {
		return err
	}
	err := e.db.Update(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, name)
		if err != nil {
			return err
		}
		if k, _ := objs.Cursor().First(); k != nil {
			return fmt.Errorf("%w: %s", store.ErrBucketNotEmpty, name)
		}
		// Deleting the bucket must not quietly destroy what its trash
		// still holds; emptying the trash is a separate, explicit step.
		if n := trashCount(tx, name); n > 0 {
			return fmt.Errorf("%w: %s has %d object(s) in trash; empty its trash first", store.ErrBucketNotEmpty, name, n)
		}
		if trashOf(tx, name) != nil {
			if err := tx.Bucket(bktTrash).DeleteBucket([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket(bktObjects).DeleteBucket([]byte(name)); err != nil {
			return err
		}
		// Links die with their objects, but sweep anyway so a re-created
		// bucket of the same name can never inherit old links.
		if err := deleteLinksWithPrefix(tx, linkIdxPrefix(name, "")); err != nil {
			return err
		}
		if err := deleteBucketUploadLinks(tx, name); err != nil {
			return err
		}
		return tx.Bucket(bktBuckets).Delete([]byte(name))
	})
	if err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}
	return nil
}

// ListBuckets implements store.Buckets, sorted by name.
func (e *Engine) ListBuckets(ctx context.Context) ([]store.BucketInfo, error) {
	var out []store.BucketInfo
	err := e.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktBuckets).ForEach(func(k, v []byte) error {
			var r bucketRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("decode bucket %s: %w", k, err)
			}
			out = append(out, r.info(string(k)))
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	return out, nil
}

// GetBucket implements store.Buckets.
func (e *Engine) GetBucket(ctx context.Context, name string) (store.BucketInfo, error) {
	if err := store.ValidateBucketName(name); err != nil {
		return store.BucketInfo{}, err
	}
	var r bucketRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bktBuckets).Get([]byte(name))
		if v == nil {
			return fmt.Errorf("%w: %s", store.ErrNoSuchBucket, name)
		}
		return json.Unmarshal(v, &r)
	})
	if err != nil {
		return store.BucketInfo{}, err
	}
	return r.info(name), nil
}

// SetBucketPublic implements store.Buckets.
func (e *Engine) SetBucketPublic(ctx context.Context, name string, public bool) (store.BucketInfo, error) {
	return e.updateBucket(name, func(r *bucketRecord) { r.Public = public })
}

// SetBucketCORS implements store.Buckets.
func (e *Engine) SetBucketCORS(ctx context.Context, name string, origins []string) (store.BucketInfo, error) {
	clean, err := store.NormalizeCORSOrigins(origins)
	if err != nil {
		return store.BucketInfo{}, err
	}
	return e.updateBucket(name, func(r *bucketRecord) { r.CORSOrigins = clean })
}

// SetBucketQuota implements store.Buckets.
func (e *Engine) SetBucketQuota(ctx context.Context, name string, quotaBytes int64) (store.BucketInfo, error) {
	if quotaBytes < 0 {
		return store.BucketInfo{}, fmt.Errorf("%w: %d; use 0 for no quota", store.ErrInvalidQuota, quotaBytes)
	}
	return e.updateBucket(name, func(r *bucketRecord) { r.QuotaBytes = quotaBytes })
}

// updateBucket changes one bucket's settings in a single transaction.
func (e *Engine) updateBucket(name string, change func(*bucketRecord)) (store.BucketInfo, error) {
	if err := store.ValidateBucketName(name); err != nil {
		return store.BucketInfo{}, err
	}
	var r bucketRecord
	err := e.db.Update(func(tx *bbolt.Tx) error {
		buckets := tx.Bucket(bktBuckets)
		v := buckets.Get([]byte(name))
		if v == nil {
			return fmt.Errorf("%w: %s", store.ErrNoSuchBucket, name)
		}
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		change(&r)
		val, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return buckets.Put([]byte(name), val)
	})
	if err != nil {
		return store.BucketInfo{}, fmt.Errorf("update bucket: %w", err)
	}
	return r.info(name), nil
}
