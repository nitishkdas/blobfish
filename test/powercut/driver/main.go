// Command driver runs BlobFish on a LazyFS mount, simulates power cuts and
// checks that no acknowledged write is lost or corrupted.
//
// Each round: write and delete objects with several concurrent clients,
// SIGKILL blobfish at a random moment, tell LazyFS to drop every byte that
// was not fsynced (what a power cut does to the OS page cache), remount,
// restart blobfish and verify every key against a ledger of what the server
// acknowledged:
//
//   - an acknowledged PUT must read back byte for byte
//   - an acknowledged DELETE must stay deleted
//   - a request in flight at the cut may land or not, but never partially
//   - no unknown keys, no orphan blob files, an empty tmp/
//
// It is meant to run inside the container built by ../run.sh.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	rounds         int
	workers        int
	keys           int
	minRun, maxRun time.Duration
	seed           uint64
	blobfish       string
	lazyfs         string
	lazyfsConfig   string
	mountDir       string
	rootDir        string
	faultFIFO      string
	doneFIFO       string
	addr           string
}

func main() {
	var c config
	flag.IntVar(&c.rounds, "rounds", 20, "power cuts to simulate")
	flag.IntVar(&c.workers, "workers", 8, "concurrent clients")
	flag.IntVar(&c.keys, "keys", 40, "distinct object keys")
	flag.DurationVar(&c.minRun, "min-run", time.Second, "shortest write phase before a cut")
	flag.DurationVar(&c.maxRun, "max-run", 4*time.Second, "longest write phase before a cut")
	flag.Uint64Var(&c.seed, "seed", uint64(time.Now().UnixNano()), "random seed (printed, for reproducing)")
	flag.StringVar(&c.blobfish, "blobfish", "/usr/local/bin/blobfish", "blobfish binary under test")
	flag.StringVar(&c.lazyfs, "lazyfs", "/opt/lazyfs/lazyfs/build/lazyfs", "LazyFS binary")
	flag.StringVar(&c.lazyfsConfig, "lazyfs-config", "/etc/powercut/lazyfs.toml", "LazyFS config")
	flag.StringVar(&c.mountDir, "mount", "/mnt/lfs", "LazyFS mount point")
	flag.StringVar(&c.rootDir, "root", "/var/lib/lfs-root", "LazyFS backing directory")
	flag.StringVar(&c.faultFIFO, "fifo", "/tmp/faults.fifo", "LazyFS fault FIFO (must match config)")
	flag.StringVar(&c.doneFIFO, "done-fifo", "/tmp/faults_done.fifo", "LazyFS completion FIFO (must match config)")
	flag.StringVar(&c.addr, "addr", "127.0.0.1:9000", "blobfish listen address")
	flag.Parse()

	log.SetFlags(log.Ltime)
	h := &harness{cfg: c, rng: rand.New(rand.NewPCG(c.seed, 0x9e3779b97f4a7c15)), http: &http.Client{Timeout: 60 * time.Second}}
	log.Printf("seed=%d rounds=%d workers=%d keys=%d", c.seed, c.rounds, c.workers, c.keys)
	err := h.run()
	h.cleanup()
	if errors.Is(err, errToolCrash) {
		log.Printf("%v", err)
		os.Exit(2)
	}
	if err != nil {
		log.Printf("FAIL: %v", err)
		os.Exit(1)
	}
	log.Printf("PASS: %d power cuts, %d acknowledged writes and deletes, %d in-flight requests resolved (%d landed, %d rolled back), 0 lost or corrupt",
		c.rounds, h.stats.acked, h.stats.landed+h.stats.rolledBack, h.stats.landed, h.stats.rolledBack)
}

// objState is what one key holds: nothing, or content with this SHA-256.
type objState struct {
	present bool
	sha     string
}

func (s objState) String() string {
	if !s.present {
		return "absent"
	}
	return "sha:" + s.sha[:12]
}

