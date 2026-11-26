package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

// TestCrashPoints stops a write dead at each step (no cleanup runs, as if
// the process were killed there), reopens the data dir, and checks that the
// object is wholly old or wholly new and that no orphan files remain.
func TestCrashPoints(t *testing.T) {
	tests := []struct {
		point    string
		hasOld   bool
		wantNew  bool // false: the old object (or nothing) must survive
		isDelete bool
	}{
		{point: "put:written", hasOld: false},
		{point: "put:written", hasOld: true},
		{point: "put:synced", hasOld: false},
		{point: "put:synced", hasOld: true},
		{point: "put:renamed", hasOld: false},
		{point: "put:renamed", hasOld: true},
		{point: "put:committed", hasOld: false, wantNew: true},
		{point: "put:committed", hasOld: true, wantNew: true},
		// bbolt reported a commit error but the record reached disk: the
		// blob must still be there (deleting it would lose the object).
		{point: "put:commit-uncertain", hasOld: false, wantNew: true},
		{point: "put:commit-uncertain", hasOld: true, wantNew: true},
		{point: "delete:committed", hasOld: true, isDelete: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/old=%v", tt.point, tt.hasOld), func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			e, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.CreateBucket(ctx, testBucket); err != nil {
				t.Fatal(err)
			}
			const oldBody, newBody = "old version", "new version, longer than the old one"
			if tt.hasOld {
				if _, err := e.Put(ctx, testBucket, "k", strings.NewReader(oldBody), store.Meta{}); err != nil {
					t.Fatal(err)
				}
			}

			fired := false
			e.crash = func(p string) bool {
				if p == tt.point {
					fired = true
					return true
				}
				return false
			}
			if tt.isDelete {
				err = e.Delete(ctx, testBucket, "k")
			} else {
				_, err = e.Put(ctx, testBucket, "k", strings.NewReader(newBody), store.Meta{})
			}
			if !fired || !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("crash point %q did not fire (err=%v)", tt.point, err)
			}
			// Callers must learn that the write may have happened.
			if uncertain := errors.Is(err, store.ErrCommitUncertain); uncertain != (tt.point == "put:commit-uncertain") {
				t.Errorf("ErrCommitUncertain = %v for %s (err=%v)", uncertain, tt.point, err)
			}
			e.Close() // the "process" is gone; only the disk remains

			// Restart. Open runs the janitor.
			e = openTest(t, dir)

			var want string
			switch {
			case tt.isDelete:
				want = ""
			case tt.wantNew:
				want = newBody
			case tt.hasOld:
				want = oldBody
			}
			rc, _, err := e.Get(ctx, testBucket, "k", nil)
			if want == "" {
				if err == nil {
					rc.Close()
					t.Fatalf("object exists after crash, want none")
				}
			} else {
				if err != nil {
					t.Fatalf("Get after crash: %v", err)
				}
				b, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatalf("read after crash: %v", err) // includes checksum check
				}
				if string(b) != want {
					t.Fatalf("after crash got %q, want %q", b, want)
				}
			}

			if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
				t.Errorf("tmp has %d files after restart", n)
			}
			wantBlobs := 0
			if want != "" {
				wantBlobs = 1
			}
			if n := countFiles(t, filepath.Join(dir, "objects")); n != wantBlobs {
				t.Errorf("objects/ has %d files after restart, want %d", n, wantBlobs)
			}
		})
	}
}

// TestJanitorRacesWrites runs the janitor in a tight loop while writers
// overwrite keys. The janitor must never delete a blob a Put is about to
// commit; every final object must read back intact.
func TestJanitorRacesWrites(t *testing.T) {
	e, dir := newEngine(t)
	ctx, cancel := context.WithCancel(context.Background())

	var janitor sync.WaitGroup
	janitor.Go(func() {
		for ctx.Err() == nil {
			if _, err := e.RunJanitor(ctx); err != nil && ctx.Err() == nil {
				t.Errorf("janitor: %v", err)
				return
			}
		}
	})

	var writers sync.WaitGroup
	for w := range 4 {
		writers.Go(func() {
			for i := range 50 {
				key := fmt.Sprintf("k%d", i%5)
				body := fmt.Sprintf("writer %d iteration %d %s", w, i, strings.Repeat("x", i*100))
				if _, err := e.Put(ctx, testBucket, key, strings.NewReader(body), store.Meta{}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				rc, _, err := e.Get(ctx, testBucket, key, nil)
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if _, err := io.Copy(io.Discard, rc); err != nil {
					t.Errorf("read %s: %v", key, err)
				}
				rc.Close()
			}
		})
	}
	writers.Wait()
	cancel()
	janitor.Wait()

	for i := range 5 {
		readAll(t, e, fmt.Sprintf("k%d", i), nil) // fails the test on corruption
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 5 {
		t.Errorf("objects/ has %d files, want 5 (one per key)", n)
	}
}

// Kill -9 test. The parent re-runs this test binary as a child process that
// writes and overwrites objects nonstop, SIGKILLs it at a random moment,
// then reopens the data dir and verifies every object. Each object's body is
// generated from a seed stored in its user metadata, so the parent can
// recompute the exact expected bytes.
//
// SIGKILL loses the process but not the OS page cache, so this checks the
// ordering logic, not fsync placement. Power-cut testing needs a VM or
// dm-flakey and is tracked separately for the milestone gate.

const crashChildEnv = "BLOBFISH_CRASH_CHILD_DIR"

func TestKill9DuringWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	dir := t.TempDir()
	const rounds = 5
	for round := range rounds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), crashChildEnv+"="+dir, "BLOBFISH_CRASH_CHILD_SEED="+strconv.Itoa(round))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Let it get going, then kill at a random point.
		time.Sleep(time.Duration(300+rand.IntN(400)) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait() // "signal: killed" is the expected outcome

		verifyAfterCrash(t, dir, round)
	}
}

