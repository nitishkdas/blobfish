package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

func backupBytes(t *testing.T, e *Engine) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Backup(context.Background(), &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return buf.Bytes()
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()
	want := map[string]string{
		"a.txt":            "alpha",
		"docs/report q.md": strings.Repeat("report ", 50_000),
		"empty":            "",
	}
	for k, v := range want {
		put(t, e, k, v)
	}
	if _, err := e.CreateBucket(ctx, "assets"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetBucketPublic(ctx, "assets", true); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateKey(ctx, store.KeyRecord{ID: "k1", Name: "app", Scopes: []string{"x:r"}, Hash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateLink(ctx, link("L1", testBucket, "a.txt")); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "restored")
	st, err := Restore(ctx, bytes.NewReader(backupBytes(t, e)), dir, Options{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if st.Objects != len(want) || st.Buckets != 2 {
		t.Errorf("stats = %+v", st)
	}

	r := openTest(t, dir)
	for k, v := range want {
		if got, _ := readAll(t, r, k, nil); got != v {
			t.Errorf("%s: %d bytes, want %d", k, len(got), len(v))
		}
	}
	if b, _ := r.GetBucket(ctx, "assets"); !b.Public {
		t.Error("public flag lost")
	}
	if k, err := r.KeyByHash(ctx, "h1"); err != nil || k.Name != "app" {
		t.Errorf("API key lost: %+v %v", k, err)
	}
	if _, err := r.LinkByToken(ctx, "tok-L1"); err != nil {
		t.Errorf("share link lost: %v", err)
	}
	if !bytes.Equal(r.PresignSecret(), e.PresignSecret()) {
		t.Error("presign secret changed, so existing presigned URLs would break")
	}
}

// TestBackupDuringWrites takes backups while other goroutines overwrite
// and delete objects. Every blob in the snapshot must still be there when
// it is copied, and the restored data must verify.
func TestBackupDuringWrites(t *testing.T) {
	e, dir := newEngine(t)
	ctx := context.Background()
	const keys = 60
	for i := range keys {
		put(t, e, fmt.Sprintf("k%02d", i), strings.Repeat(fmt.Sprint(i), 20_000))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("k%02d", (i*7+w)%keys)
				if i%5 == 0 {
					e.Delete(ctx, testBucket, key)
					continue
				}
				e.Put(ctx, testBucket, key, strings.NewReader(strings.Repeat(fmt.Sprint(w, i), 15_000)), store.Meta{})
			}
		})
	}
	for round := range 5 {
		data := backupBytes(t, e)
		if _, err := Restore(ctx, bytes.NewReader(data), filepath.Join(t.TempDir(), fmt.Sprint(round)), Options{}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("round %d: restore of a backup taken during writes: %v", round, err)
		}
	}
	close(stop)
	wg.Wait()

	// Deletions postponed during backups ran afterwards: no orphans left.
	if _, err := e.RunJanitor(ctx); err != nil {
		t.Fatal(err)
	}
	res, _ := e.List(ctx, testBucket, store.ListOptions{})
	if n := countFiles(t, filepath.Join(dir, "objects")); n != len(res.Objects) {
		t.Errorf("%d blob files for %d objects after backups finished", n, len(res.Objects))
	}
}

// rewriteTar copies a backup, letting edit change or drop entries and add
// extra ones at the end.
func rewriteTar(t *testing.T, in []byte, edit func(h *tar.Header, body []byte) ([]byte, bool), extra ...func(*tar.Writer)) []byte {
	t.Helper()
	var out bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(in)), tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr) // test-only, small backups
		body, keep := edit(h, body)
		if !keep {
			continue
		}
		h.Size = int64(len(body))
		tw.WriteHeader(h)
		tw.Write(body)
	}
	for _, x := range extra {
		x(tw)
	}
	tw.Close()
	return out.Bytes()
}

