package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Webhooks
//
// "webhooks" holds id -> store.WebhookRecord. "outbox" holds queued
// deliveries, keyed by an 8-byte big-endian sequence number, so they sort
// in the order they were queued.
//
// An event is queued by queueEvent inside the same transaction as the
// change it describes (Put commit, Delete, restore from trash): either both
// commit or neither does, so no event is lost to a crash. Delivery happens
// later, outside the engine (internal/webhook), at least once.

var (
	bktWebhooks = []byte("webhooks")
	bktOutbox   = []byte("outbox")
)

// MaxPendingPerWebhook caps queued deliveries per webhook, so a receiver
// that is down for days can't fill the disk. Events past it are counted in
// WebhookRecord.Dropped.
const MaxPendingPerWebhook = 10_000

// MaxWebhooksPerBucket keeps fan-out per write small.
const MaxWebhooksPerBucket = 10

var (
	_ store.Webhooks = (*Engine)(nil)
	_ store.Outbox   = (*Engine)(nil)
)

type delivery struct {
	WebhookID   string          `json:"webhook"`
	EventID     string          `json:"eventId"`
	EventType   string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Attempts    int             `json:"attempts"`
	NextAttempt time.Time       `json:"next"`
}

// eventPayload is the JSON body a webhook receives.
type eventPayload struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Time        time.Time `json:"time"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key,omitempty"`
	Size        *int64    `json:"size,omitempty"`
	ETag        string    `json:"etag,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	// Replaced: the upload overwrote an existing object.
	Replaced bool `json:"replaced,omitempty"`
	// Trash: the delete moved the object to trash (restorable).
	Trash bool `json:"trash,omitempty"`
}

func objectEvent(typ, bucket, key string, rec objectRecord) eventPayload {
	size := rec.Size
	return eventPayload{
		Type: typ, Bucket: bucket, Key: key, Size: &size, ETag: rec.ETag, ContentType: rec.ContentType,
	}
}

func newEventID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func seqKey(n uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, n)
}

func getWebhook(tx *bbolt.Tx, id string) (store.WebhookRecord, error) {
	var w store.WebhookRecord
	v := tx.Bucket(bktWebhooks).Get([]byte(id))
	if v == nil {
		return w, fmt.Errorf("%w: %s", store.ErrNoSuchWebhook, id)
	}
	if err := json.Unmarshal(v, &w); err != nil {
		return w, fmt.Errorf("decode webhook %s: %w", id, err)
	}
	return w, nil
}

func putWebhook(tx *bbolt.Tx, w store.WebhookRecord) error {
	val, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return tx.Bucket(bktWebhooks).Put([]byte(w.ID), val)
}

func forEachWebhook(tx *bbolt.Tx, fn func(store.WebhookRecord) error) error {
	return tx.Bucket(bktWebhooks).ForEach(func(_, v []byte) error {
		var w store.WebhookRecord
		if err := json.Unmarshal(v, &w); err != nil {
			return fmt.Errorf("decode webhook: %w", err)
		}
		return fn(w)
	})
}

// queueEvent queues ev for every webhook of its bucket that wants its type
// (or only for onlyWebhook, when set), inside the caller's transaction.
// It reports whether anything was queued, so the caller can wake the
// dispatcher after the commit.
func (e *Engine) queueEvent(tx *bbolt.Tx, ev eventPayload, onlyWebhook string) (bool, error) {
	var targets []store.WebhookRecord
	if err := forEachWebhook(tx, func(w store.WebhookRecord) error {
		if w.Bucket == ev.Bucket && (onlyWebhook == "" || w.ID == onlyWebhook) &&
			(ev.Type == store.EventPing || slices.Contains(w.Events, ev.Type)) {
			targets = append(targets, w)
		}
		return nil
	}); err != nil || len(targets) == 0 {
		return false, err
	}
	ev.ID, ev.Time = newEventID(), e.now()
	body, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	outbox := tx.Bucket(bktOutbox)
	queued := false
	for _, w := range targets {
		if w.Pending >= MaxPendingPerWebhook {
			w.Dropped++
		} else {
			seq, err := outbox.NextSequence()
			if err != nil {
				return false, err
			}
			val, err := json.Marshal(delivery{
				WebhookID: w.ID, EventID: ev.ID, EventType: ev.Type, Payload: body, NextAttempt: ev.Time,
			})
			if err != nil {
				return false, err
			}
			if err := outbox.Put(seqKey(seq), val); err != nil {
				return false, err
			}
			w.Pending++
			queued = true
		}
		if err := putWebhook(tx, w); err != nil {
			return false, err
		}
	}
	return queued, nil
}

// signalEvents wakes the dispatcher without ever blocking a write.
func (e *Engine) signalEvents() {
	select {
	case e.events <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// EventSignal implements store.Outbox.
func (e *Engine) EventSignal() <-chan struct{} { return e.events }

// CreateWebhook implements store.Webhooks.
func (e *Engine) CreateWebhook(ctx context.Context, w store.WebhookRecord) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		if _, err := objectsOf(tx, w.Bucket); err != nil {
			return err
		}
		n := 0
		if err := forEachWebhook(tx, func(o store.WebhookRecord) error {
			if o.Bucket == w.Bucket {
				n++
			}
			return nil
		}); err != nil {
			return err
		}
		if n >= MaxWebhooksPerBucket {
			return fmt.Errorf("%w: bucket %s already has %d, the maximum", store.ErrTooManyWebhooks, w.Bucket, n)
		}
		if tx.Bucket(bktWebhooks).Get([]byte(w.ID)) != nil {
			return fmt.Errorf("webhook id collision") // random 64-bit IDs: never happens
		}
		return putWebhook(tx, w)
	})
}

// WebhookByID implements store.Webhooks.
func (e *Engine) WebhookByID(ctx context.Context, id string) (store.WebhookRecord, error) {
	var w store.WebhookRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		var err error
		w, err = getWebhook(tx, id)
		return err
	})
	return w, err
}

// ListWebhooks implements store.Webhooks, oldest first.
func (e *Engine) ListWebhooks(ctx context.Context, bucket string) ([]store.WebhookRecord, error) {
	out := []store.WebhookRecord{}
	err := e.db.View(func(tx *bbolt.Tx) error {
		return forEachWebhook(tx, func(w store.WebhookRecord) error {
			if w.Bucket == bucket {
				out = append(out, w)
			}
			return nil
		})
	})
	slices.SortFunc(out, func(a, b store.WebhookRecord) int { return a.Created.Compare(b.Created) })
	return out, err
}

// DeleteWebhook implements store.Webhooks.
func (e *Engine) DeleteWebhook(ctx context.Context, id string) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		if _, err := getWebhook(tx, id); err != nil {
			return err
		}
		if err := deleteDeliveries(tx, func(d delivery) bool { return d.WebhookID == id }); err != nil {
			return err
		}
		return tx.Bucket(bktWebhooks).Delete([]byte(id))
	})
}

// deleteBucketWebhooks removes a bucket's webhooks and their queued
// events, inside the caller's transaction (DeleteBucket).
func deleteBucketWebhooks(tx *bbolt.Tx, bucket string) error {
	ids := map[string]bool{}
	if err := forEachWebhook(tx, func(w store.WebhookRecord) error {
		if w.Bucket == bucket {
			ids[w.ID] = true
		}
		return nil
	}); err != nil || len(ids) == 0 {
		return err
	}
	if err := deleteDeliveries(tx, func(d delivery) bool { return ids[d.WebhookID] }); err != nil {
		return err
	}
	for id := range ids {
		if err := tx.Bucket(bktWebhooks).Delete([]byte(id)); err != nil {
			return err
		}
	}
	return nil
}

func deleteDeliveries(tx *bbolt.Tx, match func(delivery) bool) error {
	outbox := tx.Bucket(bktOutbox)
	var doomed [][]byte
	if err := outbox.ForEach(func(k, v []byte) error {
		var d delivery
		if err := json.Unmarshal(v, &d); err != nil {
			return fmt.Errorf("decode delivery: %w", err)
		}
		if match(d) {
			doomed = append(doomed, append([]byte(nil), k...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range doomed {
		if err := outbox.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// QueueTestEvent implements store.Webhooks.
func (e *Engine) QueueTestEvent(ctx context.Context, id string) error {
	var queued bool
	err := e.db.Update(func(tx *bbolt.Tx) error {
		w, err := getWebhook(tx, id)
		if err != nil {
			return err
		}
		queued, err = e.queueEvent(tx, eventPayload{Type: store.EventPing, Bucket: w.Bucket}, id)
		return err
	})
	if queued {
		e.signalEvents()
	}
	return err
}

// DueDeliveries implements store.Outbox.
func (e *Engine) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]store.Delivery, error) {
	var out []store.Delivery
	err := e.db.View(func(tx *bbolt.Tx) error {
		hooks := map[string]store.WebhookRecord{}
		c := tx.Bucket(bktOutbox).Cursor()
		for k, v := c.First(); k != nil && len(out) < limit; k, v = c.Next() {
			var d delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return fmt.Errorf("decode delivery: %w", err)
			}
			if d.NextAttempt.After(now) {
				continue
			}
			w, ok := hooks[d.WebhookID]
			if !ok {
				var err error
				if w, err = getWebhook(tx, d.WebhookID); err != nil {
					continue // deleted meanwhile; its deliveries go with it
				}
				hooks[d.WebhookID] = w
			}
			if w.RetryAfter.After(now) {
				continue // the receiver failed recently; wait for its backoff
			}
			out = append(out, store.Delivery{
				Seq: binary.BigEndian.Uint64(k), WebhookID: w.ID, URL: w.URL, Secret: w.Secret,
				AllowPrivate: w.AllowPrivate, EventID: d.EventID, EventType: d.EventType,
				Payload: d.Payload, Attempts: d.Attempts,
			})
		}
		return nil
	})
	return out, err
}

// DeliverySucceeded implements store.Outbox.
func (e *Engine) DeliverySucceeded(ctx context.Context, seq uint64, at time.Time) error {
	return e.finishDelivery(seq, func(w *store.WebhookRecord) {
		w.LastSuccess, w.RetryAfter = at, time.Time{}
	})
}

// DeliveryFailed implements store.Outbox.
func (e *Engine) DeliveryFailed(ctx context.Context, seq uint64, at time.Time, problem string, retryAt time.Time) error {
	if retryAt.IsZero() {
		return e.finishDelivery(seq, func(w *store.WebhookRecord) {
			w.LastFailure, w.LastError, w.RetryAfter = at, problem, time.Time{}
			w.GaveUp++
		})
	}
	return e.db.Update(func(tx *bbolt.Tx) error {
		outbox := tx.Bucket(bktOutbox)
		v := outbox.Get(seqKey(seq))
		if v == nil {
			return nil // webhook deleted meanwhile
		}
		var d delivery
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		d.Attempts++
		d.NextAttempt = retryAt
		val, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := outbox.Put(seqKey(seq), val); err != nil {
			return err
		}
		w, err := getWebhook(tx, d.WebhookID)
		if err != nil {
			return nil
		}
		w.LastFailure, w.LastError, w.RetryAfter = at, problem, retryAt
		return putWebhook(tx, w)
	})
}

// finishDelivery removes a delivery from the outbox and updates its
// webhook's health in one transaction.
func (e *Engine) finishDelivery(seq uint64, update func(*store.WebhookRecord)) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		outbox := tx.Bucket(bktOutbox)
		v := outbox.Get(seqKey(seq))
		if v == nil {
			return nil // webhook deleted meanwhile
		}
		var d delivery
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		if err := outbox.Delete(seqKey(seq)); err != nil {
			return err
		}
		w, err := getWebhook(tx, d.WebhookID)
		if err != nil {
			return nil
		}
		w.Pending = max(w.Pending-1, 0)
		update(&w)
		return putWebhook(tx, w)
	})
}
