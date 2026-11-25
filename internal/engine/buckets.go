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
		// Links die with their objects, but sweep anyway so a re-created
		// bucket of the same name can never inherit old links.
		if err := deleteLinksWithPrefix(tx, linkIdxPrefix(name, "")); err != nil {
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
			out = append(out, store.BucketInfo{Name: string(k), Created: r.Created, Public: r.Public})
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
	return store.BucketInfo{Name: name, Created: r.Created, Public: r.Public}, nil
}

// SetBucketPublic implements store.Buckets.
func (e *Engine) SetBucketPublic(ctx context.Context, name string, public bool) (store.BucketInfo, error) {
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
		r.Public = public
		val, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return buckets.Put([]byte(name), val)
	})
	if err != nil {
		return store.BucketInfo{}, fmt.Errorf("set bucket visibility: %w", err)
	}
	return store.BucketInfo{Name: name, Created: r.Created, Public: r.Public}, nil
}
