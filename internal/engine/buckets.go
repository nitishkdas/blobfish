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
		if err := tx.Bucket(bktObjects).DeleteBucket([]byte(name)); err != nil {
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
			out = append(out, store.BucketInfo{Name: string(k), Created: r.Created})
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	return out, nil
}