// keyLedger tracks one key. Workers hold mu for a whole request, so at most
// one request per key is ever in flight and pending is unambiguous.
type keyLedger struct {
	name    string
	mu      sync.Mutex
	acked   objState  // last state the server acknowledged
	pending *objState // request sent but not acknowledged when the cut hit
}

type harness struct {
	cfg    config
	rng    *rand.Rand
	http   *http.Client
	token  string
	ledger []*keyLedger

	lazyfsCmd    *exec.Cmd
	lazyfsExited chan struct{}
	lazyfsState  atomic.Pointer[os.ProcessState]
	blobfishCmd  *exec.Cmd
	blobfishLog  *os.File
	starts       int

	stats struct {
		acked, landed, rolledBack int
	}
	// unexpected counts responses that were neither success nor caused by
	// the cut (e.g. 507 or 500 during normal running). Any of them fails
	// the run: the test must exercise real writes, not refusals.
	unexpectedMu sync.Mutex
	unexpected   map[int]int
}

func (h *harness) run() error {
	for _, d := range []string{h.cfg.mountDir, h.cfg.rootDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	h.ledger = make([]*keyLedger, h.cfg.keys)
	for i := range h.ledger {
		h.ledger[i] = &keyLedger{name: fmt.Sprintf("dir-%d/obj-%03d.bin", i%4, i)}
	}

	if err := h.mount(); err != nil {
		return err
	}
	if err := h.startBlobfish(); err != nil {
		return err
	}
	if err := h.api("PUT", "/v1/buckets/powercut", nil, 201); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}

	for round := 1; round <= h.cfg.rounds; round++ {
		run := h.cfg.minRun + time.Duration(h.rng.Int64N(int64(h.cfg.maxRun-h.cfg.minRun)+1))
		acked, workersDone := h.workload(run, round)
		if err := h.lazyfsAlive(); err != nil {
			return err
		}

		// The cut: the process dies first, so nothing writes after it, then
		// every byte not yet fsynced is dropped, as losing power would.
		if err := h.killBlobfish(); err != nil {
			return err
		}
		workersDone() // their in-flight requests fail now; ledger is stable after this
		if err := h.clearCache(); err != nil {
			return err
		}
		// Remount so the kernel's FUSE page cache can't serve stale bytes.
		if err := h.unmount(); err != nil {
			return err
		}
		if err := h.mount(); err != nil {
			return err
		}
		if err := h.startBlobfish(); err != nil {
			return fmt.Errorf("round %d: restart after cut: %w", round, err)
		}

		h.unexpectedMu.Lock()
		bad := fmt.Sprint(h.unexpected)
		h.unexpectedMu.Unlock()
		if bad != "map[]" {
			return fmt.Errorf("round %d: requests failed with unexpected statuses before the cut (status:count) %s; see /tmp/blobfish-*.log", round, bad)
		}

		pendingBefore := h.countPending()
		if err := h.verify(); err != nil {
			return fmt.Errorf("round %d (ran %s): %w", round, run.Round(time.Millisecond), err)
		}
		log.Printf("round %2d: ran %-6s acked %4d, in flight at cut %d -> verified",
			round, run.Round(time.Millisecond), acked, pendingBefore)
	}
	return nil
}

func (h *harness) countPending() int {
	n := 0
	for _, k := range h.ledger {
		if k.pending != nil {
			n++
		}
	}
	return n
}

// ---------- LazyFS ----------

func (h *harness) mount() error {
	// -s: single-threaded FUSE. LazyFS 0.3.1 segfaults in libpcache under
	// concurrent operations; its own test scripts use -s. Clients and the
	// server stay concurrent; only the file system serializes.
	cmd := exec.Command(h.cfg.lazyfs, h.cfg.mountDir,
		"--config-path", h.cfg.lazyfsConfig,
		"-o", "allow_other", "-o", "modules=subdir", "-o", "subdir="+h.cfg.rootDir, "-f", "-s")
	logf, err := os.OpenFile("/tmp/lazyfs.out", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start lazyfs: %w", err)
	}
	h.lazyfsCmd = cmd
	exited := make(chan struct{})
	h.lazyfsExited = exited
	go func() {
		state, _ := cmd.Process.Wait()
		h.lazyfsState.Store(state)
		close(exited)
	}()
	return waitFor(10*time.Second, "lazyfs mount", func() bool { return isMounted(h.cfg.mountDir) })
}