func TestRestoreRejects(t *testing.T) {
	e, _ := newEngine(t)
	put(t, e, "k", "precious bytes")
	good := backupBytes(t, e)
	keepAll := func(h *tar.Header, b []byte) ([]byte, bool) { return b, true }
	addEntry := func(name string, typ byte, body string) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Size: int64(len(body)), Mode: 0o600, Linkname: "/etc/passwd"})
			tw.Write([]byte(body))
		}
	}

	tests := []struct {
		name    string
		data    []byte
		target  func(t *testing.T) string
		wantErr string
	}{
		{"non-empty target", good, func(t *testing.T) string {
			d := t.TempDir()
			os.WriteFile(filepath.Join(d, "x"), nil, 0o600)
			return d
		}, "not empty"},
		{"path traversal", rewriteTar(t, good, keepAll, addEntry("../../evil", tar.TypeReg, "x")), nil, "unexpected backup entry"},
		{"absolute path", rewriteTar(t, good, keepAll, addEntry("/tmp/evil", tar.TypeReg, "x")), nil, "unexpected backup entry"},
		{"symlink", rewriteTar(t, good, keepAll, addEntry("objects/ab/cd/abcd0000000000000000000000000000", tar.TypeSymlink, "")), nil, "not a regular file"},
		{"wrong shard", rewriteTar(t, good, keepAll, addEntry("objects/00/00/abcd0000000000000000000000000000", tar.TypeReg, "x")), nil, "wrong shard"},
		{"missing meta.db", rewriteTar(t, good, func(h *tar.Header, b []byte) ([]byte, bool) { return b, h.Name != "meta.db" }), nil, "incomplete"},
		{"newer format", rewriteTar(t, good, func(h *tar.Header, b []byte) ([]byte, bool) {
			if h.Name == "manifest.json" {
				return bytes.Replace(b, []byte(`"format":1`), []byte(`"format":99`), 1), true
			}
			return b, true
		}), nil, "newer BlobFish"},
		{"corrupted object", rewriteTar(t, good, func(h *tar.Header, b []byte) ([]byte, bool) {
			if strings.HasPrefix(h.Name, "objects/") {
				b = bytes.Clone(b)
				b[0] ^= 1
			}
			return b, true
		}), nil, "failed verification"},
		{"missing object", rewriteTar(t, good, func(h *tar.Header, b []byte) ([]byte, bool) {
			return b, !strings.HasPrefix(h.Name, "objects/")
		}), nil, "failed verification"},
		{"truncated", good[:len(good)/2], nil, "read backup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "r")
			if tt.target != nil {
				dir = tt.target(t)
			}
			_, err := Restore(context.Background(), bytes.NewReader(tt.data), dir, Options{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); statErr == nil {
				t.Fatal("restore wrote outside its target directory")
			}
			// A failed restore leaves nothing behind, so it can be retried.
			if leftovers, _ := os.ReadDir(dir); tt.target == nil && len(leftovers) > 0 {
				t.Errorf("failed restore left %d entries in %s", len(leftovers), dir)
			}
		})
	}
}

func TestBackupStopsOnCancel(t *testing.T) {
	e, _ := newEngine(t)
	put(t, e, "k", "v")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Backup(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// The pause on deletions ends even when a backup fails.
	if e.backupRunning() {
		t.Error("backup still marked running after it failed")
	}
}

func TestFailedRestoreCanBeRetried(t *testing.T) {
	e, _ := newEngine(t)
	put(t, e, "k", "v")
	good := backupBytes(t, e)
	for _, existing := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "r")
		if existing {
			os.Mkdir(dir, 0o750) // an empty dir the operator made
		}
		if _, err := Restore(context.Background(), bytes.NewReader(good[:len(good)/2]), dir, Options{}); err == nil {
			t.Fatal("truncated backup restored")
		}
		if st, err := Restore(context.Background(), bytes.NewReader(good), dir, Options{}); err != nil || st.Objects != 1 {
			t.Fatalf("existing=%v: retry after a failed restore: %+v %v", existing, st, err)
		}
	}
}
