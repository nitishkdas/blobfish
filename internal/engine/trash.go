package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Trash
//
// With trash on (Options.TrashRetention > 0), Delete moves the object's
// record from objects/<bucket> to trash/<bucket>/<trash id> in the same
// transaction that marks its share links as trashed. Nothing happens to
// the blob file: it stays in the blob index, so the janitor keeps it and
// backups include it. Restore moves the record back and re-enables the
// links. Purging (expiry, one item, or a whole bucket's trash) deletes the
// record, the blob index entry and the links in one transaction and the
// blob file after it, the same order as a delete without trash. A crash
// between the two leaves an orphan file the janitor removes.
//
// Trash is not versioning: overwrites never go to trash, only deletes.

var bktTrash = []byte("trash") // nested bucket per BlobFish bucket: trash id -> trashRecord

var _ store.Trash = (*Engine)(nil)

type trashRecord struct {
	Key     string       `json:"key"`
	Object  objectRecord `json:"obj"`
	Deleted time.Time    `json:"deleted"`
	// PurgeAt is fixed at delete time, so changing the retention setting
	// only affects later deletes.
	PurgeAt time.Time `json:"purgeAt"`
}

func (r trashRecord) item(bucket, id string) store.TrashItem {
	return store.TrashItem{ID: id, Object: r.Object.info(bucket, r.Key), Deleted: r.Deleted, PurgeAt: r.PurgeAt}
}

// Trash IDs are 16 hex digits of the delete time in nanoseconds plus 8
// random ones, so they sort by delete time and never collide.
var trashIDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

func newTrashID(t time.Time) string {
	var r [4]byte
	rand.Read(r[:])
	return fmt.Sprintf("%016x%s", uint64(t.UnixNano()), hex.EncodeToString(r[:]))
}

// TrashRetention implements store.Trash.
func (e *Engine) TrashRetention() time.Duration { return e.trashRetention }

// trashOf returns a bucket's trash, or nil if it has never had any.
func trashOf(tx *bbolt.Tx, bucket string) *bbolt.Bucket {
	return tx.Bucket(bktTrash).Bucket([]byte(bucket))
}

// moveToTrash is Delete's work when trash is on: inside the caller's
// transaction, it moves the record to trash and marks the object's links.
func (e *Engine) moveToTrash(tx *bbolt.Tx, bucket, key string, rec objectRecord) error {
	tb, err := tx.Bucket(bktTrash).CreateBucketIfNotExists([]byte(bucket))
	if err != nil {
		return err
	}
	now := e.now()
	id := newTrashID(now)
	val, err := json.Marshal(trashRecord{Key: key, Object: rec, Deleted: now, PurgeAt: now.Add(e.trashRetention)})
	if err != nil {
		return err
	}
	if err := tb.Put([]byte(id), val); err != nil {
		return err
	}
	return updateLinks(tx, bucket, key, func(l *store.LinkRecord) bool {
		if l.Trashed != "" {
			return false // already belongs to an earlier deletion
		}
		l.Trashed = id
		return true
	})
}

// updateLinks rewrites the links of one object for which fn returns true.
func updateLinks(tx *bbolt.Tx, bucket, key string, fn func(*store.LinkRecord) bool) error {
	links := tx.Bucket(bktLinks)
	prefix := linkIdxPrefix(bucket, key)
	type change struct{ token, val []byte }
	var changes []change
	c := tx.Bucket(bktLinkIdx).Cursor()
	for k, token := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, token = c.Next() {
		var l store.LinkRecord
		if err := json.Unmarshal(links.Get(token), &l); err != nil {
			return fmt.Errorf("decode share link: %w", err)
		}
		if !fn(&l) {
			continue
		}
		val, err := json.Marshal(l)
		if err != nil {
			return err
		}
		changes = append(changes, change{bytes.Clone(token), val})
	}
	for _, ch := range changes { // writing while iterating confuses bbolt cursors
		if err := links.Put(ch.token, ch.val); err != nil {
			return err
		}
	}
	return nil
}

