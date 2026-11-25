package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

const mib = 1 << 20

// fakeSpace makes e report free bytes from fn, with a 100 MiB reserve.
func fakeSpace(e *Engine, fn func() uint64) {
	e.minFree = 100 * mib
	e.freeSpace = func() (uint64, error) { return fn(), nil }
}

// zeros is an io.Reader of n zero bytes of unknown length to the engine.
type zeros struct{ n int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), z.n)
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

func TestSpaceReserve(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		free     uint64
		hint     int64
		wantFull bool
	}{
		{"fits", 1000 * mib, 500 * mib, false},
		{"would cut into reserve", 1000 * mib, 950 * mib, true},
		{"already inside reserve, unknown size", 90 * mib, 0, true},
		{"exactly at the edge", 600 * mib, 500 * mib, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, dir := newEngine(t)
			fakeSpace(e, func() uint64 { return tt.free })
			_, err := e.Put(ctx, testBucket, "k", strings.NewReader("x"), store.Meta{SizeHint: tt.hint})
			if got := errors.Is(err, store.ErrInsufficientStorage); got != tt.wantFull {
				t.Fatalf("err = %v, want insufficient=%v", err, tt.wantFull)
			}
			if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
				t.Errorf("refused upload left %d temp files", n)
			}
		})
	}
}

// TestSpaceRunsOutMidStream: an upload of unknown size is stopped once the
// disk drops into the reserve while it streams; the old object survives.
func TestSpaceRunsOutMidStream(t *testing.T) {
	e, dir := newEngine(t)
	put(t, e, "k", "old version")
	var calls atomic.Int64
	fakeSpace(e, func() uint64 {
		if calls.Add(1) == 1 {
			return 10_000 * mib // plenty at the start
		}
		return 50 * mib // then the disk fills up
	})
	_, err := e.Put(context.Background(), testBucket, "k", &zeros{n: 300 * mib}, store.Meta{})
	if !errors.Is(err, store.ErrInsufficientStorage) {
		t.Fatalf("err = %v, want ErrInsufficientStorage", err)
	}
	if calls.Load() < 2 {
		t.Fatal("free space was never re-checked while streaming")
	}
	if got, _ := readAll(t, e, "k", nil); got != "old version" {
		t.Errorf("old object = %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
		t.Errorf("%d temp files left", n)
	}
}

// TestDeleteWorksWhenFull: freeing space must always be possible.
func TestDeleteWorksWhenFull(t *testing.T) {
	e, _ := newEngine(t)
	put(t, e, "k", "v")
	fakeSpace(e, func() uint64 { return 0 })
	if err := e.Delete(context.Background(), testBucket, "k"); err != nil {
		t.Fatalf("delete on a full disk: %v", err)
	}
	if _, err := e.Put(context.Background(), testBucket, "k2", strings.NewReader("x"), store.Meta{}); !errors.Is(err, store.ErrInsufficientStorage) {
		t.Errorf("upload on a full disk: %v", err)
	}
}

func TestDiskFreeReal(t *testing.T) {
	free, err := diskFree(t.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("diskFree = %d, %v", free, err)
	}
}

func TestFreeFromStatfs(t *testing.T) {
	if free, err := freeFromStatfs(1000, 250, 4096); err != nil || free != 250*4096 {
		t.Errorf("normal fs: %d, %v", free, err)
	}
	// LazyFS and some network file systems report zero capacity: that is
	// "unknown", and must not block uploads as if the disk were full.
	if _, err := freeFromStatfs(0, 0, 0); !errors.Is(err, errCapacityUnknown) {
		t.Errorf("zero-capacity fs: %v", err)
	}
	e, _ := newEngine(t)
	e.minFree = 100 * mib
	e.freeSpace = func() (uint64, error) { return freeFromStatfs(0, 0, 4096) }
	if err := e.checkSpace(10 * mib); err != nil {
		t.Errorf("upload refused on a fs that reports no capacity: %v", err)
	}
}
