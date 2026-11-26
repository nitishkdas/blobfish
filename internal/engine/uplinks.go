package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Upload links live in "uplinks" (token -> store.UploadLinkRecord). They
// are few, so listing and lookup by ID scan; uploads look up by token.
// Deleting a bucket deletes its upload links in the same transaction, so a
// re-created bucket of that name never inherits them.

var bktUplinks = []byte("uplinks")

var _ store.UploadLinks = (*Engine)(nil)

// CreateUploadLink implements store.UploadLinks.
func (e *Engine) CreateUploadLink(ctx context.Context, rec store.UploadLinkRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		if _, err := objectsOf(tx, rec.Bucket); err != nil {
			return err
		}
		ul := tx.Bucket(bktUplinks)
		if ul.Get([]byte(rec.Token)) != nil {
			return fmt.Errorf("upload link token collision") // 128-bit tokens: never happens
		}
		return ul.Put([]byte(rec.Token), val)
	})
}

// UploadLinkByToken implements store.UploadLinks.
func (e *Engine) UploadLinkByToken(ctx context.Context, token string) (store.UploadLinkRecord, error) {
	var rec store.UploadLinkRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		var err error
		rec, err = getUploadLink(tx, token)
		return err
	})
	return rec, err
}

func getUploadLink(tx *bbolt.Tx, token string) (store.UploadLinkRecord, error) {
	var rec store.UploadLinkRecord
	v := tx.Bucket(bktUplinks).Get([]byte(token))
	if v == nil {
		return rec, store.ErrNoSuchLink
	}
	if err := json.Unmarshal(v, &rec); err != nil {
		return rec, fmt.Errorf("decode upload link: %w", err)
	}
	return rec, nil
}

func forEachUploadLink(tx *bbolt.Tx, fn func(store.UploadLinkRecord) error) error {
	return tx.Bucket(bktUplinks).ForEach(func(_, v []byte) error {
		var rec store.UploadLinkRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("decode upload link: %w", err)
		}
		return fn(rec)
	})
}

// UploadLinkByID implements store.UploadLinks.
func (e *Engine) UploadLinkByID(ctx context.Context, id string) (store.UploadLinkRecord, error) {
	var found *store.UploadLinkRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		return forEachUploadLink(tx, func(rec store.UploadLinkRecord) error {
			if rec.ID == id {
				found = &rec
			}
			return nil
		})
	})
	if err != nil {
		return store.UploadLinkRecord{}, err
	}
	if found == nil {
		return store.UploadLinkRecord{}, fmt.Errorf("%w: %s", store.ErrNoSuchLink, id)
	}
	return *found, nil
}

// ListUploadLinks implements store.UploadLinks.
func (e *Engine) ListUploadLinks(ctx context.Context, bucket string) ([]store.UploadLinkRecord, error) {
	out := []store.UploadLinkRecord{}
	err := e.db.View(func(tx *bbolt.Tx) error {
		return forEachUploadLink(tx, func(rec store.UploadLinkRecord) error {
			if rec.Bucket == bucket {
				out = append(out, rec)
			}
			return nil
		})
	})
	slices.SortFunc(out, func(a, b store.UploadLinkRecord) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return out, err
}

// DeleteUploadLink implements store.UploadLinks.
func (e *Engine) DeleteUploadLink(ctx context.Context, id string) error {
	rec, err := e.UploadLinkByID(ctx, id)
	if err != nil {
		return err
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktUplinks).Delete([]byte(rec.Token))
	})
}

// ClaimUploadSlot implements store.UploadLinks.
func (e *Engine) ClaimUploadSlot(ctx context.Context, token string) (store.UploadLinkRecord, error) {
	var rec store.UploadLinkRecord
	err := e.updateUploadLink(token, func(r *store.UploadLinkRecord) error {
		if r.MaxFiles > 0 && r.Uploads >= r.MaxFiles {
			return fmt.Errorf("%w: all %d used", store.ErrUploadLimitReached, r.MaxFiles)
		}
		r.Uploads++
		rec = *r
		return nil
	})
	return rec, err
}

// ReleaseUploadSlot implements store.UploadLinks. A link deleted
// meanwhile is not an error.
func (e *Engine) ReleaseUploadSlot(ctx context.Context, token string) error {
	err := e.updateUploadLink(token, func(r *store.UploadLinkRecord) error {
		r.Uploads = max(r.Uploads-1, 0)
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNoSuchLink) {
		return err
	}
	return nil
}

func (e *Engine) updateUploadLink(token string, change func(*store.UploadLinkRecord) error) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		rec, err := getUploadLink(tx, token)
		if err != nil {
			return err
		}
		if err := change(&rec); err != nil {
			return err
		}
		val, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Bucket(bktUplinks).Put([]byte(token), val)
	})
}

// deleteBucketUploadLinks removes a bucket's upload links inside the
// caller's transaction.
func deleteBucketUploadLinks(tx *bbolt.Tx, bucket string) error {
	var doomed []string
	if err := forEachUploadLink(tx, func(rec store.UploadLinkRecord) error {
		if rec.Bucket == bucket {
			doomed = append(doomed, rec.Token)
		}
		return nil
	}); err != nil {
		return err
	}
	ul := tx.Bucket(bktUplinks)
	for _, tok := range doomed {
		if err := ul.Delete([]byte(tok)); err != nil {
			return err
		}
	}
	return nil
}