func verifyAfterCrash(t *testing.T, dir string, round int) {
	t.Helper()
	ctx := context.Background()
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("round %d: reopen: %v", round, err)
	}
	defer e.Close()

	res, err := e.List(ctx, testBucket, store.ListOptions{})
	if err != nil {
		t.Fatalf("round %d: list: %v", round, err)
	}
	if len(res.Objects) == 0 {
		t.Fatalf("round %d: child wrote nothing before being killed", round)
	}
	for _, o := range res.Objects {
		seed, err := strconv.ParseUint(o.User["seed"], 10, 64)
		if err != nil {
			t.Fatalf("round %d: %s: bad seed %q", round, o.Key, o.User["seed"])
		}
		size, _ := strconv.Atoi(o.User["size"])
		want := payload(seed, size)

		rc, _, err := e.Get(ctx, testBucket, o.Key, nil)
		if err != nil {
			t.Fatalf("round %d: get %s: %v", round, o.Key, err)
		}
		got, err := io.ReadAll(rc) // test-only; bodies are at most a few MB
		rc.Close()
		if err != nil {
			t.Fatalf("round %d: read %s: %v", round, o.Key, err)
		}
		if string(got) != string(want) {
			t.Fatalf("round %d: %s: content differs from what was written (%d vs %d bytes)", round, o.Key, len(got), len(want))
		}
	}
	if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
		t.Errorf("round %d: tmp has %d files after restart", round, n)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != len(res.Objects) {
		t.Errorf("round %d: %d blob files for %d objects", round, n, len(res.Objects))
	}
	t.Logf("round %d: %d objects verified", round, len(res.Objects))
}

// TestCrashChild is the child half of TestKill9DuringWrites. It does nothing
// unless started by the parent, and it never returns on its own.
func TestCrashChild(t *testing.T) {
	dir := os.Getenv(crashChildEnv)
	if dir == "" {
		t.Skip("helper process for TestKill9DuringWrites")
	}
	baseSeed, _ := strconv.ParseUint(os.Getenv("BLOBFISH_CRASH_CHILD_SEED"), 10, 64)
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.CreateBucket(ctx, testBucket); err != nil && !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(baseSeed, 0))
	for {
		key := fmt.Sprintf("obj-%02d", rng.IntN(20))
		if rng.IntN(10) == 0 {
			if err := e.Delete(ctx, testBucket, key); err != nil {
				t.Fatal(err)
			}
			continue
		}
		seed := rng.Uint64()
		size := rng.IntN(512 << 10)
		if rng.IntN(8) == 0 {
			size = 2<<20 + rng.IntN(2<<20) // some multi-MB writes widen the window
		}
		meta := store.Meta{User: map[string]string{
			"seed": strconv.FormatUint(seed, 10),
			"size": strconv.Itoa(size),
		}}
		if _, err := e.Put(ctx, testBucket, key, io.LimitReader(newPayloadReader(seed), int64(size)), meta); err != nil {
			t.Fatal(err)
		}
	}
}

// payload returns the deterministic body for seed.
func payload(seed uint64, size int) []byte {
	b := make([]byte, size)
	if _, err := io.ReadFull(newPayloadReader(seed), b); err != nil {
		panic(err)
	}
	return b
}

type payloadReader struct{ r *rand.ChaCha8 }

func newPayloadReader(seed uint64) payloadReader {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	return payloadReader{rand.NewChaCha8(s)}
}

func (p payloadReader) Read(b []byte) (int, error) { return p.r.Read(b) }