func (h *harness) unmount() error {
	if out, err := exec.Command("fusermount3", "-u", h.cfg.mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("unmount: %v: %s", err, out)
	}
	// LazyFS 0.3.1 aborts on exit ("terminate called without an active
	// exception": a fault-worker thread is never joined). That happens after
	// the unmount completes, so only the unmount itself is checked.
	select {
	case <-h.lazyfsExited:
	case <-time.After(30 * time.Second):
		return errors.New("lazyfs did not exit after unmount")
	}
	h.lazyfsCmd = nil
	if isMounted(h.cfg.mountDir) {
		return errors.New("still mounted after fusermount3 -u")
	}
	return nil
}

// errToolCrash marks a failure of LazyFS itself. The run says nothing about
// BlobFish then: a crashing tool may have written anything to disk.
var errToolCrash = errors.New("INCONCLUSIVE: LazyFS crashed; this run proves nothing either way")

func (h *harness) lazyfsAlive() error {
	select {
	case <-h.lazyfsExited:
		return fmt.Errorf("%w (%v; see /tmp/lazyfs.out)", errToolCrash, h.lazyfsState.Load())
	default:
		return nil
	}
}

// clearCache asks LazyFS to drop all unsynced data and waits until it has.
func (h *harness) clearCache() error {
	if err := h.lazyfsAlive(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		f, err := os.Open(h.cfg.doneFIFO) // blocks until LazyFS opens it to reply
		if err != nil {
			done <- err
			return
		}
		defer f.Close()
		line, err := bufio.NewReader(f).ReadString('\n')
		if err != nil && line == "" {
			done <- fmt.Errorf("read completion fifo: %w", err)
			return
		}
		if !strings.Contains(line, "finished::clear-cache") {
			done <- fmt.Errorf("unexpected completion message %q", line)
			return
		}
		done <- nil
	}()
	var f *os.File
	err := waitFor(10*time.Second, "lazyfs fault fifo reader", func() bool {
		var err error
		f, err = os.OpenFile(h.cfg.faultFIFO, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		return err == nil
	})
	if err != nil {
		if aerr := h.lazyfsAlive(); aerr != nil {
			return aerr
		}
		return err
	}
	_, err = f.WriteString("lazyfs::clear-cache\n")
	f.Close()
	if err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-h.lazyfsExited:
		return h.lazyfsAlive()
	case <-time.After(30 * time.Second):
		return errors.New("lazyfs did not confirm clear-cache")
	}
}

func isMounted(dir string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 2 && f[1] == dir && strings.HasPrefix(f[2], "fuse") {
			return true
		}
	}
	return false
}

// ---------- blobfish ----------

var tokenRE = regexp.MustCompile(`bf_[A-Za-z0-9_-]{20,}`)

