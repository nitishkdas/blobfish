package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

func TestNoOverwrite(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	put(t, e, "k", "original")
	_, err := e.Put(ctx, testBucket, "k", strings.NewReader("intruder"), store.Meta{NoOverwrite: true})
	if !errors.Is(err, store.ErrKeyExists) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := readAll(t, e, "k", nil); got != "original" {
		t.Errorf("overwritten: %q", got)
	}
	if _, err := e.Put(ctx, testBucket, "new", strings.NewReader("ok"), store.Meta{NoOverwrite: true}); err != nil {
		t.Fatal(err)
	}

	// Racing uploads to one new key: exactly one wins, decided at commit.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Go(func() {
			_, err := e.Put(ctx, testBucket, "race", strings.NewReader("x"), store.Meta{NoOverwrite: true})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, store.ErrKeyExists) {
				t.Errorf("put: %v", err)
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d uploads won the same key", won)
	}
	wantUsage(t, e, int64(len("original")+len("ok")+1), 3)
}

func TestUploadLinks(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	rec := store.UploadLinkRecord{ID: "u1", Token: "tok1", Bucket: testBucket, Prefix: "in/", MaxFiles: 2, Created: time.Now()}
	if err := e.CreateUploadLink(ctx, rec); err != nil {
		t.Fatal(err)
	}
	bad := rec
	bad.ID, bad.Token, bad.Bucket = "u2", "tok2", "no-such-bucket"
	if err := e.CreateUploadLink(ctx, bad); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("link to missing bucket: %v", err)
	}

	// Slots: two claims, the third refused; a release gives one back.
	for i := range 2 {
		if r, err := e.ClaimUploadSlot(ctx, "tok1"); err != nil || r.Uploads != i+1 {
			t.Fatalf("claim %d: %+v, %v", i, r, err)
		}
	}
	if _, err := e.ClaimUploadSlot(ctx, "tok1"); !errors.Is(err, store.ErrUploadLimitReached) {
		t.Fatalf("third claim: %v", err)
	}
	e.ReleaseUploadSlot(ctx, "tok1")
	if _, err := e.ClaimUploadSlot(ctx, "tok1"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if _, err := e.ClaimUploadSlot(ctx, "nope"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("unknown token: %v", err)
	}
	if err := e.ReleaseUploadSlot(ctx, "nope"); err != nil {
		t.Errorf("release of a deleted link: %v", err)
	}

	list, _ := e.ListUploadLinks(ctx, testBucket)
	if len(list) != 1 || list[0].Uploads != 2 || list[0].Prefix != "in/" {
		t.Errorf("list = %+v", list)
	}
	if got, err := e.UploadLinkByID(ctx, "u1"); err != nil || got.Token != "tok1" {
		t.Errorf("by id: %+v, %v", got, err)
	}

	// Deleting the bucket takes its upload links along.
	if err := e.DeleteBucket(ctx, testBucket); err != nil {
		t.Fatal(err)
	}
	e.CreateBucket(ctx, testBucket)
	if _, err := e.UploadLinkByToken(ctx, "tok1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("re-created bucket inherited an upload link: %v", err)
	}
}

func TestUploadLinkDelete(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	e.CreateUploadLink(ctx, store.UploadLinkRecord{ID: "u1", Token: "tok1", Bucket: testBucket})
	if err := e.DeleteUploadLink(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.UploadLinkByToken(ctx, "tok1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("after delete: %v", err)
	}
	if err := e.DeleteUploadLink(ctx, "u1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("delete twice: %v", err)
	}
}
