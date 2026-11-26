package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

func usage(t *testing.T, e *Engine) (bytes, objects int64) {
	t.Helper()
	b, err := e.GetBucket(context.Background(), testBucket)
	if err != nil {
		t.Fatal(err)
	}
	return b.UsedBytes, b.Objects
}

func wantUsage(t *testing.T, e *Engine, bytes, objects int64) {
	t.Helper()
	if b, o := usage(t, e); b != bytes || o != objects {
		t.Errorf("usage = %d bytes / %d objects, want %d / %d", b, o, bytes, objects)
	}
}

// putSized uploads body, telling the engine its size up front (like an
// HTTP Content-Length) unless sized is false.
func putSized(e *Engine, key, body string, sized bool) error {
	hint := int64(len(body))
	if !sized {
		hint = -1
	}
	_, err := e.Put(context.Background(), testBucket, key, strings.NewReader(body), store.Meta{SizeHint: hint})
	return err
}

func TestUsageCounters(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	e := newTrashEngine(t, t.TempDir(), week, &clock)
	wantUsage(t, e, 0, 0)
	put(t, e, "a", "12345")
	put(t, e, "b", "123")
	wantUsage(t, e, 8, 2)
	put(t, e, "a", "1234567890") // overwrite: bytes change, count doesn't
	wantUsage(t, e, 13, 2)
	e.Delete(ctx, testBucket, "a") // to trash: trash doesn't count
	wantUsage(t, e, 3, 1)
	e.Delete(ctx, testBucket, "missing")
	wantUsage(t, e, 3, 1)
	id := trashItems(t, e)[0].ID
	if _, err := e.RestoreTrash(ctx, testBucket, id); err != nil {
		t.Fatal(err)
	}
	wantUsage(t, e, 13, 2)
	e.Delete(ctx, testBucket, "a")
	e.PurgeTrash(ctx, testBucket, "") // purging changes nothing more
	wantUsage(t, e, 3, 1)
	e.Close()

	// Trash off: deletes count the same way.
	e2 := newTrashEngine(t, t.TempDir(), 0, &clock)
	put(t, e2, "x", "1234")
	e2.Delete(ctx, testBucket, "x")
	wantUsage(t, e2, 0, 0)
}

func TestQuota(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	if _, err := e.SetBucketQuota(ctx, testBucket, -1); !errors.Is(err, store.ErrInvalidQuota) {
		t.Errorf("negative quota: %v", err)
	}
	if b, err := e.SetBucketQuota(ctx, testBucket, 10); err != nil || b.QuotaBytes != 10 {
		t.Fatalf("set quota: %+v, %v", b, err)
	}
	if err := putSized(e, "a", "123456", true); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, key, body string
		sized           bool
		wantErr         bool
	}{
		{"known size over quota", "b", "12345", true, true},
		{"unknown size over quota", "b", "12345", false, true},
		{"fits exactly", "b", "1234", true, false},
		{"full: even one byte", "c", "1", false, true},
		{"overwrite counts only the difference", "a", "12", true, false}, // 2+4 = 6
		{"overwrite growing within quota", "a", "123456", false, false},  // 6+4 = 10
		{"overwrite growing past quota", "a", "1234567", true, true},     // would be 11
	}
	for _, tt := range tests {
		err := putSized(e, tt.key, tt.body, tt.sized)
		if gotErr := err != nil; gotErr != tt.wantErr || (err != nil && !errors.Is(err, store.ErrQuotaExceeded)) {
			t.Errorf("%s: err = %v, want error %v", tt.name, err, tt.wantErr)
		}
	}
	if got, _ := readAll(t, e, "a", nil); got != "123456" { // a refused overwrite keeps the old version
		t.Errorf("a = %q after refused overwrite", got)
	}
	wantUsage(t, e, 10, 2)
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 2 {
		t.Errorf("blob files = %d, refused uploads left files behind", n)
	}
	if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
		t.Errorf("tmp files = %d", n)
	}

	// Deleting frees quota at once, even into trash.
	e.Delete(ctx, testBucket, "b")
	if err := putSized(e, "c", "1234", true); err != nil {
		t.Errorf("after delete: %v", err)
	}
	// Restoring b (4 bytes) would pass the quota.
	if _, err := e.RestoreTrash(ctx, testBucket, trashItems(t, e)[0].ID); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Errorf("restore over quota: %v", err)
	}

	// A quota lowered below use blocks growth but not shrinking or deletes.
	e.SetBucketQuota(ctx, testBucket, 5)
	if err := putSized(e, "a", "1234567", true); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Errorf("grow over lowered quota: %v", err)
	}
	if err := putSized(e, "a", "1", true); err != nil {
		t.Errorf("shrink over lowered quota: %v", err)
	}
	if err := e.Delete(ctx, testBucket, "c"); err != nil {
		t.Errorf("delete over quota: %v", err)
	}
	// Removing the quota lifts the limit.
	e.SetBucketQuota(ctx, testBucket, 0)
	if err := putSized(e, "big", strings.Repeat("x", 1000), true); err != nil {
		t.Errorf("no quota: %v", err)
	}
}

// Uploads racing for the last of a quota: the commit decides, so the total
// never passes it however the pre-checks interleave.
func TestQuotaConcurrentUploads(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	e.SetBucketQuota(ctx, testBucket, 100)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 12 {
		wg.Go(func() {
			err := putSized(e, fmt.Sprintf("k%d", i), strings.Repeat("x", 20), true)
			if err != nil && !errors.Is(err, store.ErrQuotaExceeded) {
				t.Errorf("put: %v", err)
			}
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 5 {
		t.Errorf("%d uploads of 20 bytes fit a 100-byte quota, want 5", ok)
	}
	wantUsage(t, e, 100, 5)
}

// The counters live in the same transaction as the object records, so a
// crash at any point of a Put leaves them matching what's there.
func TestUsageAfterCrash(t *testing.T) {
	for _, point := range []string{"put:written", "put:synced", "put:renamed", "put:committed"} {
		t.Run(point, func(t *testing.T) {
			e, dir := newEngine(t)
			put(t, e, "k", "old")
			e.crash = func(p string) bool { return p == point }
			putSized(e, "k", "a much longer new version", true)
			e.Close()
			e = openTest(t, dir)
			got, _ := readAll(t, e, "k", nil)
			wantUsage(t, e, int64(len(got)), 1)
		})
	}
}

func TestFormat2RecountsUsage(t *testing.T) {
	e, dir := newEngine(t)
	put(t, e, "a", "12345")
	put(t, e, "b", "123")
	e.Close()
	// Like format-2 data: stamp 2, no counters.
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bktBuckets).Put([]byte(testBucket), []byte(`{"created":"2026-01-01T00:00:00Z"}`)); err != nil {
			return err
		}
		return tx.Bucket(bktSettings).Put(setFormat, []byte("2"))
	})
	e = openTest(t, dir)
	wantUsage(t, e, 8, 2)
	e.Close()
	if v := storedFormat(t, dir); v != fmt.Sprint(FormatVersion) {
		t.Errorf("format = %s", v)
	}
}