func (h *harness) startBlobfish() error {
	h.starts++
	logPath := fmt.Sprintf("/tmp/blobfish-%02d.log", h.starts)
	logf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(h.cfg.blobfish)
	cmd.Env = append(os.Environ(),
		"BLOBFISH_LISTEN="+h.cfg.addr,
		"BLOBFISH_DATA_DIR="+filepath.Join(h.cfg.mountDir, "data"),
		"BLOBFISH_LOG_LEVEL=warn",
	)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // one stream: stdout and stderr share the pipe
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start blobfish: %w", err)
	}
	h.blobfishCmd, h.blobfishLog = cmd, logf
	tokenCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			if t := tokenRE.FindString(line); t != "" {
				tokenCh <- t
				line = tokenRE.ReplaceAllString(line, "bf_<redacted>")
			}
			fmt.Fprintln(logf, line)
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(logf, "log reader:", err)
		}
	}()

	err = waitFor(20*time.Second, "blobfish /healthz", func() bool {
		resp, err := h.http.Get("http://" + h.cfg.addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	if err != nil {
		return fmt.Errorf("%w (see %s)", err, logPath)
	}
	if h.token == "" {
		// The first start writes the admin key to <data dir>/initial-admin-key
		// (older builds printed it; accept either).
		select {
		case h.token = <-tokenCh:
		case <-time.After(500 * time.Millisecond):
			b, err := os.ReadFile(filepath.Join(h.cfg.mountDir, "data", "initial-admin-key"))
			if err != nil {
				return fmt.Errorf("no bootstrap admin key printed or saved: %w", err)
			}
			h.token = strings.TrimSpace(string(b))
		}
	}
	return nil
}

func (h *harness) killBlobfish() error {
	if err := h.blobfishCmd.Process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill blobfish: %w", err)
	}
	_ = h.blobfishCmd.Wait() // "signal: killed" is expected
	h.blobfishLog.Close()
	h.blobfishCmd = nil
	return nil
}

func (h *harness) cleanup() {
	if h.blobfishCmd != nil {
		_ = h.blobfishCmd.Process.Kill()
		_ = h.blobfishCmd.Wait()
	}
	if h.lazyfsCmd != nil {
		_ = exec.Command("fusermount3", "-uz", h.cfg.mountDir).Run()
		select {
		case <-h.lazyfsExited:
		case <-time.After(5 * time.Second):
			_ = h.lazyfsCmd.Process.Kill()
		}
	}
}

// ---------- workload ----------

// workload runs clients until the deadline and returns without waiting for
// requests still in flight, so the cut lands mid-request. Call the returned
// func after the cut to wait for the clients to give up; their unfinished
// requests stay pending in the ledger.
func (h *harness) workload(d time.Duration, round int) (int, func()) {
	deadline := time.Now().Add(d)
	var acked atomic.Int64
	var wg sync.WaitGroup
	for w := range h.cfg.workers {
		rng := rand.New(rand.NewPCG(h.cfg.seed+uint64(round)*1000+uint64(w), 1))
		wg.Go(func() {
			for time.Now().Before(deadline) {
				k := h.ledger[rng.IntN(len(h.ledger))]
				if !k.mu.TryLock() {
					continue // another client owns this key right now
				}
				if h.oneOp(rng, k) {
					acked.Add(1)
				}
				k.mu.Unlock()
			}
		})
	}
	time.Sleep(time.Until(deadline))
	return int(acked.Load()), func() {
		wg.Wait()
		h.stats.acked += int(acked.Load()) // includes requests acked just before the cut
	}
}

// oneOp does one PUT or DELETE on k (whose lock the caller holds) and
// reports whether the server acknowledged it.
func (h *harness) oneOp(rng *rand.Rand, k *keyLedger) bool {
	key := k.name
	if rng.IntN(5) == 0 {
		next := objState{present: false}
		k.pending = &next
		req, _ := http.NewRequest("DELETE", h.objectURL(key), nil)
		resp, err := h.do(req)
		if err == nil && resp.StatusCode == 204 {
			k.acked, k.pending = next, nil
			return true
		}
		if err == nil {
			h.noteUnexpected(resp.StatusCode)
		}
		return false
	}

	size := int64(rng.IntN(1 << 20))
	if rng.IntN(10) == 0 {
		size = 4<<20 + int64(rng.IntN(4<<20)) // some multi-MB bodies widen the window
	}
	seed := rng.Uint64()
	sum := sha256.New()
	io.Copy(sum, io.LimitReader(newPayload(seed), size))
	next := objState{present: true, sha: hex.EncodeToString(sum.Sum(nil))}
	k.pending = &next

	req, _ := http.NewRequest("PUT", h.objectURL(key), io.LimitReader(newPayload(seed), size))
	req.ContentLength = size
	resp, err := h.do(req)
	if err == nil && resp.StatusCode == 200 {
		k.acked, k.pending = next, nil
		return true
	}
	if err == nil {
		h.noteUnexpected(resp.StatusCode)
	}
	return false
}

func (h *harness) noteUnexpected(status int) {
	h.unexpectedMu.Lock()
	defer h.unexpectedMu.Unlock()
	if h.unexpected == nil {
		h.unexpected = map[int]int{}
	}
	h.unexpected[status]++
}

func (h *harness) objectURL(key string) string {
	return "http://" + h.cfg.addr + "/v1/b/powercut/" + key
}

func (h *harness) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, nil
}

