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
	bktLinks   = []byte("links")   // token -> store.LinkRecord
	bktLinkIdx = []byte("linkidx") // bucket NUL key NUL id -> token
)

var _ store.Links = (*Engine)(nil)

// linkIdxPrefix is the index prefix for one object's links, or for a whole
// bucket when key is empty. Bucket names and keys cannot contain NUL, so
// prefixes never collide ("a" + key "b/c" vs bucket "ab").
func linkIdxPrefix(bucket, key string) []byte {
	p := []byte(bucket + "\x00")
	if key != "" {
		p = append(p, key+"\x00"...)
	}
	return p
}

// CreateLink implements store.Links.
func (e *Engine) CreateLink(ctx context.Context, rec store.LinkRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		// The object must exist now; deleting it later removes the link.
		objs, err := objectsOf(tx, rec.Bucket)
		if err != nil {
			return err
		}
		if objs.Get([]byte(rec.Key)) == nil {
			return fmt.Errorf("%w: %s/%s", store.ErrNoSuchKey, rec.Bucket, rec.Key)
		}
		links := tx.Bucket(bktLinks)
		if links.Get([]byte(rec.Token)) != nil {
			return fmt.Errorf("share link token collision") // 128-bit tokens: never happens
		}
		if err := links.Put([]byte(rec.Token), val); err != nil {
			return err
		}
		idx := append(linkIdxPrefix(rec.Bucket, rec.Key), rec.ID...)
		return tx.Bucket(bktLinkIdx).Put(idx, []byte(rec.Token))
	})
}

// LinkByToken implements store.Links.
func (e *Engine) LinkByToken(ctx context.Context, token string) (store.LinkRecord, error) {
	var rec store.LinkRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bktLinks).Get([]byte(token))
		if v == nil {
			return store.ErrNoSuchLink
		}
		return json.Unmarshal(v, &rec)
	})
	return rec, err
}

// LinkByID implements store.Links. Links are few; a scan is fine.
func (e *Engine) LinkByID(ctx context.Context, id string) (store.LinkRecord, error) {
	var found *store.LinkRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktLinks).ForEach(func(_, v []byte) error {
			var rec store.LinkRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.ID == id {
				found = &rec
			}
			return nil
		})
	})
	if err != nil {
		return store.LinkRecord{}, err
	}
	if found == nil {
		return store.LinkRecord{}, fmt.Errorf("%w: %s", store.ErrNoSuchLink, id)
	}
	return *found, nil
}

// ListLinks implements store.Links.
func (e *Engine) ListLinks(ctx context.Context, bucket, key string) ([]store.LinkRecord, error) {
	var out []store.LinkRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		links := tx.Bucket(bktLinks)
		add := func(token []byte) error {
			var rec store.LinkRecord
			if err := json.Unmarshal(links.Get(token), &rec); err != nil {
				return fmt.Errorf("decode share link: %w", err)
			}
			out = append(out, rec)
			return nil
		}
		if bucket == "" {
			return links.ForEach(func(k, _ []byte) error { return add(k) })
		}
		prefix := linkIdxPrefix(bucket, key)
		c := tx.Bucket(bktLinkIdx).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := add(v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b store.LinkRecord) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// DeleteLink implements store.Links.
func (e *Engine) DeleteLink(ctx context.Context, id string) error {
	rec, err := e.LinkByID(ctx, id)
	if err != nil {
		return err
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bktLinks).Delete([]byte(rec.Token)); err != nil {
			return err
		}
		return tx.Bucket(bktLinkIdx).Delete(append(linkIdxPrefix(rec.Bucket, rec.Key), rec.ID...))
	})
}

// deleteLinksWithPrefix removes every link whose index entry starts with
// prefix, inside the caller's transaction.
func deleteLinksWithPrefix(tx *bbolt.Tx, prefix []byte) error {
	idx := tx.Bucket(bktLinkIdx)
	links := tx.Bucket(bktLinks)
	var doomed [][2][]byte
	c := idx.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		doomed = append(doomed, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
	}
	for _, d := range doomed { // deleting while iterating confuses bbolt cursors
		if err := idx.Delete(d[0]); err != nil {
			return err
		}
		if err := links.Delete(d[1]); err != nil {
			return err
		}
	}
	return nil
}
