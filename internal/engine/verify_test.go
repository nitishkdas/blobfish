package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

// verifyFixture writes good.txt, victim.txt and other-bucket/x.txt, closes
// the engine (verify needs the server stopped) and returns the dir and the
// victim's blob ID.
func verifyFixture(t *testing.T) (dir, victimBlob string) {
	t.Helper()
	e, dir := newEngine(t)
	ctx := context.Background()
	put(t, e, "good.txt", "all fine here")
	put(t, e, "victim.txt", "0123456789")
	if _, err := e.CreateBucket(ctx, "other-bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Put(ctx, "other-bucket", "x.txt", bytes.NewReader([]byte("x")), store.Meta{}); err != nil {
		t.Fatal(err)
	}
	rec, err := e.lookup(testBucket, "victim.txt")
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	return dir, rec.Blob
}

// editDB changes meta.db directly, as damage would.
func editDB(t *testing.T, dir string, fn func(tx *bbolt.Tx) error) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

func runVerify(t *testing.T, dir string, opts VerifyOptions) (VerifyStats, []VerifyProblem) {
	t.Helper()
	var probs []VerifyProblem
	opts.Problem = func(p VerifyProblem) { probs = append(probs, p) }
	st, err := Verify(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if st.Problems != len(probs) {
		t.Errorf("Problems = %d, but %d reported", st.Problems, len(probs))
	}
	return st, probs
}

func TestVerifyFindsDamage(t *testing.T) {
	tests := []struct {
		name      string
		damage    func(t *testing.T, dir, blob string)
		wantKinds []string // problems on victim.txt, in order
		orphans   int
	}{
		{name: "clean"},
		{
			name: "flipped byte",
			damage: func(t *testing.T, dir, blob string) {
				p := blobFile(dir, blob)
				b, _ := os.ReadFile(p)
				b[3] ^= 0x01
				os.WriteFile(p, b, 0o600)
			},
			wantKinds: []string{ProblemChecksum},
		},
		{
			name:      "truncated",
			damage:    func(t *testing.T, dir, blob string) { os.Truncate(blobFile(dir, blob), 4) },
			wantKinds: []string{ProblemSize},
		},
		{
			name:      "file gone",
			damage:    func(t *testing.T, dir, blob string) { os.Remove(blobFile(dir, blob)) },
			wantKinds: []string{ProblemMissing},
		},
		{
			// The bytes are fine, but the index lost the blob: the janitor
			// would delete it at the next start. Verify must say so, and
			// must not delete it itself.
			name: "missing from blob index",
			damage: func(t *testing.T, dir, blob string) {
				editDB(t, dir, func(tx *bbolt.Tx) error { return tx.Bucket(bktBlobs).Delete([]byte(blob)) })
			},
			wantKinds: []string{ProblemUnindexed},
			orphans:   1, // the same file, seen from the disk side
		},
		{
			name: "unreadable record",
			damage: func(t *testing.T, dir, blob string) {
				editDB(t, dir, func(tx *bbolt.Tx) error {
					return tx.Bucket(bktObjects).Bucket([]byte(testBucket)).Put([]byte("victim.txt"), []byte("{not json"))
				})
			},
			wantKinds: []string{ProblemMetadata},
		},
		{
			name: "leftover blob is not a problem",
			damage: func(t *testing.T, dir, blob string) {
				p := blobFile(dir, "ffff0000000000000000000000000000")
				os.MkdirAll(filepath.Dir(p), 0o750)
				os.WriteFile(p, []byte("junk"), 0o600)
			},
			orphans: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, blob := verifyFixture(t)
			if tt.damage != nil {
				tt.damage(t, dir, blob)
			}
			st, probs := runVerify(t, dir, VerifyOptions{})
			var kinds []string
			for _, p := range probs {
				if p.Bucket != testBucket || p.Key != "victim.txt" {
					t.Errorf("problem on the wrong object: %v", p)
				}
				kinds = append(kinds, p.Kind)
			}
			if !slices.Equal(kinds, tt.wantKinds) {
				t.Errorf("problems = %v, want kinds %v", probs, tt.wantKinds)
			}
			if st.Objects != 3 || st.Buckets != 2 || st.OrphanBlobs != tt.orphans {
				t.Errorf("stats = %+v, want 3 objects, 2 buckets, %d orphans", st, tt.orphans)
			}
		})
	}
}

