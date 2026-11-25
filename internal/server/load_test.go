package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/engine"
)

// liveServer runs the full stack on a real TCP listener.
func liveServer(t *testing.T, maxObject int64) (url, admin string) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	svc := auth.NewService(eng, eng.PresignSecret())
	admin, err = svc.EnsureAdminKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{
		Logger: slog.New(slog.DiscardHandler), MaxObjectSize: maxObject,
		Store: eng, Buckets: eng, Auth: svc,
	}))
	t.Cleanup(srv.Close)
	return srv.URL, admin
}

func call(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// patternReader yields n deterministic, non-repeating-looking bytes without
// holding them in memory.
type patternReader struct {
	n, off int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	k := min(int64(len(b)), p.n-p.off)
	for i := range k {
		x := uint64(p.off + i)
		b[i] = byte(x ^ x>>8 ^ x>>16*7)
	}
	p.off += k
	return int(k), nil
}

// heapPeak samples the live heap until stop is closed.
func heapPeak(stop <-chan struct{}) <-chan uint64 {
	out := make(chan uint64, 1)
	go func() {
		var peak uint64
		var ms runtime.MemStats
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapInuse)
			select {
			case <-stop:
				out <- peak
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// TestStreamingMemory uploads and downloads an object far larger than the
// allowed memory growth, proving bodies are streamed end to end, never
// buffered, as a 512 MB server requires.
func TestStreamingMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 256 MB")
	}
	const size = 256 << 20
	const budget = 48 << 20 // allowed heap growth; the race detector adds overhead
	url, admin := liveServer(t, 1<<40)
	call(t, "PUT", url+"/v1/buckets/big", admin, nil).Body.Close()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	stop := make(chan struct{})
	peakCh := heapPeak(stop)

	h := sha256.New()
	resp := call(t, "PUT", url+"/v1/b/big/blob", admin, io.TeeReader(&patternReader{n: size}, h))
	var put objectJSON
	json.NewDecoder(resp.Body).Decode(&put)
	resp.Body.Close()
	if resp.StatusCode != 200 || put.Size != size || put.ETag != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("PUT %d: %+v", resp.StatusCode, put)
	}

	resp = call(t, "GET", url+"/v1/b/big/blob", admin, nil)
	h2 := sha256.New()
	n, err := io.Copy(h2, resp.Body)
	resp.Body.Close()
	if err != nil || n != size || hex.EncodeToString(h2.Sum(nil)) != put.ETag {
		t.Fatalf("GET: %d bytes, err %v", n, err)
	}

	close(stop)
	peak := <-peakCh
	growth := int64(peak) - int64(base.HeapInuse)
	t.Logf("moved %d MB each way; heap in use grew by %.1f MB at peak", size>>20, float64(growth)/(1<<20))
	if growth > budget {
		t.Fatalf("heap grew %d MB for a %d MB object: something buffers bodies", growth>>20, size>>20)
	}
}