// deleteTrashedLinks removes the links of one object that belong to the
// trash entry trashID; "" means the live object's links.
func deleteTrashedLinks(tx *bbolt.Tx, bucket, key, trashID string) error {
	links, idx := tx.Bucket(bktLinks), tx.Bucket(bktLinkIdx)
	prefix := linkIdxPrefix(bucket, key)
	var doomed [][2][]byte
	c := idx.Cursor()
	for k, token := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, token = c.Next() {
		var l store.LinkRecord
		if err := json.Unmarshal(links.Get(token), &l); err != nil {
			return fmt.Errorf("decode share link: %w", err)
		}
		if l.Trashed == trashID {
			doomed = append(doomed, [2][]byte{bytes.Clone(k), bytes.Clone(token)})
		}
	}
	for _, d := range doomed {
		if err := idx.Delete(d[0]); err != nil {
			return err
		}
		if err := links.Delete(d[1]); err != nil {
			return err
		}
	}
	return nil
}

// ListTrash implements store.Trash. Newest first; the cursor is the last
// ID of the previous page.
func (e *Engine) ListTrash(ctx context.Context, bucket string, opts store.TrashListOptions) (store.TrashList, error) {
	var out store.TrashList
	if err := store.ValidateBucketName(bucket); err != nil {
		return out, err
	}
	if opts.Cursor != "" && !trashIDPattern.MatchString(opts.Cursor) {
		return out, fmt.Errorf("%w: %q", store.ErrInvalidCursor, opts.Cursor)
	}
	limit := opts.Limit
	if limit <= 0 || limit > store.MaxListLimit {
		limit = store.DefaultListLimit
	}
	err := e.db.View(func(tx *bbolt.Tx) error {
		if _, err := objectsOf(tx, bucket); err != nil {
			return err
		}
		tb := trashOf(tx, bucket)
		if tb == nil {
			return nil
		}
		c := tb.Cursor()
		var k, v []byte
		if opts.Cursor == "" {
			k, v = c.Last()
		} else if k, _ = c.Seek([]byte(opts.Cursor)); k == nil {
			// Every ID is before the cursor.
			k, v = c.Last()
		} else {
			// Seek landed on the first ID at or after the cursor; the page
			// continues with the one just before it.
			k, v = c.Prev()
		}
		for ; k != nil; k, v = c.Prev() {
			if len(out.Items) == limit {
				out.NextCursor = out.Items[len(out.Items)-1].ID
				break
			}
			var r trashRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("decode trash record %s: %w", k, err)
			}
			out.Items = append(out.Items, r.item(bucket, string(k)))
		}
		return nil
	})
	return out, err
}

// RestoreTrash implements store.Trash.
func (e *Engine) RestoreTrash(ctx context.Context, bucket, id string) (store.ObjectInfo, error) {
	var info store.ObjectInfo
	if err := store.ValidateBucketName(bucket); err != nil {
		return info, err
	}
	var queued bool
	err := e.db.Update(func(tx *bbolt.Tx) error {
		objs, err := objectsOf(tx, bucket)
		if err != nil {
			return err
		}
		tb, r, err := getTrash(tx, bucket, id)
		if err != nil {
			return err
		}
		if objs.Get([]byte(r.Key)) != nil {
			return fmt.Errorf("%w: %s/%s; delete or rename it first", store.ErrKeyExists, bucket, r.Key)
		}
		if err := adjustUsage(tx, bucket, r.Object.Size, 1, true); err != nil {
			return err
		}
		if queued, err = e.queueEvent(tx, objectEvent(store.EventObjectRestored, bucket, r.Key, r.Object), ""); err != nil {
			return err
		}
		val, err := json.Marshal(r.Object)
		if err != nil {
			return err
		}
		if err := objs.Put([]byte(r.Key), val); err != nil {
			return err
		}
		if err := tb.Delete([]byte(id)); err != nil {
			return err
		}
		info = r.Object.info(bucket, r.Key)
		return updateLinks(tx, bucket, r.Key, func(l *store.LinkRecord) bool {
			if l.Trashed != id {
				return false
			}
			l.Trashed = ""
			return true
		})
	})
	if err != nil {
		return info, fmt.Errorf("restore from trash: %w", err)
	}
	if queued {
		e.signalEvents()
	}
	return info, nil
}