func blobFile(dir, id string) string {
	return (&Engine{objDir: filepath.Join(dir, "objects")}).blobPath(id)
}

func TestVerifyWritesNothing(t *testing.T) {
	dir, blob := verifyFixture(t)
	// The dangerous case: a blob the janitor would delete.
	editDB(t, dir, func(tx *bbolt.Tx) error { return tx.Bucket(bktBlobs).Delete([]byte(blob)) })
	os.WriteFile(filepath.Join(dir, "tmp", "leftover-upload"), []byte("x"), 0o600)
	meta := filepath.Join(dir, "meta.db")
	before, _ := os.ReadFile(meta)
	stBefore, _ := os.Stat(meta)

	runVerify(t, dir, VerifyOptions{})

	after, _ := os.ReadFile(meta)
	stAfter, _ := os.Stat(meta)
	if !bytes.Equal(before, after) || !stBefore.ModTime().Equal(stAfter.ModTime()) {
		t.Error("meta.db changed")
	}
	for _, p := range []string{blobFile(dir, blob), filepath.Join(dir, "tmp", "leftover-upload")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("verify removed %s: %v", p, err)
		}
	}
}

func TestVerifyBucketFilter(t *testing.T) {
	dir, blob := verifyFixture(t)
	os.Remove(blobFile(dir, blob)) // damage in test-bucket only

	st, probs := runVerify(t, dir, VerifyOptions{Bucket: "other-bucket"})
	if st.Objects != 1 || st.Buckets != 1 || len(probs) != 0 {
		t.Errorf("other-bucket: stats %+v, problems %v", st, probs)
	}
	if _, err := Verify(context.Background(), dir, VerifyOptions{Bucket: "no-such"}); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("unknown bucket: err = %v", err)
	}
}

// More objects than one metadata batch, so paging between batches is
// exercised, with damage on both sides of the batch boundary.
func TestVerifyManyObjects(t *testing.T) {
	e, dir := newEngine(t)
	const n = verifyBatch*2 + 7
	var keys []string
	for i := range n {
		k := fmt.Sprintf("k%05d", i)
		keys = append(keys, k)
		put(t, e, k, k)
	}
	var broken []string
	for _, k := range []string{keys[verifyBatch-1], keys[verifyBatch], keys[n-1]} {
		rec, _ := e.lookup(testBucket, k)
		os.Remove(blobFile(dir, rec.Blob))
		broken = append(broken, k)
	}
	e.Close()

	var progressCalls int
	st, probs := runVerify(t, dir, VerifyOptions{Progress: func(VerifyStats) { progressCalls++ }})
	var got []string
	for _, p := range probs {
		got = append(got, p.Key)
	}
	if st.Objects != n || !slices.Equal(got, broken) {
		t.Errorf("objects %d (want %d), problems on %v (want %v)", st.Objects, n, got, broken)
	}
	if progressCalls == 0 {
		t.Error("progress never reported")
	}
}

func TestVerifyRefuses(t *testing.T) {
	t.Run("server running", func(t *testing.T) {
		e, dir := newEngine(t)
		defer e.Close()
		start := time.Now()
		if _, err := Verify(context.Background(), dir, VerifyOptions{}); err == nil {
			t.Error("verify ran while the data dir was open")
		}
		if time.Since(start) > 10*time.Second {
			t.Error("verify waited too long for the lock")
		}
	})
	t.Run("no data", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Verify(context.Background(), dir, VerifyOptions{}); err == nil {
			t.Error("verify of an empty dir succeeded")
		}
		if _, err := os.Stat(filepath.Join(dir, "meta.db")); err == nil {
			t.Error("verify created meta.db")
		}
	})
	t.Run("newer format", func(t *testing.T) {
		dir, _ := verifyFixture(t)
		editDB(t, dir, func(tx *bbolt.Tx) error {
			return tx.Bucket(bktSettings).Put(setFormat, []byte(fmt.Sprint(FormatVersion+1)))
		})
		if _, err := Verify(context.Background(), dir, VerifyOptions{}); !errors.Is(err, ErrFormatTooNew) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		dir, _ := verifyFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Verify(ctx, dir, VerifyOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}
