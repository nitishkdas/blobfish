package engine

import (
	"context"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// A receiver that is down long enough fills its queue; further events are
// counted as dropped instead of growing meta.db without bound.
func TestWebhookBacklogCap(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	w := store.WebhookRecord{ID: "h1", Bucket: testBucket, URL: "https://example.com/", Events: store.EventTypes}
	if err := e.CreateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	put(t, e, "a", "1")
	e.db.Update(func(tx *bbolt.Tx) error {
		w, _ := getWebhook(tx, "h1")
		w.Pending = MaxPendingPerWebhook
		return putWebhook(tx, w)
	})
	put(t, e, "b", "2")
	got, _ := e.WebhookByID(ctx, "h1")
	if got.Dropped != 1 || got.Pending != MaxPendingPerWebhook {
		t.Errorf("webhook = %+v", got)
	}
	if due, _ := e.DueDeliveries(ctx, time.Now().Add(time.Minute), 10); len(due) != 1 {
		t.Errorf("queued %d deliveries, want 1", len(due))
	}
}

func TestWebhookLimitPerBucket(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	for i := range MaxWebhooksPerBucket + 1 {
		err := e.CreateWebhook(ctx, store.WebhookRecord{ID: string(rune('a' + i)), Bucket: testBucket, URL: "https://example.com/"})
		if (err != nil) != (i == MaxWebhooksPerBucket) {
			t.Fatalf("webhook %d: %v", i, err)
		}
	}
}
