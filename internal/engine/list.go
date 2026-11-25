package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// List implements store.Store.
//
// The cursor is the base64url-encoded key where the next page starts
// (inclusive). After a common prefix it points just past every key under
// that prefix, so a "directory" is never returned twice across pages.
func (e *Engine) List(ctx context.Context, bucket string, opts store.ListOptions) (store.ListResult, error) {
	if err := store.ValidateBucketName(bucket); err != nil {
		return store.ListResult{}, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = store.DefaultListLimit
	}
	limit = min(limit, store.MaxListLimit)

	prefix := []byte(opts.Prefix)
	start := prefix
	if opts.Cursor != "" {
		c, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if err != nil || !bytes.HasPrefix(c, prefix) {
			return store.ListResult{}, fmt.Errorf("%w: does not match this prefix", store.ErrInvalidCursor)
		}
		start = c
	}

	var res store.ListResult
	err := e.db.View(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, bucket)
		if err != nil {
			return err
		}
		c := objs.Cursor()
		n := 0
		for k, v := c.Seek(start); k != nil && bytes.HasPrefix(k, prefix); {
			if n == limit {
				res.NextCursor = base64.RawURLEncoding.EncodeToString(k)
				return nil
			}
			n++

			if opts.Delimiter != "" {
				rest := string(k[len(prefix):])
				if i := strings.Index(rest, opts.Delimiter); i >= 0 {
					cp := string(k[:len(prefix)+i+len(opts.Delimiter)])
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					next := prefixSuccessor([]byte(cp))
					if next == nil {
						return nil
					}
					k, v = c.Seek(next)
					continue
				}
			}

			rec, err := decodeObject(v)
			if err != nil {
				return err
			}
			res.Objects = append(res.Objects, rec.info(bucket, string(k)))
			k, v = c.Next()
		}
		return nil
	})
	if err != nil {
		return store.ListResult{}, fmt.Errorf("list %s: %w", bucket, err)
	}
	return res, nil
}

// prefixSuccessor returns the smallest key greater than every key that
// starts with p, or nil if there is none (p is all 0xff bytes).
func prefixSuccessor(p []byte) []byte {
	out := bytes.Clone(p)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
