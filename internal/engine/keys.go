package engine

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

var (
	bktKeys     = []byte("keys")     // token hash -> store.KeyRecord
	bktSettings = []byte("settings") // server-wide values
	setPresign  = []byte("presign_secret")
)

var _ store.Keys = (*Engine)(nil)

// PresignSecret returns the HMAC key for presigned URLs. It is generated
// once, on the first Open of a data dir, and kept in meta.db so URLs stay
// valid across restarts.
func (e *Engine) PresignSecret() []byte {
	return bytes.Clone(e.presignSecret)
}

// CreateKey implements store.Keys.
func (e *Engine) CreateKey(ctx context.Context, rec store.KeyRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		keys := tx.Bucket(bktKeys)
		if keys.Get([]byte(rec.Hash)) != nil {
			return fmt.Errorf("API key hash collision") // 256-bit tokens: never happens
		}
		return keys.Put([]byte(rec.Hash), val)
	})
}

// KeyByHash implements store.Keys.
func (e *Engine) KeyByHash(ctx context.Context, hash string) (store.KeyRecord, error) {
	var rec store.KeyRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bktKeys).Get([]byte(hash))
		if v == nil {
			return store.ErrNoSuchAPIKey
		}
		return json.Unmarshal(v, &rec)
	})
	return rec, err
}

// ListKeys implements store.Keys.
func (e *Engine) ListKeys(ctx context.Context) ([]store.KeyRecord, error) {
	var out []store.KeyRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktKeys).ForEach(func(_, v []byte) error {
			var rec store.KeyRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode API key: %w", err)
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// bbolt orders by hash, which is random; oldest first reads better.
	slices.SortFunc(out, func(a, b store.KeyRecord) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// DeleteKey implements store.Keys. Keys are few, so a scan is fine.
func (e *Engine) DeleteKey(ctx context.Context, id string) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		keys := tx.Bucket(bktKeys)
		c := keys.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec store.KeyRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode API key: %w", err)
			}
			if rec.ID == id {
				return keys.Delete(k)
			}
		}
		return fmt.Errorf("%w: %s", store.ErrNoSuchAPIKey, id)
	})
}