// TestLoadSmallMachine runs mixed traffic on one CPU, like a 1 vCPU VPS,
// and reports throughput and latency. It fails on any error or on a final
// object that doesn't read back intact. Opt in: BLOBFISH_LOADTEST=1.
func TestLoadSmallMachine(t *testing.T) {
	if os.Getenv("BLOBFISH_LOADTEST") == "" {
		t.Skip("set BLOBFISH_LOADTEST=1 to run")
	}
	duration := 15 * time.Second
	if d, err := time.ParseDuration(os.Getenv("BLOBFISH_LOADTEST_DURATION")); err == nil {
		duration = d
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	url, admin := liveServer(t, 1<<30)
	call(t, "PUT", url+"/v1/buckets/load", admin, nil).Body.Close()

	const workers = 16
	const keys = 200
	type sample struct {
		op  string
		dur time.Duration
	}
	var (
		mu      sync.Mutex
		samples []sample
		errs    atomic.Int64
		bytes   atomic.Int64
		// written[key] = sha256 of the last successful PUT body; guarded by keyLocks
		written  = make([]string, keys)
		keyLocks [keys]sync.Mutex
	)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	do := func(method, path string, body io.Reader) (*http.Response, error) {
		req, _ := http.NewRequest(method, url+path, body)
		req.Header.Set("Authorization", "Bearer "+admin)
		return client.Do(req)
	}

	stop := make(chan struct{})
	peakCh := heapPeak(stop)
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			for time.Now().Before(deadline) {
				k := rng.IntN(keys)
				path := fmt.Sprintf("/v1/b/load/obj-%03d", k)
				start := time.Now()
				var op string
				var err error
				switch r := rng.IntN(100); {
				case r < 70:
					op = "GET"
					var resp *http.Response
					if resp, err = do("GET", path, nil); err == nil {
						n, _ := io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						bytes.Add(n)
						if resp.StatusCode != 200 && resp.StatusCode != 404 {
							err = fmt.Errorf("GET status %d", resp.StatusCode)
						}
					}
				case r < 90:
					op = "PUT"
					size := int64(1<<10 + rng.IntN(1<<20)) // 1 KB .. 1 MB
					h := sha256.New()
					keyLocks[k].Lock()
					var resp *http.Response
					if resp, err = do("PUT", path, io.TeeReader(&patternReader{n: size, off: int64(rng.IntN(1000))}, h)); err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode == 200 {
							written[k] = hex.EncodeToString(h.Sum(nil))
							bytes.Add(size)
						} else {
							err = fmt.Errorf("PUT status %d", resp.StatusCode)
						}
					}
					keyLocks[k].Unlock()
				case r < 95:
					op = "LIST"
					var resp *http.Response
					if resp, err = do("GET", "/v1/b/load?limit=100", nil); err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != 200 {
							err = fmt.Errorf("LIST status %d", resp.StatusCode)
						}
					}
				default:
					op = "DELETE"
					keyLocks[k].Lock()
					var resp *http.Response
					if resp, err = do("DELETE", path, nil); err == nil {
						resp.Body.Close()
						if resp.StatusCode == 204 {
							written[k] = ""
						} else {
							err = fmt.Errorf("DELETE status %d", resp.StatusCode)
						}
					}
					keyLocks[k].Unlock()
				}
				if err != nil {
					errs.Add(1)
					t.Errorf("%s %s: %v", op, path, err)
				}
				mu.Lock()
				samples = append(samples, sample{op, time.Since(start)})
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(stop)
	peak := <-peakCh

	byOp := map[string][]time.Duration{}
	for _, s := range samples {
		byOp[s.op] = append(byOp[s.op], s.dur)
	}
	t.Logf("GOMAXPROCS=1, %d workers, %s: %d requests (%.0f/s), %.1f MB/s, peak heap %.1f MB, %d errors",
		workers, duration, len(samples), float64(len(samples))/duration.Seconds(),
		float64(bytes.Load())/(1<<20)/duration.Seconds(), float64(peak)/(1<<20), errs.Load())
	for _, op := range []string{"GET", "PUT", "LIST", "DELETE"} {
		d := byOp[op]
		if len(d) == 0 {
			continue
		}
		slices.Sort(d)
		t.Logf("  %-6s n=%-6d p50=%-10s p99=%-10s max=%s", op, len(d), d[len(d)/2].Round(time.Microsecond),
			d[len(d)*99/100].Round(time.Microsecond), d[len(d)-1].Round(time.Microsecond))
	}

	// Final state must match the last successful write to every key.
	for k := range keys {
		path := fmt.Sprintf("/v1/b/load/obj-%03d", k)
		resp, err := do("GET", path, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, resp.Body)
		resp.Body.Close()
		switch {
		case written[k] == "" && resp.StatusCode != 404:
			t.Errorf("%s: deleted but status %d", path, resp.StatusCode)
		case written[k] != "" && (err != nil || hex.EncodeToString(h.Sum(nil)) != written[k]):
			t.Errorf("%s: content does not match last write (status %d, err %v)", path, resp.StatusCode, err)
		}
	}
	if errs.Load() != 0 {
		t.Fatalf("%d request errors", errs.Load())
	}
}