func getTrash(tx *bbolt.Tx, bucket, id string) (*bbolt.Bucket, trashRecord, error) {
	var r trashRecord
	tb := trashOf(tx, bucket)
	if !trashIDPattern.MatchString(id) || tb == nil {
		return nil, r, fmt.Errorf("%w: %s", store.ErrNoSuchTrashItem, id)
	}
	v := tb.Get([]byte(id))
	if v == nil {
		return nil, r, fmt.Errorf("%w: %s", store.ErrNoSuchTrashItem, id)
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, r, fmt.Errorf("decode trash record %s: %w", id, err)
	}
	return tb, r, nil
}

// PurgeTrash implements store.Trash.
func (e *Engine) PurgeTrash(ctx context.Context, bucket, id string) error {
	if err := store.ValidateBucketName(bucket); err != nil {
		return err
	}
	_, err := e.purge(func(tx *bbolt.Tx) ([]purgeTarget, error) {
		if _, err := objectsOf(tx, bucket); err != nil {
			return nil, err
		}
		if id != "" {
			_, r, err := getTrash(tx, bucket, id)
			if err != nil {
				return nil, err
			}
			return []purgeTarget{{bucket, id, r}}, nil
		}
		var all []purgeTarget
		err := forEachTrash(tx, bucket, func(id string, r trashRecord) bool {
			all = append(all, purgeTarget{bucket, id, r})
			return true
		})
		return all, err
	})
	return err
}

// PurgeExpiredTrash deletes trashed objects whose time is up. The janitor
// runs it; it returns how many it purged.
func (e *Engine) PurgeExpiredTrash(ctx context.Context) (int, error) {
	now := e.now()
	return e.purge(func(tx *bbolt.Tx) ([]purgeTarget, error) {
		var due []purgeTarget
		err := tx.Bucket(bktTrash).ForEachBucket(func(name []byte) error {
			return forEachTrash(tx, string(name), func(id string, r trashRecord) bool {
				if !r.PurgeAt.After(now) {
					due = append(due, purgeTarget{string(name), id, r})
				}
				return len(due) < 1000 // bound one transaction; the rest go next run
			})
		})
		return due, err
	})
}

type purgeTarget struct {
	bucket, id string
	rec        trashRecord
}

// forEachTrash calls fn for a bucket's trash, oldest first, until fn
// returns false.
func forEachTrash(tx *bbolt.Tx, bucket string, fn func(id string, r trashRecord) bool) error {
	tb := trashOf(tx, bucket)
	if tb == nil {
		return nil
	}
	c := tb.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var r trashRecord
		if err := json.Unmarshal(v, &r); err != nil {
			return fmt.Errorf("decode trash record %s: %w", k, err)
		}
		if !fn(string(k), r) {
			break
		}
	}
	return nil
}

// purge deletes the chosen trash records, their blob index entries and
// links in one transaction, then the blob files.
func (e *Engine) purge(choose func(tx *bbolt.Tx) ([]purgeTarget, error)) (int, error) {
	var targets []purgeTarget
	err := e.db.Update(func(tx *bbolt.Tx) error {
		var err error
		if targets, err = choose(tx); err != nil {
			return err
		}
		blobs := tx.Bucket(bktBlobs)
		for _, t := range targets {
			if err := trashOf(tx, t.bucket).Delete([]byte(t.id)); err != nil {
				return err
			}
			if err := blobs.Delete([]byte(t.rec.Object.Blob)); err != nil {
				return err
			}
			if err := deleteTrashedLinks(tx, t.bucket, t.rec.Key, t.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("purge trash: %w", err)
	}
	if len(targets) > 0 && e.crashAt("purge:committed") {
		return 0, errSimulatedCrash
	}
	for _, t := range targets {
		e.removeBlob(t.rec.Object.Blob)
	}
	return len(targets), nil
}

// trashCount returns how many objects a bucket has in trash.
func trashCount(tx *bbolt.Tx, bucket string) int {
	if tb := trashOf(tx, bucket); tb != nil {
		return tb.Stats().KeyN
	}
	return 0
}