func (h *harness) api(method, path string, body io.Reader, want int) error {
	req, _ := http.NewRequest(method, "http://"+h.cfg.addr+path, body)
	resp, err := h.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s: status %d, want %d", method, path, resp.StatusCode, want)
	}
	return nil
}

// payload is a deterministic byte stream for a seed.
func newPayload(seed uint64) io.Reader {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	return rand.NewChaCha8(s)
}

// ---------- verification ----------

func (h *harness) verify() error {
	var problems []string
	for i, k := range h.ledger {
		key := k.name
		observed, err := h.observe(key)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v (acked %s)", key, err, k.acked))
			continue
		}
		allowed := []objState{k.acked}
		if k.pending != nil {
			allowed = append(allowed, *k.pending)
		}
		ok := false
		for _, a := range allowed {
			if a == observed {
				ok = true
			}
		}
		if !ok {
			msg := fmt.Sprintf("%s: found %s, acknowledged state was %s", key, observed, k.acked)
			if k.pending != nil {
				msg += fmt.Sprintf(" (in flight: %s)", *k.pending)
			}
			problems = append(problems, msg)
			continue
		}
		if k.pending != nil {
			if observed == *k.pending && observed != k.acked {
				h.stats.landed++
			} else {
				h.stats.rolledBack++
			}
		}
		h.ledger[i].acked, h.ledger[i].pending = observed, nil
	}

	// No keys the clients never wrote, and nothing left behind on disk.
	listed, err := h.listAll()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	present := 0
	for _, k := range h.ledger {
		known[k.name] = true
		if k.acked.present {
			present++
		}
	}
	for _, key := range listed {
		if !known[key] {
			problems = append(problems, "unknown key listed: "+key)
		}
	}
	if len(listed) != present {
		problems = append(problems, fmt.Sprintf("listing has %d objects, ledger expects %d", len(listed), present))
	}
	blobs, err := countFiles(filepath.Join(h.cfg.mountDir, "data", "objects"))
	if err != nil {
		return err
	}
	if blobs != present {
		problems = append(problems, fmt.Sprintf("%d blob files on disk for %d objects (orphans or missing blobs)", blobs, present))
	}
	if tmp, err := countFiles(filepath.Join(h.cfg.mountDir, "data", "tmp")); err != nil || tmp != 0 {
		problems = append(problems, fmt.Sprintf("tmp/ has %d files after restart (err %v)", tmp, err))
	}

	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

// observe reads key fully. A body that fails BlobFish's own checksum check
// arrives as a broken connection and is reported as corruption.
func (h *harness) observe(key string) (objState, error) {
	req, _ := http.NewRequest("GET", h.objectURL(key), nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return objState{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 404:
		return objState{present: false}, nil
	case 200:
		sum := sha256.New()
		if _, err := io.Copy(sum, resp.Body); err != nil {
			return objState{}, fmt.Errorf("CORRUPT: download aborted (checksum failure?): %w", err)
		}
		return objState{present: true, sha: hex.EncodeToString(sum.Sum(nil))}, nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return objState{}, fmt.Errorf("GET status %d: %s", resp.StatusCode, b)
	}
}

func (h *harness) listAll() ([]string, error) {
	var out []string
	cursor := ""
	for {
		req, _ := http.NewRequest("GET", "http://"+h.cfg.addr+"/v1/b/powercut?limit=1000&cursor="+cursor, nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := h.http.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Objects []struct {
				Key string `json:"key"`
			} `json:"objects"`
			NextCursor string `json:"nextCursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		for _, o := range page.Objects {
			out = append(out, o.Key)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func countFiles(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n, err
}

func waitFor(timeout time.Duration, what string, ok func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", what)
}
